package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/core/memory"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/storedb"
	pgmem "agentflow/internal/drivers/postgres"
	sqlitemem "agentflow/internal/drivers/sqlite"
)

// TestMigrateStoreSQLiteToSQLite: the copy path is the same on both backends,
// so the smallest test is two files. It also checks the empty-destination
// guard: a second run refuses until -append.
func TestMigrateStoreSQLiteToSQLite(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.db")
	dst := filepath.Join(dir, "dest.db")

	srcStore, err := runtime.OpenStore(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := srcStore.PutRow(ctx, "route|state|chat-1", `{"mode":"group"}`, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := srcStore.Close(); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	if code := cmdMigrateStore([]string{"-from", src, "-to", dst}, &out, &errb); code != 0 {
		t.Fatalf("migrate: code=%d stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "files_meta") {
		t.Fatalf("output did not report the copied table:\n%s", out.String())
	}

	dstStore, err := runtime.OpenStore(dst, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dstStore.Close()
	row, ok, err := dstStore.GetRow(ctx, "route|state|chat-1")
	if err != nil || !ok {
		t.Fatalf("destination row: ok=%v err=%v", ok, err)
	}
	if row.Value != `{"mode":"group"}` {
		t.Fatalf("destination value = %q", row.Value)
	}

	// A second run without -append must refuse rather than duplicate.
	out.Reset()
	errb.Reset()
	if code := cmdMigrateStore([]string{"-from", src, "-to", dst}, &out, &errb); code != 1 {
		t.Fatalf("second migrate: code=%d, want 1", code)
	}
	if !strings.Contains(errb.String(), "already has") {
		t.Fatalf("refusal did not explain the non-empty destination:\n%s", errb.String())
	}
}

func TestMigrateStoreRejectsUnknownTable(t *testing.T) {
	var out, errb bytes.Buffer
	code := cmdMigrateStore([]string{"-from", "a.db", "-to", "b.db", "-tables", "nope"}, &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "unknown table") {
		t.Fatalf("code=%d stderr=%s", code, errb.String())
	}
}

// TestMigrateStoreToPostgres is the tier change the command exists for. Gated
// on AGENTFLOW_TEST_POSTGRES like the other backend integration tests.
func TestMigrateStoreToPostgres(t *testing.T) {
	dsn := os.Getenv("AGENTFLOW_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("AGENTFLOW_TEST_POSTGRES not set")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "source.db")
	srcStore, err := runtime.OpenStore(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := "route|state|migrate-" + time.Now().Format("150405.000000000")
	if err := srcStore.PutRow(ctx, key, "migrated", time.Time{}); err != nil {
		t.Fatal(err)
	}
	_ = srcStore.Close()

	// A fresh database per run keeps the append guard out of the test. The
	// table is created by the destination runtime store.
	var out, errb bytes.Buffer
	if code := cmdMigrateStore([]string{"-from", src, "-to", dsn, "-tables", "files_meta", "-append"}, &out, &errb); code != 0 {
		t.Fatalf("migrate: code=%d stderr=%s", code, errb.String())
	}
	dstStore, err := runtime.OpenStore(dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dstStore.Close()
	row, ok, err := dstStore.GetRow(ctx, key)
	if err != nil || !ok || row.Value != "migrated" {
		t.Fatalf("destination row: ok=%v err=%v row=%+v", ok, err, row)
	}
}

// seedMemory writes three memory rows through the sqlite memory driver: one
// with an embedding, one with a TTL, one already expired. It backdates the
// first so the test can see that updated_at survives the copy.
func seedMemory(t *testing.T, path string) {
	t.Helper()
	h, err := sqlitemem.Provider{}.Open(map[string]any{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := h.Put("dialogue", "u1:0001", map[string]any{"text": "hello"}, memory.PutOpts{Vector: []float32{0.25, -1, 3.5}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Put("facts", "u1:likes", "tea", memory.PutOpts{TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := h.Put("facts", "u1:stale", "gone", memory.PutOpts{TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	db, err := storedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.Exec(ctx, `UPDATE kv SET updated_at = 1000 WHERE key = 'u1:0001'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE kv SET expires_at = 1 WHERE key = 'u1:stale'`); err != nil {
		t.Fatal(err)
	}
}

// Memory moves with the store: values, vectors, recency and TTLs all survive,
// and an expired row is left behind.
func TestMigrateStoreCopiesMemory(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.db")
	dst := filepath.Join(dir, "dest.db")
	seedMemory(t, src)

	var out, errb bytes.Buffer
	if code := cmdMigrateStore([]string{"-from", src, "-to", dst, "-tables", "memory"}, &out, &errb); code != 0 {
		t.Fatalf("migrate: code=%d stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "memory") || !strings.Contains(out.String(), "2 rows") {
		t.Fatalf("output did not report two memory rows:\n%s", out.String())
	}

	h, err := sqlitemem.Provider{}.Open(map[string]any{"path": dst})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if v, ok, err := h.Get("facts", "u1:likes"); err != nil || !ok || v != "tea" {
		t.Fatalf("facts row: v=%v ok=%v err=%v", v, ok, err)
	}
	if _, ok, _ := h.Get("facts", "u1:stale"); ok {
		t.Fatal("expired row was copied")
	}

	db, err := storedb.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	var updated int64
	var emb []byte
	if err := db.QueryRow(ctx, `SELECT updated_at, embedding FROM kv WHERE key = 'u1:0001'`).Scan(&updated, &emb); err != nil {
		t.Fatal(err)
	}
	if updated != 1000 {
		t.Fatalf("updated_at = %d, want the source's 1000", updated)
	}
	vec, err := decodeAnyVector(emb)
	if err != nil || len(vec) != 3 || vec[0] != 0.25 || vec[1] != -1 || vec[2] != 3.5 {
		t.Fatalf("embedding = %v (err %v), want [0.25 -1 3.5]", vec, err)
	}
	var expires *int64
	if err := db.QueryRow(ctx, `SELECT expires_at FROM kv WHERE key = 'u1:likes'`).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if expires == nil || *expires <= time.Now().Unix() {
		t.Fatalf("TTL did not survive: expires_at = %v", expires)
	}
	// The full-text index is fed by the driver's triggers on insert.
	var hits int
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM kv_fts WHERE kv_fts MATCH 'tea'`).Scan(&hits); err != nil || hits != 1 {
		t.Fatalf("text search over migrated rows: hits=%d err=%v", hits, err)
	}
}

func TestVectorLiteralRoundTrip(t *testing.T) {
	in := []float32{0.1, -2, 3.25e-7}
	got, err := parseVectorLiteral(vectorLiteral(in))
	if err != nil || len(got) != len(in) {
		t.Fatalf("round trip: %v %v", got, err)
	}
	for i := range in {
		if got[i] != in[i] {
			t.Fatalf("element %d: %v != %v", i, got[i], in[i])
		}
	}
}

// The tier change proper: SQLite memory and usage events into PostgreSQL.
// After the copy the next usage event must get a fresh id, not collide with a
// migrated one. Gated like the other backend integration tests; the database
// should be empty, since memory and usage_events refuse a non-empty target.
func TestMigrateStoreMemoryAndUsageToPostgres(t *testing.T) {
	dsn := os.Getenv("AGENTFLOW_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("AGENTFLOW_TEST_POSTGRES not set")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "source.db")
	seedMemory(t, src)
	srcStore, err := runtime.OpenStore(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	type appender interface {
		AppendEvent(string, runtime.UsageEvent) error
	}
	for i := 0; i < 3; i++ {
		if err := srcStore.(appender).AppendEvent("u1", runtime.UsageEvent{Ts: time.Now().Unix(), Kind: "llm", Model: "m", Agent: "a", OK: true}); err != nil {
			t.Fatal(err)
		}
	}
	_ = srcStore.Close()

	var out, errb bytes.Buffer
	if code := cmdMigrateStore([]string{"-from", src, "-to", dsn, "-tables", "memory,usage_events"}, &out, &errb); code != 0 {
		t.Fatalf("migrate: code=%d stderr=%s", code, errb.String())
	}
	dstStore, err := runtime.OpenStore(dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dstStore.Close()
	if err := dstStore.(appender).AppendEvent("u1", runtime.UsageEvent{Ts: time.Now().Unix(), Kind: "llm", Model: "m", Agent: "a", OK: true}); err != nil {
		t.Fatalf("first insert after migration collided: %v", err)
	}
	h, err := pgmem.Provider{}.Open(map[string]any{"url": dsn, "dim": 3})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if v, ok, err := h.Get("facts", "u1:likes"); err != nil || !ok || v != "tea" {
		t.Fatalf("facts row: v=%v ok=%v err=%v", v, ok, err)
	}
}
