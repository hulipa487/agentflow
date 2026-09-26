package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// memProvider is an in-memory backend keyed by (table, key), shared across
// agents the way one physical database is.
type memProvider struct {
	mu   sync.Mutex
	data map[string]map[string]any
}

func (p *memProvider) Name() string       { return "mem" }
func (p *memProvider) Features() []string { return []string{"kv"} }
func (p *memProvider) Open(config map[string]any) (BackendHandle, error) {
	if p.data == nil {
		p.data = map[string]map[string]any{}
	}
	return &memHandle{p: p}, nil
}

type memHandle struct{ p *memProvider }

func (h *memHandle) Put(table, key string, value any, opts PutOpts) error {
	h.p.mu.Lock()
	defer h.p.mu.Unlock()
	if h.p.data[table] == nil {
		h.p.data[table] = map[string]any{}
	}
	h.p.data[table][key] = value
	return nil
}
func (h *memHandle) Get(table, key string) (any, bool, error) {
	h.p.mu.Lock()
	defer h.p.mu.Unlock()
	v, ok := h.p.data[table][key]
	return v, ok, nil
}
func (h *memHandle) Delete(table, key string) error { return nil }
func (h *memHandle) Query(table string, q Query) (Iterator, error) {
	return EmptyIterator{}, nil
}
func (h *memHandle) GC(table string, window int) error { return nil }
func (h *memHandle) Close() error                      { return nil }

func openMemRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	reg.RegisterProvider(&memProvider{})
	reg.AddBackend("main_db", "mem", nil)
	if err := reg.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	return reg
}

func conversationalProfile() map[string]Store {
	return map[string]Store{
		"dialogue": {Backend: "main_db", Table: "dialogue", Window: 1000},
		"facts":    {Backend: "main_db", Table: "facts"},
	}
}

// TestPerAgentTableIsolation: two agents on the same conversational profile
// bind distinct physical tables — conversation history cannot leak across
// agents (previously both bound the shared "dialogue"/"facts" tables).
func TestPerAgentTableIsolation(t *testing.T) {
	reg := openMemRegistry(t)
	writer, err := reg.ResolveStoresFor("writer", conversationalProfile())
	if err != nil {
		t.Fatal(err)
	}
	librarian, err := reg.ResolveStoresFor("librarian", conversationalProfile())
	if err != nil {
		t.Fatal(err)
	}

	wb := writer.Tables["dialogue"]
	lb := librarian.Tables["dialogue"]
	if wb.Table != "writer.dialogue" || lb.Table != "librarian.dialogue" {
		t.Fatalf("bindings: writer=%q librarian=%q", wb.Table, lb.Table)
	}
	// The loop-facing key is unchanged: the profile's table name.
	if _, ok := writer.Tables["dialogue"]; !ok {
		t.Fatal("Tables must stay keyed by the profile table name")
	}

	h, _ := reg.Handle("main_db")
	if err := h.Put(wb.Table, "turn-1", "writer said this", PutOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := h.Get(lb.Table, "turn-1"); found {
		t.Fatal("librarian must not see the writer's turns")
	}
	if v, found, _ := h.Get(wb.Table, "turn-1"); !found || v != "writer said this" {
		t.Fatalf("writer lost its own turn: %v found=%v", v, found)
	}
}

// TestPoolStoreBindingResolvesByName: a pool-bound store keeps the agent-axis
// table prefix and resolves the pool's uuid into its scoping at bind time —
// the (pool, agent) half of the carve, known at bind. An unknown pool name is
// a bind error, which is what makes a misconfigured carve fail at boot.
func TestPoolStoreBindingResolvesByName(t *testing.T) {
	reg := openMemRegistry(t)
	resolved := ""
	reg.SetPoolResolver(func(name string) (string, error) {
		if name == "proj-x" {
			resolved = "pool-uuid-1"
			return resolved, nil
		}
		return "", fmt.Errorf("pool %q does not exist", name)
	})
	profile := map[string]Store{
		"kb": {Backend: "main_db", Table: "kb", Pool: "proj-x"},
	}
	a, err := reg.ResolveStoresFor("writer", profile)
	if err != nil {
		t.Fatal(err)
	}
	if a.Stores["kb"].Table != "writer.kb" {
		t.Fatalf("a pool store keeps the agent axis: %q", a.Stores["kb"].Table)
	}
	if a.Stores["kb"].Scoping != "pool:pool-uuid-1" || a.Stores["kb"].PoolUUID != "pool-uuid-1" {
		t.Fatalf("binding scoping = %q, uuid = %q; want the resolved pool", a.Stores["kb"].Scoping, a.Stores["kb"].PoolUUID)
	}

	// An unknown pool is a boot error, never a silent fallback.
	_, err = reg.ResolveStoresFor("writer", map[string]Store{
		"kb": {Backend: "main_db", Table: "kb", Pool: "no-such-pool"},
	})
	if err == nil || !strings.Contains(err.Error(), "no-such-pool") {
		t.Fatalf("unknown pool = %v; want a bind error naming it", err)
	}
	_ = resolved
}

// TestResolveStoresUnscoped: an empty agent (no identity) binds tables as-is —
// the pre-isolation behavior, kept for callers with no agent identity.
func TestResolveStoresUnscoped(t *testing.T) {
	reg := openMemRegistry(t)
	am, err := reg.ResolveStoresFor("", conversationalProfile())
	if err != nil {
		t.Fatal(err)
	}
	if am.Tables["dialogue"].Table != "dialogue" {
		t.Fatalf("unscoped resolve must not prefix: %q", am.Tables["dialogue"].Table)
	}
}
