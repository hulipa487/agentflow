package redisvector

import (
	"context"
	"os"
	"testing"
	"time"

	"agentflow/internal/core/memory"
)

// TestIntegrationAgainstLiveRediSearch exercises a real Redis with the
// RediSearch module: index creation, k-NN search, kv round-trip, delete.
//
// It is skipped unless AF_REDIS_URL points at Redis Stack, because this is the
// only place the FT.CREATE / FT.SEARCH command shapes are checked against a
// server that actually parses them — the unit tests assert the arguments
// against this package's own expectation, not against RediSearch.
//
// Last verified green against Redis Stack with RediSearch module 81000
// (2026-09-17), which covered index creation, the k-NN query string, the
// kv/scan paths, delete, and the vectorless put. Note the k-NN query needs
// RESP2 (see parseOptions); this test is what caught that. Re-run it after a
// Redis or module upgrade:
//
//	AF_REDIS_URL=redis://localhost:6379 go test ./internal/drivers/redisvector/ -run Integration -v
func TestIntegrationAgainstLiveRediSearch(t *testing.T) {
	url := os.Getenv("AF_REDIS_URL")
	if url == "" {
		t.Skip("set AF_REDIS_URL to run against a live Redis with RediSearch")
	}
	const table = "it.facts"
	h, err := Provider{}.Open(map[string]any{"url": url, "dim": 4})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	handle := h.(*Handle)
	ctx := context.Background()
	t.Cleanup(func() {
		// Drop the index and the documents it covered.
		_ = handle.client.Do(ctx, "FT.DROPINDEX", indexFor(table), "DD").Err()
		_ = handle.Close()
	})

	points := []struct {
		key string
		vec []float32
		val map[string]any
	}{
		{"a", []float32{1, 0, 0, 0}, map[string]any{"text": "first"}},
		{"b", []float32{0, 1, 0, 0}, map[string]any{"text": "second"}},
		{"c", []float32{0, 0, 1, 0}, map[string]any{"text": "third"}},
	}
	for _, p := range points {
		if err := handle.Put(table, p.key, p.val, memory.PutOpts{Vector: p.vec}); err != nil {
			t.Fatalf("put %s: %v", p.key, err)
		}
	}
	// RediSearch indexes asynchronously; give it a moment before searching.
	time.Sleep(300 * time.Millisecond)

	it, err := handle.Query(table, memory.Query{Kind: "vector", Vector: []float32{1, 0, 0, 0}, K: 1})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !it.Next() {
		t.Fatalf("no hits (err=%v)", it.Err())
	}
	rec := it.Record()
	if rec.Key != "a" {
		t.Fatalf("nearest = %q; want a", rec.Key)
	}
	if m, ok := rec.Value.(map[string]any); !ok || m["text"] != "first" {
		t.Fatalf("value not round-tripped: %#v", rec.Value)
	}

	// kv and prefix_scan come free with the same records.
	v, ok, err := handle.Get(table, "b")
	if err != nil || !ok {
		t.Fatalf("get b: ok=%v err=%v", ok, err)
	}
	if m, _ := v.(map[string]any); m["text"] != "second" {
		t.Fatalf("get returned %#v", v)
	}
	scan, err := handle.Query(table, memory.Query{Kind: "all"})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	n := 0
	for scan.Next() {
		n++
	}
	if scan.Err() != nil {
		t.Fatal(scan.Err())
	}
	if n != 3 {
		t.Fatalf("scan returned %d records; want 3", n)
	}

	if err := handle.Delete(table, "b"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, err := handle.Get(table, "b"); ok || err != nil {
		t.Fatalf("a deleted key must read as absent: ok=%v err=%v", ok, err)
	}

	// A value written with no embedding must not disturb the stored vector:
	// HSET leaves the embedding field alone.
	if err := handle.Put(table, "a", map[string]any{"text": "first, revised"}, memory.PutOpts{}); err != nil {
		t.Fatalf("vectorless put: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	it, err = handle.Query(table, memory.Query{Kind: "vector", Vector: []float32{1, 0, 0, 0}, K: 1})
	if err != nil {
		t.Fatalf("query after vectorless put: %v", err)
	}
	if !it.Next() {
		t.Fatalf("the vector was lost by a vectorless put (err=%v)", it.Err())
	}
	if rec := it.Record(); rec.Key != "a" {
		t.Fatalf("after a vectorless put the nearest = %q; want a", rec.Key)
	}
}
