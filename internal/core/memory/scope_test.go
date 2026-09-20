// External test package: the wrapper is exercised over a real driver, which
// in-package tests cannot import without a cycle.
package memory_test

import (
	"context"
	"testing"

	"agentflow/internal/core/memory"
	"agentflow/internal/drivers/volatile"
)

// openOne backs a test with ONE volatile store; tests wrap it locally as
// many sessions of however many users — every wrapper in a test shares the
// same physical store.
func openOne(t *testing.T) memory.BackendHandle {
	t.Helper()
	reg := memory.NewRegistry()
	reg.RegisterProvider(volatile.Provider{})
	reg.AddBackend("v", "volatile", nil)
	if err := reg.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	h, ok := reg.Handle("v")
	if !ok {
		t.Fatal("no handle")
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func wrap(h memory.BackendHandle, scoping string, mode memory.ScopeMode, user string) memory.BackendHandle {
	return memory.WrapScoped(h, scoping, mode, user)
}

func mustPut(t *testing.T, h memory.BackendHandle, key string, val string) {
	t.Helper()
	if err := h.Put("t", key, val, memory.PutOpts{}); err != nil {
		t.Fatal(err)
	}
}

func mustGet(t *testing.T, h memory.BackendHandle, key string) (string, bool) {
	t.Helper()
	v, ok, err := h.Get("t", key)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := v.(string)
	return s, ok
}

func queryKeys(t *testing.T, h memory.BackendHandle, q memory.Query) []string {
	t.Helper()
	it, err := h.Query("t", q)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for it.Next() {
		out = append(out, it.Record().Key)
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestInteractiveIsolation: the property oscar prioritized — one user's
// session can never read another user's rows, even with the same key.
func TestInteractiveIsolation(t *testing.T) {
	raw := openOne(t)
	a := wrap(raw, "user", memory.ModeInteractive, "u1")
	b := wrap(raw, "user", memory.ModeInteractive, "u2")

	mustPut(t, a, "turn:1", "A-secret")
	mustPut(t, b, "turn:1", "B-secret") // same key, no collision

	if v, ok := mustGet(t, a, "turn:1"); !ok || v != "A-secret" {
		t.Fatalf("A must read own row, got %q ok=%v", v, ok)
	}
	// Same key from B returns B's own row, never A's.
	if v, ok := mustGet(t, b, "turn:1"); !ok || v != "B-secret" {
		t.Fatalf("B must read its own row, not A's: got %q ok=%v", v, ok)
	}
	for _, k := range queryKeys(t, b, memory.Query{Kind: "prefix", Prefix: "turn:"}) {
		if k != "turn:1" {
			t.Fatalf("B's prefix query leaked A's key %q", k)
		}
	}
	// The raw handle shows both prefixed rows (storage shape proof).
	if keys := queryKeys(t, raw, memory.Query{Kind: "all"}); len(keys) != 2 {
		t.Fatalf("raw store holds %v, want 2 rows", keys)
	}
}

// TestServiceStratum: interactive sessions read service rows but never write
// them; service contexts write the service stratum, not a user stratum.
func TestServiceStratum(t *testing.T) {
	raw := openOne(t)
	user := wrap(raw, "user", memory.ModeInteractive, "u1")
	svc := wrap(raw, "user", memory.ModeService, "")

	mustPut(t, svc, "kb:faq", "fleet-wide")
	if v, ok := mustGet(t, user, "kb:faq"); !ok || v != "fleet-wide" {
		t.Fatalf("interactive must read service rows, got %q ok=%v", v, ok)
	}
	mustPut(t, user, "note", "mine")
	if _, ok := mustGet(t, svc, "note"); ok {
		t.Fatal("service context must not read user rows")
	}
	if _, ok := mustGet(t, raw, "service|note"); ok {
		t.Fatal("interactive write must not land in the service stratum")
	}
	if _, ok := mustGet(t, raw, "user:u1|note"); !ok {
		t.Fatal("interactive write must land in the user scope")
	}
}

// TestLegacyStratum: pre-upgrade rows (unprefixed) stay readable by everyone
// and queries return them with stripped keys; new writes never use legacy.
func TestLegacyStratum(t *testing.T) {
	raw := openOne(t)
	user := wrap(raw, "user", memory.ModeInteractive, "u1")

	mustPut(t, raw, "old:turn", "pre-upgrade") // direct raw write = legacy row
	if v, ok := mustGet(t, user, "old:turn"); !ok || v != "pre-upgrade" {
		t.Fatalf("legacy row must stay readable, got %q ok=%v", v, ok)
	}
	keys := queryKeys(t, user, memory.Query{Kind: "prefix", Prefix: "old:"})
	if len(keys) != 1 || keys[0] != "old:turn" {
		t.Fatalf("legacy key must appear stripped in queries: %v", keys)
	}
}

// TestMaintenanceMode: engine-fired contexts (the nightly distiller) read
// every scope with raw scope-visible keys, enumerate user scopes, and write
// scope-explicitly.
func TestMaintenanceMode(t *testing.T) {
	raw := openOne(t)
	a := wrap(raw, "user", memory.ModeInteractive, "u1")
	b := wrap(raw, "user", memory.ModeInteractive, "u2")
	maint := wrap(raw, "user", memory.ModeMaintenance, "")

	mustPut(t, a, "turn:1", "A")
	mustPut(t, b, "turn:2", "B")
	if err := maint.Put("t", "user:u1|deep", "distilled", memory.PutOpts{}); err != nil {
		t.Fatal(err)
	}

	got := queryKeys(t, maint, memory.Query{Kind: "all"})
	want := map[string]bool{"user:u1|turn:1": true, "user:u2|turn:2": true, "user:u1|deep": true}
	if len(got) != len(want) {
		t.Fatalf("maintenance read-all %v, want %v", got, want)
	}
	for _, k := range got {
		if !want[k] {
			t.Fatalf("maintenance saw unexpected key %q", k)
		}
	}
	// The distilled row is in u1's stratum: u1 reads it through the
	// interactive boundary, u2 does not.
	if v, ok := mustGet(t, a, "deep"); !ok || v != "distilled" {
		t.Fatalf("u1 must read its distilled row, got %q ok=%v", v, ok)
	}
	if _, ok := mustGet(t, b, "deep"); ok {
		t.Fatal("u2 must not read u1's distilled row")
	}
	sc, ok := maint.(memory.ScopeEnumerator)
	if !ok {
		t.Fatal("maintenance handle must implement ScopeEnumerator")
	}
	scopes, err := sc.Scopes("t")
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 2 || scopes[0] != "user:u1" || scopes[1] != "user:u2" {
		t.Fatalf("Scopes() = %v, want [user:u1 user:u2]", scopes)
	}
}

// TestAgentScoping: scope: agent keeps one pool for the whole agent (the
// pre-isolation opt-out), shared by every user session.
func TestAgentScoping(t *testing.T) {
	raw := openOne(t)
	a := wrap(raw, "agent", memory.ModeInteractive, "u1")
	b := wrap(raw, "agent", memory.ModeInteractive, "u2")

	mustPut(t, a, "pool", "shared-by-design")
	if v, ok := mustGet(t, b, "pool"); !ok || v != "shared-by-design" {
		t.Fatalf("agent scope must share one pool, got %q ok=%v", v, ok)
	}
}

// TestSharedUnwrapped: shared bindings are never wrapped — kb writes are raw
// and visible to service contexts, exactly as before isolation.
func TestSharedUnwrapped(t *testing.T) {
	raw := openOne(t)
	svc := wrap(raw, "user", memory.ModeService, "")
	shared := memory.WrapScoped(raw, "", memory.ModeInteractive, "u1")
	mustPut(t, shared, "kb:x", "v")
	if _, ok := mustGet(t, svc, "kb:x"); !ok {
		t.Fatal("shared store writes must be raw (visible to service contexts)")
	}
}

// TestModeOf: provenance kinds map to the maintenance/service/interactive
// classes; the provenance stamp is what unforgeably admits maintenance reads.
func TestModeOf(t *testing.T) {
	cases := []struct {
		kind, user string
		want       memory.ScopeMode
	}{
		{"system", "", memory.ModeMaintenance},
		{"scheduler", "", memory.ModeMaintenance},
		{"system", "u1", memory.ModeMaintenance}, // engine-fired stays maintenance
		{"", "u1", memory.ModeInteractive},
		{"agent", "u1", memory.ModeInteractive},
		{"agent", "", memory.ModeService},
		{"", "", memory.ModeService},
	}
	for _, c := range cases {
		if got := memory.ModeOf(c.kind, c.user); got != c.want {
			t.Errorf("ModeOf(%q, %q) = %v, want %v", c.kind, c.user, got, c.want)
		}
	}
}

// TestDeleteAcrossStrata: forget removes the key wherever it lives.
func TestDeleteAcrossStrata(t *testing.T) {
	raw := openOne(t)
	a := wrap(raw, "user", memory.ModeInteractive, "u1")
	svc := wrap(raw, "user", memory.ModeService, "")

	mustPut(t, a, "k", "user-copy")
	mustPut(t, svc, "k", "service-copy")
	mustPut(t, raw, "k", "legacy-copy")
	if err := a.Delete("t", "k"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"user:u1|k", "service|k", "k"} {
		if _, ok := mustGet(t, raw, k); ok {
			t.Fatalf("stratum copy %q must be deleted", k)
		}
	}
}
