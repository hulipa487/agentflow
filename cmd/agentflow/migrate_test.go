package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/core/runtime"
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
