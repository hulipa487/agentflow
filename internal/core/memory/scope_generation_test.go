// External test package: the wrapper is exercised over a real driver.
package memory_test

import (
	"testing"

	"agentflow/internal/core/memory"
)

func wrapGen(h memory.BackendHandle, user string, gen int64, loc memory.Locality, mode memory.ScopeMode) memory.BackendHandle {
	return memory.WrapScoped(h, "user", mode,
		memory.Caller{ScopeUUID: user, PersonalUUID: user, Locality: loc, Generation: gen},
		memory.PoolScope{})
}

// TestInstanceLocalityIsKeyedByGeneration: when a session moves to another
// instance and comes back, the instance-local store must not hand it the rows
// its earlier life wrote. The generation rides on the scope head, so the
// isolation filter still recognizes the stratum.
func TestInstanceLocalityIsKeyedByGeneration(t *testing.T) {
	raw := openOne(t)
	old := wrapGen(raw, "u1", 1, memory.LocalityInstance, memory.ModeInteractive)
	fresh := wrapGen(raw, "u1", 2, memory.LocalityInstance, memory.ModeInteractive)

	mustPut(t, old, "only-old", "stale")
	if _, ok := mustGet(t, fresh, "only-old"); ok {
		t.Fatal("a new generation read data from the previous one")
	}
	mustPut(t, fresh, "turn:1", "new")
	if v, ok := mustGet(t, fresh, "turn:1"); !ok || v != "new" {
		t.Fatalf("fresh generation read %q ok=%v", v, ok)
	}
	if v, ok := mustGet(t, old, "turn:1"); ok {
		t.Fatalf("the old generation read the new generation's row: %q", v)
	}

	// A prefix scan is the path that would leak if the generation head parsed
	// as legacy: it would be admitted by the filter and returned.
	keys := queryKeys(t, fresh, memory.Query{Kind: "prefix"})
	for _, k := range keys {
		if k == "only-old" {
			t.Fatalf("prefix scan surfaced a previous generation's key: %v", keys)
		}
	}
}

// TestServiceStratumIsKeyedByGeneration: engine work with no user stamp also
// writes per-session state on an instance-local store, and that state is just
// as stale after a move.
func TestServiceStratumIsKeyedByGeneration(t *testing.T) {
	raw := openOne(t)
	old := wrapGen(raw, "", 1, memory.LocalityInstance, memory.ModeService)
	fresh := wrapGen(raw, "", 2, memory.LocalityInstance, memory.ModeService)
	mustPut(t, old, "cursor", "old")
	if _, ok := mustGet(t, fresh, "cursor"); ok {
		t.Fatal("service stratum leaked across generations")
	}
	mustPut(t, fresh, "cursor", "new")
	if v, ok := mustGet(t, fresh, "cursor"); !ok || v != "new" {
		t.Fatalf("fresh service row = %q ok=%v", v, ok)
	}
}

// TestSharedLocalityIgnoresGeneration: a deployment- or region-wide store is
// shared by every instance that can own the session, so it must not be
// partitioned by a generation it was never moved between.
func TestSharedLocalityIgnoresGeneration(t *testing.T) {
	raw := openOne(t)
	old := wrapGen(raw, "u1", 1, memory.LocalityDeployment, memory.ModeInteractive)
	fresh := wrapGen(raw, "u1", 2, memory.LocalityDeployment, memory.ModeInteractive)
	mustPut(t, old, "turn:1", "shared")
	if v, ok := mustGet(t, fresh, "turn:1"); !ok || v != "shared" {
		t.Fatalf("a shared store was partitioned by generation: %q ok=%v", v, ok)
	}
}
