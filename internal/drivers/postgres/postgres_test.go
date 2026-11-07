package postgres

import (
	"os"
	"testing"

	"agentflow/internal/core/memory"
)

// TestPostgresReportsVectorWhenAvailable: the provider's static features are
// the KV set; an opened handle adds vector when the database has the pgvector
// extension, and serves it from the same table.
func TestPostgresReportsVectorWhenAvailable(t *testing.T) {
	dsn := os.Getenv("AGENTFLOW_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("AGENTFLOW_TEST_POSTGRES not set")
	}
	h, err := Provider{}.Open(map[string]any{"url": dsn, "dim": 3})
	if err != nil {
		// Without the extension, Provider.Open refuses an explicit dim. Retry
		// without one: that is the KV-only deployment.
		h, err = Provider{}.Open(map[string]any{"url": dsn})
		if err != nil {
			t.Fatal(err)
		}
	}
	defer h.Close()

	hasVector := false
	for _, f := range h.(memory.BackendFeatureReporter).BackendFeatures() {
		if f == "vector" {
			hasVector = true
		}
	}
	if !hasVector {
		// KV still works; vector is simply not offered.
		if err := h.Put("pg_test", "k", "v", memory.PutOpts{}); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := h.Get("pg_test", "k"); err != nil || !ok {
			t.Fatalf("kv get: ok=%v err=%v", ok, err)
		}
		if _, err := h.Query("pg_test", memory.Query{Kind: "vector", Vector: []float32{1, 0, 0}}); err == nil {
			t.Fatal("a database without the vector extension must refuse a vector query")
		}
		return
	}

	if err := h.Put("pg_test", "near", "a", memory.PutOpts{Vector: []float32{1, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Put("pg_test", "far", "b", memory.PutOpts{Vector: []float32{0, 1, 0}}); err != nil {
		t.Fatal(err)
	}
	it, err := h.Query("pg_test", memory.Query{Kind: "vector", Vector: []float32{1, 0.1, 0}, K: 1})
	if err != nil {
		t.Fatalf("vector query: %v", err)
	}
	if !it.Next() || it.Record().Key != "near" {
		t.Fatalf("nearest = %v, want near", it.Record().Key)
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
}
