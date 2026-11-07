// Package volatile is the volatile memory backend provider: an
// in-process, non-persisted key-value store. It survives loop reloads (the
// registry, not the Luau state, owns the map) but is lost on process exit.
// Use it for decaying private buffers that must outlive one loop instance
// but are not worth persisting.
package volatile

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"agentflow/internal/core/memory"
)

// Provider implements memory.BackendProvider for "volatile".
type Provider struct{}

func (Provider) Name() string { return "volatile" }

func (Provider) Features() []string {
	return []string{"kv", "prefix_scan", "ttl"}
}

func (Provider) Open(cfg map[string]any) (memory.BackendHandle, error) {
	return &Handle{
		tables:     map[string]map[string]*entry{},
		maxEntries: maxEntriesFromConfig(cfg),
	}, nil
}

// maxEntriesFromConfig reads the optional bound. A non-positive or missing
// value means unbounded, which is the historical behaviour; config validation
// refuses a store bound to a volatile backend with neither this nor a TTL.
func maxEntriesFromConfig(cfg map[string]any) int {
	switch v := cfg["max_entries"].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case uint64:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}

// orderRef names one insertion in the FIFO order max_entries evicts from. A
// ref may be stale (the key was deleted or overwritten); eviction skips those.
type orderRef struct{ table, key string }

// entry is one stored value with its write time and optional expiry. The
// value is stored both as the raw Go any and its JSON bytes: Put marshals so
// Get/Query return JSON-normalized values (int->float64, etc.), matching the
// sqlite backend's semantics and keeping backends substitutable.
type entry struct {
	raw     []byte
	value   any
	updated time.Time
	expires time.Time // zero = no expiry
}

// Handle is an opened volatile backend. All access is mutex-guarded; Query
// snapshots matching keys under the lock so iteration is lock-free.
type Handle struct {
	mu     sync.Mutex
	tables map[string]map[string]*entry
	// maxEntries bounds the whole backend (not one table): it is a process
	// memory ceiling, which is what a volatile store needs. 0 = unbounded.
	maxEntries int
	// order is the insertion order evictions take from, oldest first.
	order []orderRef
	// count is the number of live entries across every table. order may hold
	// stale refs, so it is not usable as the count.
	count int
}

func (h *Handle) Close() error { return nil }

func (h *Handle) Put(table, key string, value any, opts memory.PutOpts) error {
	if table == "" {
		return fmt.Errorf("volatile: empty table")
	}
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("volatile: marshal value: %w", err)
	}
	var norm any
	if err := json.Unmarshal(b, &norm); err != nil {
		return fmt.Errorf("volatile: normalize value: %w", err)
	}
	now := time.Now()
	e := &entry{raw: b, value: norm, updated: now}
	if opts.TTL > 0 {
		e.expires = now.Add(opts.TTL)
	}
	h.mu.Lock()
	t, ok := h.tables[table]
	if !ok {
		t = map[string]*entry{}
		h.tables[table] = t
	}
	if _, exists := t[key]; !exists {
		h.order = append(h.order, orderRef{table: table, key: key})
		h.count++
	}
	t[key] = e
	h.evictLocked()
	h.mu.Unlock()
	return nil
}

// evictLocked drops the oldest entries until the backend is within
// maxEntries. Caller holds the lock. Stale order refs — a key deleted or
// expired since it was inserted — are skipped without touching count.
func (h *Handle) evictLocked() {
	if h.maxEntries <= 0 {
		return
	}
	for h.count > h.maxEntries && len(h.order) > 0 {
		ref := h.order[0]
		h.order = h.order[1:]
		t := h.tables[ref.table]
		if t == nil {
			continue
		}
		if _, ok := t[ref.key]; !ok {
			continue
		}
		delete(t, ref.key)
		h.count--
	}
}

func (h *Handle) Get(table, key string) (any, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.tables[table]
	if !ok {
		return nil, false, nil
	}
	e, ok := t[key]
	if !ok {
		return nil, false, nil
	}
	if !e.expires.IsZero() && time.Now().After(e.expires) {
		delete(t, key)
		h.count--
		return nil, false, nil
	}
	return e.value, true, nil
}

func (h *Handle) Delete(table, key string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t, ok := h.tables[table]; ok {
		if _, exists := t[key]; exists {
			delete(t, key)
			h.count--
		}
	}
	return nil
}

func (h *Handle) Query(table string, q memory.Query) (memory.Iterator, error) {
	switch q.Kind {
	case "prefix":
		return h.queryPrefix(table, q.Prefix)
	case "all":
		return h.queryAll(table)
	case "time_range":
		return h.queryTimeRange(table, q.From, q.To)
	case "text":
		return nil, fmt.Errorf("text queries are not supported by volatile; use a text-capable backend")
	case "vector":
		return nil, fmt.Errorf("vector queries are not supported by volatile; configure a vector-capable backend")
	default:
		return nil, fmt.Errorf("unsupported query kind %q", q.Kind)
	}
}

// snapshot collects live (non-expired) entries for a table under the lock,
// purging any expired ones it touches. It returns them newest-first.
func (h *Handle) snapshot(table string, filter func(key string, e *entry) bool) []memory.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.tables[table]
	if !ok {
		return nil
	}
	now := time.Now()
	var recs []memory.Record
	for k, e := range t {
		if !e.expires.IsZero() && now.After(e.expires) {
			delete(t, k)
			h.count--
			continue
		}
		if filter != nil && !filter(k, e) {
			continue
		}
		recs = append(recs, memory.Record{Key: k, Value: e.value})
	}
	// Newest-first by write time. Stable enough for recency windows; ties keep
	// no meaningful order (volatile buffers don't need a strict tiebreak).
	sortDesc(recs, t)
	return recs
}

func (h *Handle) queryAll(table string) (memory.Iterator, error) {
	return &sliceIter{recs: h.snapshot(table, nil)}, nil
}

func (h *Handle) queryPrefix(table, prefix string) (memory.Iterator, error) {
	return &sliceIter{recs: h.snapshot(table, func(key string, _ *entry) bool {
		return strings.HasPrefix(key, prefix)
	})}, nil
}

func (h *Handle) queryTimeRange(table string, from, to time.Time) (memory.Iterator, error) {
	return &sliceIter{recs: h.snapshot(table, func(_ string, e *entry) bool {
		return !e.updated.Before(from) && !e.updated.After(to)
	})}, nil
}

// GC drops expired records and, if window > 0, keeps only the most recent
// `window` rows. The manager calls this on its periodic ticker.
func (h *Handle) GC(table string, window int) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.tables[table]
	if !ok {
		return nil
	}
	now := time.Now()
	for k, e := range t {
		if !e.expires.IsZero() && now.After(e.expires) {
			delete(t, k)
			h.count--
		}
	}
	if window <= 0 {
		return nil
	}
	if len(t) <= window {
		return nil
	}
	// Collect keys by recency, drop the oldest beyond the window.
	type ke struct {
		k string
		u time.Time
	}
	all := make([]ke, 0, len(t))
	for k, e := range t {
		all = append(all, ke{k, e.updated})
	}
	// Partial selection sort: move the newest `window` to the front, drop the rest.
	for i := 0; i < window && i < len(all); i++ {
		max := i
		for j := i + 1; j < len(all); j++ {
			if all[j].u.After(all[max].u) {
				max = j
			}
		}
		all[i], all[max] = all[max], all[i]
	}
	for i := window; i < len(all); i++ {
		if _, ok := t[all[i].k]; ok {
			delete(t, all[i].k)
			h.count--
		}
	}
	return nil
}

// sortDesc orders recs newest-first by each record's entry.updated. The map
// is read-only here; the lock is already held.
func sortDesc(recs []memory.Record, t map[string]*entry) {
	for i := 0; i < len(recs); i++ {
		max := i
		ei, _ := t[recs[i].Key]
		for j := i + 1; j < len(recs); j++ {
			ej, _ := t[recs[j].Key]
			if ej.updated.After(ei.updated) {
				max = j
				ei = ej
			}
		}
		recs[i], recs[max] = recs[max], recs[i]
	}
}

// sliceIter serves a precomputed, snapshotted record slice.
type sliceIter struct {
	recs []memory.Record
	i    int
}

func (it *sliceIter) Next() bool {
	if it.i >= len(it.recs) {
		return false
	}
	it.i++
	return true
}

func (it *sliceIter) Record() memory.Record {
	return it.recs[it.i-1]
}

func (it *sliceIter) Err() error { return nil }
