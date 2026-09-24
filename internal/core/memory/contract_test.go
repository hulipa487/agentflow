package memory_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/core/memory"
	"agentflow/internal/drivers/mongodb"
	"agentflow/internal/drivers/pgvector"
	"agentflow/internal/drivers/postgres"
	"agentflow/internal/drivers/qdrant"
	"agentflow/internal/drivers/redis"
	"agentflow/internal/drivers/redisvector"
	"agentflow/internal/drivers/sqlite"
	"agentflow/internal/drivers/volatile"
)

// TestBackendContract runs one contract against every memory provider.
//
// There was no such test. Four of the eight providers — mongodb, postgres,
// pgvector and redis — had no tests at all, and the other four were covered by
// bespoke assertions that could not notice a provider answering a query the
// wrong way: a prefix scan that matches too much, a text search that finds
// nothing, a vector query that returns an empty list where it should say the
// backend cannot do it. Recall is built on these four operations, so a provider
// failing one silently degrades every loop that uses it.
//
// The server-backed providers sit behind the same env gates the rest of the
// suite uses, so a plain `go test ./...` exercises SQLite and the volatile store
// and the others run wherever a server is available.
func TestBackendContract(t *testing.T) {
	cases := []struct {
		name     string
		provider memory.BackendProvider
		cfg      func(t *testing.T) map[string]any
		gate     string // env var that must be set, or the case is skipped
	}{
		{
			name:     "sqlite",
			provider: sqlite.Provider{},
			cfg:      func(t *testing.T) map[string]any { return map[string]any{"path": filepath.Join(t.TempDir(), "mem.db")} },
		},
		{
			name:     "volatile",
			provider: volatile.Provider{},
			cfg:      func(t *testing.T) map[string]any { return nil },
		},
		{
			name:     "mongodb",
			provider: mongodb.Provider{},
			cfg:      func(t *testing.T) map[string]any { return map[string]any{"url": os.Getenv("AF_MONGO_URL"), "database": "af_contract"} },
			gate:     "AF_MONGO_URL",
		},
		{
			name:     "postgres",
			provider: postgres.Provider{},
			cfg:      func(t *testing.T) map[string]any { return map[string]any{"url": os.Getenv("AGENTFLOW_TEST_POSTGRES")} },
			gate:     "AGENTFLOW_TEST_POSTGRES",
		},
		{
			name:     "pgvector",
			provider: pgvector.Provider{},
			cfg: func(t *testing.T) map[string]any {
				return map[string]any{"url": os.Getenv("AGENTFLOW_TEST_POSTGRES"), "dim": 3}
			},
			gate: "AGENTFLOW_TEST_POSTGRES",
		},
		{
			name:     "redis",
			provider: redis.Provider{},
			cfg:      func(t *testing.T) map[string]any { return map[string]any{"url": os.Getenv("AF_REDIS_URL")} },
			gate:     "AF_REDIS_URL",
		},
		{
			name:     "qdrant",
			provider: qdrant.Provider{},
			cfg: func(t *testing.T) map[string]any {
				return map[string]any{"url": os.Getenv("AF_QDRANT_URL"), "collection": "af_contract"}
			},
			gate: "AF_QDRANT_URL",
		},
		{
			name:     "redisvector",
			provider: redisvector.Provider{},
			cfg:      func(t *testing.T) map[string]any { return map[string]any{"url": os.Getenv("AF_REDIS_URL")} },
			gate:     "AF_REDIS_URL",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.gate != "" && os.Getenv(c.gate) == "" {
				t.Skipf("%s is unset; skipping", c.gate)
			}
			h, err := c.provider.Open(c.cfg(t))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			t.Cleanup(func() { _ = h.Close() })
			runContract(t, h, c.provider.Features())
		})
	}
}

// runContract asserts the behaviour every backend owes its callers. Each
// assertion is gated on the feature that makes it meaningful, so adding a
// provider means declaring its features truthfully rather than writing a test.
func runContract(t *testing.T, h memory.BackendHandle, features []string) {
	t.Helper()
	has := func(f string) bool {
		for _, x := range features {
			if x == f {
				return true
			}
		}
		return false
	}
	// A table per run, so a shared server is re-runnable.
	table := "contract_" + strings.ReplaceAll(t.Name(), "/", "_")

	// A key that was never written is absent, not an error. Every caller
	// distinguishes "no memory" from "the store is broken", so both halves
	// matter.
	if v, ok, err := h.Get(table, "missing"); err != nil || ok {
		t.Fatalf("Get(missing) = %v, %v, %v; want absent with no error", v, ok, err)
	}

	if err := h.Put(table, "a:one", "first", memory.PutOpts{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	v, ok, err := h.Get(table, "a:one")
	if err != nil || !ok {
		t.Fatalf("Get after Put: ok=%v err=%v", ok, err)
	}
	if s, _ := v.(string); s != "first" {
		t.Fatalf("Get returned %#v; want the string that was written", v)
	}

	// A second write to the same key replaces it rather than appending.
	if err := h.Put(table, "a:one", "second", memory.PutOpts{}); err != nil {
		t.Fatalf("Put (overwrite): %v", err)
	}
	if v, _, _ := h.Get(table, "a:one"); v != "second" {
		t.Fatalf("overwrite returned %#v; want %q", v, "second")
	}

	if err := h.Put(table, "a:two", "other", memory.PutOpts{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := h.Put(table, "b:one", "elsewhere", memory.PutOpts{}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// prefix: exactly the keys under it, and no more.
	keys := collect(t, h, table, memory.Query{Kind: "prefix", Prefix: "a:"})
	if len(keys) != 2 || !keys["a:one"] || !keys["a:two"] {
		t.Fatalf("prefix query returned %v; want exactly a:one and a:two", keys)
	}

	// all: everything in the table.
	if got := collect(t, h, table, memory.Query{Kind: "all"}); len(got) != 3 {
		t.Fatalf("all query returned %v; want the three keys written", got)
	}

	if has("text_search") {
		got := collect(t, h, table, memory.Query{Kind: "text", Text: "elsewhere", K: 10})
		if len(got) != 1 || !got["b:one"] {
			t.Fatalf("text query returned %v; want just b:one", got)
		}
	}

	if has("vector") {
		// Three points on a line, so the nearest neighbour is unambiguous.
		for i, vec := range [][]float32{{0, 0, 0}, {1, 1, 1}, {9, 9, 9}} {
			key := []string{"v:zero", "v:one", "v:nine"}[i]
			if err := h.Put(table, key, key, memory.PutOpts{Vector: vec}); err != nil {
				t.Fatalf("Put with vector (%s): %v", key, err)
			}
		}
		it, err := h.Query(table, memory.Query{Kind: "vector", Vector: []float32{0.1, 0.1, 0.1}, K: 1})
		if err != nil {
			t.Fatalf("vector query: %v", err)
		}
		got := drain(t, it)
		if len(got) == 0 {
			t.Fatal("vector query returned nothing; a recall query would silently find no memory")
		}
		if got[0].Key != "v:zero" {
			t.Fatalf("nearest neighbour = %q; want v:zero", got[0].Key)
		}
	} else {
		// A backend that cannot do vectors must say so. An empty iterator reads
		// to a caller as "no matches", which is how a missing vector backend
		// turns into silently degraded recall instead of a loud failure.
		it, err := h.Query(table, memory.Query{Kind: "vector", Vector: []float32{1, 0, 0}, K: 1})
		if err == nil && it != nil {
			if recs := drain(t, it); len(recs) == 0 {
				t.Errorf("a vector query against a backend declaring no vector feature returned an empty result instead of an error")
			}
		}
	}

	if has("ttl") {
		if err := h.Put(table, "t:expiring", "gone soon", memory.PutOpts{TTL: 50 * time.Millisecond}); err != nil {
			t.Fatalf("Put with TTL: %v", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			v, ok, err := h.Get(table, "t:expiring")
			if err != nil {
				t.Fatalf("Get after TTL: %v", err)
			}
			if !ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("a 50ms TTL had not expired after 5s (last value %#v)", v)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}

	if err := h.Delete(table, "a:two"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := h.Get(table, "a:two"); ok {
		t.Fatal("a deleted key is still readable")
	}

	// GC is a no-op on some backends by design (a vector store's delete-by-age
	// is not implemented). What is not acceptable is an error on a healthy
	// store, which is what a broken query in the sweep would produce.
	if err := h.GC(table, 1); err != nil {
		t.Fatalf("GC: %v", err)
	}
}

// collect runs a query and returns the key set, which is what the assertions
// above are actually about.
func collect(t *testing.T, h memory.BackendHandle, table string, q memory.Query) map[string]bool {
	t.Helper()
	it, err := h.Query(table, q)
	if err != nil {
		t.Fatalf("query %+v: %v", q, err)
	}
	out := map[string]bool{}
	for _, r := range drain(t, it) {
		out[r.Key] = true
	}
	return out
}

func drain(t *testing.T, it memory.Iterator) []memory.Record {
	t.Helper()
	var out []memory.Record
	for it.Next() {
		out = append(out, it.Record())
	}
	if err := it.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return out
}
