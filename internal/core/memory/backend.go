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

// Store describes a logical store binding (backend name + table/collection).
type Store struct {
	Backend    string
	Table      string
	Collection string
	Window     int
	Retention  time.Duration
}

// StoreBinding is a resolved (backend, table) pair.
type StoreBinding struct {
	Backend   string
	Table     string
	Window    int
	Retention time.Duration
}

// AgentMemory holds the resolved memory profile for an agent.
type AgentMemory struct {
	Stores  map[string]StoreBinding // by logical store name
	Tables  map[string]StoreBinding // by physical table name
	Write   []string
	Recall  string
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
		b := StoreBinding{Backend: s.Backend, Table: s.Table, Window: s.Window, Retention: s.Retention}
		out.Stores[sname] = b
		out.Tables[s.Table] = b
	}
	return out, nil
}
