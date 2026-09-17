package qdrant

import (
	"os"
	"testing"

	"agentflow/internal/core/memory"
)

// TestIntegrationAgainstLiveQdrant exercises the real REST API: create the
// collection, upsert, k-NN search, get, delete.
//
// It is skipped unless AF_QDRANT_URL points at a running Qdrant, because the
// shapes asserted here are the ones the unit tests cannot check — those run
// against this package's own fake, which encodes what we believe the API to be
// rather than what it is. Run it against a real server before trusting the
// driver:
//
//	AF_QDRANT_URL=http://localhost:6333 go test ./internal/drivers/qdrant/ -run Integration -v
func TestIntegrationAgainstLiveQdrant(t *testing.T) {
	url := os.Getenv("AF_QDRANT_URL")
	if url == "" {
		t.Skip("set AF_QDRANT_URL to run against a live Qdrant")
	}
	const (
		collection = "agentflow_integration_test"
		table      = "it.facts"
	)
	h, err := Provider{}.Open(map[string]any{
		"url":        url,
		"dim":        4,
		"collection": collection,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	handle := h.(*Handle)
	t.Cleanup(func() {
		// Clean up only the points this test made.
		for _, k := range []string{"a", "b", "c"} {
			_ = handle.Delete(table, k)
		}
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

	// The nearest neighbour of a's own vector must be a.
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
		t.Fatalf("payload value not round-tripped: %#v", rec.Value)
	}

	// A different table must not see these points.
	other, err := handle.Query("it.other", memory.Query{Kind: "vector", Vector: []float32{1, 0, 0, 0}, K: 5})
	if err != nil {
		t.Fatalf("cross-table query: %v", err)
	}
	if other.Next() {
		t.Fatal("the table filter leaked across tables")
	}

	v, ok, err := handle.Get(table, "b")
	if err != nil || !ok {
		t.Fatalf("get b: ok=%v err=%v", ok, err)
	}
	if m, _ := v.(map[string]any); m["text"] != "second" {
		t.Fatalf("get returned %#v", v)
	}

	if err := handle.Delete(table, "b"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, err := handle.Get(table, "b"); ok || err != nil {
		t.Fatalf("a deleted point must read as absent: ok=%v err=%v", ok, err)
	}

	// A value written with no embedding must not disturb the stored vector.
	if err := handle.Put(table, "a", map[string]any{"text": "first, revised"}, memory.PutOpts{}); err != nil {
		t.Fatalf("vectorless put: %v", err)
	}
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
