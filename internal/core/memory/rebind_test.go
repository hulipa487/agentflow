package memory

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// fsOf is a fake feature lookup over a name->features table.
func fsOf(table map[string][]string) func(string) []string {
	return func(name string) []string { return table[name] }
}

func captureLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// TestRebindSkippedVectorToSQLite: a store on a credential-skipped pgvector
// backend rebinds to the surviving sqlite backend (which offers text_search),
// with requires degraded to [kv, text_search] — a vector store becoming an
// FTS store, exactly as the downstream generator used to do.
func TestRebindSkippedVectorToSQLite(t *testing.T) {
	log, buf := captureLog()
	stores := map[string]Store{
		"facts": {Backend: "vec", Table: "facts", Requires: []string{"vector"}, Retention: 0},
	}
	feats := fsOf(map[string][]string{
		"main_db": {"kv", "prefix_scan", "text_search", "ttl"},
		"vec":     {"vector"},
	})

	out, dropped := RebindSkipped(stores, map[string]bool{"vec": true}, []string{"main_db"}, feats, log)
	if len(dropped) != 0 {
		t.Fatalf("unexpected drops: %v", dropped)
	}
	got := out["facts"]
	if got.Backend != "main_db" {
		t.Fatalf("store not rebound: %+v", got)
	}
	if strings.Join(got.Requires, ",") != "kv,text_search" {
		t.Fatalf("requires not degraded: %v", got.Requires)
	}
	if got.Table != "facts" {
		t.Fatalf("table must be preserved: %+v", got)
	}
	if !strings.Contains(buf.String(), "rebound") {
		t.Fatalf("rebind not logged: %s", buf.String())
	}
	// The fallback must satisfy the degraded requires, or the boot still dies.
	reg := NewRegistry(log)
	reg.RegisterProvider(featureProvider{name: "sqlite", features: feats("main_db")})
	reg.AddBackend("main_db", "sqlite", nil)
	if err := reg.Open(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.ResolveStoresFor("bot", out); err != nil {
		t.Fatalf("rebound profile must resolve: %v", err)
	}
}

// TestRebindSkippedFallbackWithoutTextSearch: a kv-only survivor degrades
// requires to [kv] rather than claiming text_search it does not have.
func TestRebindSkippedFallbackWithoutTextSearch(t *testing.T) {
	log, _ := captureLog()
	stores := map[string]Store{
		"notes": {Backend: "vec", Table: "notes", Requires: []string{"vector"}},
	}
	feats := fsOf(map[string][]string{"cache": {"kv", "prefix_scan", "ttl"}})

	out, dropped := RebindSkipped(stores, map[string]bool{"vec": true}, []string{"cache"}, feats, log)
	if len(dropped) != 0 {
		t.Fatalf("unexpected drops: %v", dropped)
	}
	if got := out["notes"]; got.Backend != "cache" || strings.Join(got.Requires, ",") != "kv" {
		t.Fatalf("kv-only fallback wrong: %+v", got)
	}
}

// TestRebindSkippedPrefersTextSearchSurvivor: with several survivors the
// text_search-capable one wins regardless of position.
func TestRebindSkippedPrefersTextSearchSurvivor(t *testing.T) {
	log, _ := captureLog()
	stores := map[string]Store{"kb": {Backend: "vec", Table: "kb", Requires: []string{"vector"}}}
	feats := fsOf(map[string][]string{
		"cache":   {"kv", "ttl"},
		"main_db": {"kv", "text_search"},
	})

	out, _ := RebindSkipped(stores, map[string]bool{"vec": true}, []string{"cache", "main_db"}, feats, log)
	if got := out["kb"]; got.Backend != "main_db" {
		t.Fatalf("must prefer the text_search survivor: %+v", got)
	}
}

// TestRebindSkippedNoSurvivorDropsStore: with nothing left to fall back to the
// store is dropped (logged), so the agent's memory resolves without it.
func TestRebindSkippedNoSurvivorDropsStore(t *testing.T) {
	log, buf := captureLog()
	stores := map[string]Store{
		"facts":    {Backend: "vec", Table: "facts", Requires: []string{"vector"}},
		"dialogue": {Backend: "vec2", Table: "dialogue"},
	}
	out, dropped := RebindSkipped(stores, map[string]bool{"vec": true, "vec2": true}, nil, fsOf(nil), log)
	if len(out) != 0 {
		t.Fatalf("all stores must be dropped: %+v", out)
	}
	if len(dropped) != 2 {
		t.Fatalf("dropped = %v, want both stores", dropped)
	}
	if !strings.Contains(buf.String(), "dropped") {
		t.Fatalf("drop not logged: %s", buf.String())
	}

	// The surviving agent memory resolves to an empty store set, not an error.
	reg := NewRegistry(log)
	if _, err := reg.ResolveStoresFor("bot", out); err != nil {
		t.Fatalf("empty profile must resolve: %v", err)
	}
}

// TestRebindSkippedNoSkipsUntouched: with nothing skipped the input map is
// returned as-is (same map, no copy, no logs) — the no-op path.
func TestRebindSkippedNoSkipsUntouched(t *testing.T) {
	log, buf := captureLog()
	stores := map[string]Store{"dialogue": {Backend: "main_db", Table: "dialogue"}}
	out, dropped := RebindSkipped(stores, nil, []string{"main_db"}, fsOf(map[string][]string{"main_db": {"kv"}}), log)
	if dropped != nil {
		t.Fatalf("unexpected drops: %v", dropped)
	}
	if len(out) != 1 || out["dialogue"].Backend != "main_db" {
		t.Fatalf("profile must pass through untouched: %+v", out)
	}
	if buf.Len() != 0 {
		t.Fatalf("no-op path must not log: %s", buf.String())
	}
}

// TestRebindSkippedLeavesOtherBackends: only stores on skipped backends move;
// a healthy store keeps its backend, requires, and table.
func TestRebindSkippedLeavesOtherBackends(t *testing.T) {
	log, _ := captureLog()
	stores := map[string]Store{
		"dialogue": {Backend: "main_db", Table: "dialogue", Requires: []string{"kv", "text_search"}},
		"facts":    {Backend: "vec", Table: "facts", Requires: []string{"vector"}},
	}
	feats := fsOf(map[string][]string{"main_db": {"kv", "text_search"}})

	out, _ := RebindSkipped(stores, map[string]bool{"vec": true}, []string{"main_db"}, feats, log)
	if got := out["dialogue"]; got.Backend != "main_db" || strings.Join(got.Requires, ",") != "kv,text_search" {
		t.Fatalf("healthy store must be untouched: %+v", got)
	}
	if got := out["facts"]; got.Backend != "main_db" {
		t.Fatalf("skipped store must rebind: %+v", got)
	}
}

// featureProvider is a minimal BackendProvider for the resolution assertions.
type featureProvider struct {
	name     string
	features []string
}

func (p featureProvider) Name() string       { return p.name }
func (p featureProvider) Features() []string { return p.features }
func (p featureProvider) Open(config map[string]any) (BackendHandle, error) {
	return noopHandle{}, nil
}

type noopHandle struct{}

func (noopHandle) Put(string, string, any, PutOpts) error { return nil }
func (noopHandle) Get(string, string) (any, bool, error)  { return nil, false, nil }
func (noopHandle) Delete(string, string) error            { return nil }
func (noopHandle) Query(string, Query) (Iterator, error)  { return EmptyIterator{}, nil }
func (noopHandle) GC(string, int) error                   { return nil }
func (noopHandle) Close() error                           { return nil }
