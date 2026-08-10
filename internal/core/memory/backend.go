// Package memory defines the three-level memory model: provider → backend
// instance → store. A backend provider is a Go implementation that opens a
// handle; a store is a logical sink bound to one backend and one table.
package memory

import (
	"context"
	"fmt"
	"time"
)

// Record is a single memory item returned by query.
type Record struct {
	Key   string         `json:"key"`
	Value any            `json:"value"`
	Meta  map[string]any `json:"meta,omitempty"`
}

// PutOpts carries optional parameters for Put.
type PutOpts struct {
	TTL time.Duration
	// Vector, when set, is an embedding to store alongside the value.
	// Only vector-capable backends (feature "vector") honor it; callers
	// must gate on features before sending one.
	Vector []float32
}

// Query describes a backend query.
type Query struct {
	Kind   string         // "prefix" | "text" | "vector" | "time_range" | "all"
	Prefix string         // Kind == "prefix"
	Text   string         // Kind == "text"
	Vector []float32      // Kind == "vector"
	K      int            // Kind == "vector" / "text"
	From   time.Time      // Kind == "time_range"
	To     time.Time      // Kind == "time_range"
	Table  string         // optional logical table filter
}

// Iterator is a pull-style query result.
type Iterator interface {
	Next() bool
	Record() Record
	Err() error
}

// EmptyIterator is a finished iterator.
type EmptyIterator struct{}

func (EmptyIterator) Next() bool    { return false }
func (EmptyIterator) Record() Record { return Record{} }
func (EmptyIterator) Err() error    { return nil }

// BackendHandle is an opened backend instance.
type BackendHandle interface {
	Put(table, key string, value any, opts PutOpts) error
	Get(table, key string) (any, bool, error)
	Delete(table, key string) error
	Query(table string, q Query) (Iterator, error)
	GC(table string, window int) error
	Close() error
}

// BackendProvider is a factory for backend instances.
type BackendProvider interface {
	Name() string
	Features() []string
	Open(config map[string]any) (BackendHandle, error)
}

// Registry resolves provider names and opened backend instances.
type Registry struct {
	providers map[string]BackendProvider
	backends  map[string]BackendHandle
	config    map[string]BackendConfig
}

type BackendConfig struct {
	Provider string
	Config   map[string]any
}

func NewRegistry() *Registry {
	return &Registry{
		providers: map[string]BackendProvider{},
		backends:  map[string]BackendHandle{},
		config:    map[string]BackendConfig{},
	}
}

func (r *Registry) RegisterProvider(p BackendProvider) {
	r.providers[p.Name()] = p
}

func (r *Registry) AddBackend(name string, provider string, config map[string]any) {
	r.config[name] = BackendConfig{Provider: provider, Config: config}
}

// Open opens all configured backends.
func (r *Registry) Open(ctx context.Context) error {
	for name, c := range r.config {
		p, ok := r.providers[c.Provider]
		if !ok {
			return fmt.Errorf("unknown memory provider %q for backend %q", c.Provider, name)
		}
		h, err := p.Open(c.Config)
		if err != nil {
			return fmt.Errorf("open memory backend %q: %w", name, err)
		}
		r.backends[name] = h
	}
	return nil
}

// Close closes all opened backends.
func (r *Registry) Close() error {
	var first error
	for _, h := range r.backends {
		if err := h.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Handle returns a named backend handle.
func (r *Registry) Handle(name string) (BackendHandle, bool) {
	h, ok := r.backends[name]
	return h, ok
}

// Features reports the provider features of a named backend (nil when the
// backend or its provider is unknown).
func (r *Registry) Features(name string) []string {
	c, ok := r.config[name]
	if !ok {
		return nil
	}
	p, ok := r.providers[c.Provider]
	if !ok {
		return nil
	}
	return p.Features()
}

// Store describes a logical store binding (backend name + table/collection).
type Store struct {
	Backend    string
	Table      string
	Collection string
	Window     int
	Retention  time.Duration
	Requires   []string // provider features the backend must offer
}

// StoreBinding is a resolved (backend, table) pair.
type StoreBinding struct {
	Backend   string
	Table     string
	Window    int
	Retention time.Duration
	// Features are the backend provider's capabilities ("kv", "vector",
	// ...), resolved at bind time so loops can adapt (e.g. only embed for
	// vector-capable stores).
	Features []string
}

// AgentMemory holds the resolved memory profile for an agent.
type AgentMemory struct {
	Stores  map[string]StoreBinding // by logical store name
	Tables  map[string]StoreBinding // by physical table name
	Write   []string
	Recall  string
	// EmbedModel names the models: entry used to embed record text on
	// write (memory.write) and queries on semantic recall. RerankModel,
	// when set, reranks oversampled vector hits on semantic recall.
	// Oversample is the vector-fetch multiplier used before reranking.
	EmbedModel  string
	RerankModel string
	Oversample  int
}

// ResolveStore resolves a profile's store names into backend handles.
func (r *Registry) ResolveStores(profile map[string]Store) (AgentMemory, error) {
	out := AgentMemory{
		Stores: map[string]StoreBinding{},
		Tables: map[string]StoreBinding{},
	}
	for sname, s := range profile {
		_, ok := r.backends[s.Backend]
		if !ok {
			return AgentMemory{}, fmt.Errorf("memory store %q references unknown backend %q", sname, s.Backend)
		}
		if err := r.checkRequires(sname, s); err != nil {
			return AgentMemory{}, err
		}
		b := StoreBinding{Backend: s.Backend, Table: s.Table, Window: s.Window, Retention: s.Retention, Features: r.Features(s.Backend)}
		out.Stores[sname] = b
		out.Tables[s.Table] = b
	}
	return out, nil
}

// checkRequires fails loudly when a store declares features its backend does
// not provide, instead of degrading silently at query time.
func (r *Registry) checkRequires(sname string, s Store) error {
	if len(s.Requires) == 0 {
		return nil
	}
	have := map[string]bool{}
	for _, f := range r.Features(s.Backend) {
		have[f] = true
	}
	for _, req := range s.Requires {
		if !have[req] {
			return fmt.Errorf("memory store %q requires feature %q, which backend %q does not provide", sname, req, s.Backend)
		}
	}
	return nil
}
