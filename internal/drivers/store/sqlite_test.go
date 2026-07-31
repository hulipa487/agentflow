package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentflow/internal/core/memory"
)

func TestSQLitePutGetQuery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	p := Provider{}
	h, err := p.Open(map[string]any{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	if err := h.Put("t", "k1", map[string]any{"x": 1}, memory.PutOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := h.Put("t", "k2", map[string]any{"x": 2}, memory.PutOpts{TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}

	v, found, err := h.Get("t", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected k1 to be found")
	}
	m, ok := v.(map[string]any)
	if !ok || m["x"] != float64(1) {
		t.Fatalf("unexpected value %v (%T)", v, v)
	}

	it, err := h.Query("t", memory.Query{Kind: "all"})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for it.Next() {
		count++
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected 2 records, got %d", count)
	}
}

func TestSQLiteGCWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	p := Provider{}
	h, err := p.Open(map[string]any{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	for i := 0; i < 5; i++ {
		if err := h.Put("t", "k"+string('0'+byte(i)), i, memory.PutOpts{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.GC("t", 2); err != nil {
		t.Fatal(err)
	}
	it, err := h.Query("t", memory.Query{Kind: "all"})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for it.Next() {
		count++
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected 2 records after GC, got %d", count)
	}
}

func TestSQLiteGCExpired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	p := Provider{}
	h, err := p.Open(map[string]any{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	if err := h.Put("t", "gone", "v", memory.PutOpts{TTL: time.Second}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	if err := h.GC("t", 100); err != nil {
		t.Fatal(err)
	}
	_, found, err := h.Get("t", "gone")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("expected expired record to be removed")
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
