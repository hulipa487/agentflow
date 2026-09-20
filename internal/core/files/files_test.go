package files

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/core/media"
	"agentflow/internal/core/runtime"
)

// testManager builds a Manager over a temp-dir FS blob store and a temp
// runtime store, with a controllable clock.
func testManager(t *testing.T, ttl time.Duration) (*Manager, *time.Time) {
	t.Helper()
	blobs, err := media.Open(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	rt, err := runtime.Open(filepath.Join(t.TempDir(), "rt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close() })
	now := time.Now()
	m := New(blobs, rt, ttl, 1<<20, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.now = func() time.Time { return now }
	return m, &now
}

func putString(t *testing.T, m *Manager, scope, project, p, content string) *Entry {
	t.Helper()
	e, err := m.Put(context.Background(), scope, project, p, strings.NewReader(content), "text/plain")
	if err != nil {
		t.Fatalf("Put(%s): %v", p, err)
	}
	return e
}

func TestPutGetListDelete(t *testing.T) {
	m, _ := testManager(t, time.Hour)
	ctx := context.Background()

	e1 := putString(t, m, "user:u1", "proj", "src/main.go", "package main")
	e2 := putString(t, m, "user:u1", "proj", "README.md", "# proj")
	if e1.Handle == e2.Handle {
		t.Fatal("distinct content must get distinct handles")
	}

	// Rewrite bumps the revision, dedupes nothing (new content).
	e3 := putString(t, m, "user:u1", "proj", "src/main.go", "package main // v2")
	if e3.Revision != 2 {
		t.Fatalf("rewrite revision %d, want 2", e3.Revision)
	}

	got, err := m.Get(ctx, "user:u1", "proj", "src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if got.Handle != e3.Handle || got.Revision != 2 {
		t.Fatalf("Get %+v, want handle of v2 rev 2", got)
	}

	list, err := m.List(ctx, "user:u1", "proj", "src/")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Path != "src/main.go" {
		t.Fatalf("prefix list %+v", list)
	}
	all, err := m.List(ctx, "user:u1", "proj", "")
	if err != nil || len(all) != 2 {
		t.Fatalf("full list %+v err %v", all, err)
	}

	// Scopes are isolated.
	other, err := m.List(ctx, "user:u2", "proj", "")
	if err != nil || len(other) != 0 {
		t.Fatalf("cross-scope leak: %+v err %v", other, err)
	}

	if err := m.Delete(ctx, "user:u1", "proj", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(ctx, "user:u1", "proj", "README.md"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted file must be not-found, got %v", err)
	}
}

func TestPathValidation(t *testing.T) {
	m, _ := testManager(t, time.Hour)
	ctx := context.Background()
	for _, bad := range []string{"", "/abs", "../up", "a/../../b", "a//b", "a/./b", `a\\b`, "a\x00b", strings.Repeat("x", 600)} {
		if err := validPath(bad); err == nil {
			t.Errorf("validPath(%q) accepted", bad)
		}
		if _, err := m.Put(ctx, "s", "p", bad, strings.NewReader("x"), ""); err == nil {
			t.Errorf("Put(%q) accepted", bad)
		}
	}
	if err := validPath("dir/sub/file.txt"); err != nil {
		t.Errorf("valid nested path rejected: %v", err)
	}
}

func TestCommitCheckoutHistory(t *testing.T) {
	m, now := testManager(t, time.Hour)
	ctx := context.Background()

	putString(t, m, "user:u1", "proj", "a.txt", "A")
	putString(t, m, "user:u1", "proj", "b.txt", "B")
	c1, err := m.Commit(ctx, "user:u1", "proj", "main", "initial")
	if err != nil {
		t.Fatal(err)
	}
	if c1.Parent != "" {
		t.Fatalf("first commit parent %q, want empty", c1.Parent)
	}

	// Second commit: a.txt changes, b.txt untouched → shared handle.
	putString(t, m, "user:u1", "proj", "a.txt", "A2")
	*now = now.Add(time.Second)
	c2, err := m.Commit(ctx, "user:u1", "proj", "main", "change a")
	if err != nil {
		t.Fatal(err)
	}
	if c2.Parent != c1.ID {
		t.Fatalf("parent chain broken: %q -> %q", c1.ID, c2.Parent)
	}
	if c2.Tree["b.txt"] != c1.Tree["b.txt"] {
		t.Fatal("unchanged file must share its handle across snapshots")
	}
	if c2.Tree["a.txt"] == c1.Tree["a.txt"] {
		t.Fatal("changed file must get a new handle")
	}

	// Checkout by ref resolves to the head commit.
	man, err := m.Checkout(ctx, "user:u1", "proj", "main")
	if err != nil {
		t.Fatal(err)
	}
	if man.Commit.ID != c2.ID || len(man.Files) != 2 {
		t.Fatalf("checkout by ref %+v", man)
	}
	// Checkout by commit id reaches the older snapshot.
	old, err := m.Checkout(ctx, "user:u1", "proj", c1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Commit.ID != c1.ID || old.Files[0].Handle != c1.Tree[old.Files[0].Path] {
		t.Fatalf("checkout by id %+v", old)
	}
	// Sorted manifest. b.txt is unchanged since c1, so its size resolves from
	// the working tree; a.txt was rewritten (new handle), so its size stays 0
	// — the handle is still materializable, size is informational.
	if old.Files[0].Path != "a.txt" || old.Files[1].Path != "b.txt" {
		t.Fatalf("manifest order %+v", old.Files)
	}
	if old.Files[1].Size != 1 {
		t.Fatalf("unchanged file size %d, want 1", old.Files[1].Size)
	}

	if _, err := m.Checkout(ctx, "user:u1", "proj", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ref must be not-found, got %v", err)
	}
	if _, err := m.Commit(ctx, "user:u1", "empty-proj", "main", "x"); err == nil {
		t.Fatal("committing an empty tree must fail")
	}
}

func TestScratchTTL(t *testing.T) {
	m, _ := testManager(t, time.Hour)
	ctx := context.Background()

	e, err := m.ScratchPut(ctx, "sess:worker1", "notes.txt", strings.NewReader("scratch"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.ScratchGet(ctx, "sess:worker1", "notes.txt")
	if err != nil || got.Handle != e.Handle {
		t.Fatalf("scratch get %+v err %v", got, err)
	}
	// Another owner cannot see it.
	if _, err := m.ScratchGet(ctx, "sess:worker2", "notes.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner scratch leak: %v", err)
	}

	// The deadline was written as now+ttl (expiry itself is checked against
	// the wall clock in the runtime layer; see TestScratchExpiryMechanics).
	row, ok, err := m.meta.GetRow(ctx, scratchKey("sess:worker1", "notes.txt"))
	if err != nil || !ok {
		t.Fatalf("raw row: ok=%v err %v", ok, err)
	}
	want := time.Until(row.ExpiresAt)
	if want < 55*time.Minute || want > time.Hour {
		t.Fatalf("expires in %v, want ~1h", want)
	}
}

// TestScratchExpiryMechanics drives expiry through the runtime layer's real
// clock: a row whose deadline is already past is lazily deleted on read and
// swept at boot.
func TestScratchExpiryMechanics(t *testing.T) {
	m, _ := testManager(t, time.Hour)
	ctx := context.Background()
	past := time.Now().Add(-time.Minute)

	expired := &Entry{Path: "die.txt", Handle: "media:" + strings.Repeat("a", 64), Ts: past.Unix(), Revision: 1}
	mustJSON(t, expired)
	if err := m.meta.PutRow(ctx, scratchKey("s1", "die.txt"), mustJSON(t, expired), past); err != nil {
		t.Fatal(err)
	}
	live := &Entry{Path: "live.txt", Handle: "media:" + strings.Repeat("b", 64), Ts: time.Now().Unix(), Revision: 1}
	if err := m.meta.PutRow(ctx, scratchKey("s1", "live.txt"), mustJSON(t, live), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if _, err := m.ScratchGet(ctx, "s1", "die.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired scratch must be not-found, got %v", err)
	}
	list, err := m.ScratchList(ctx, "s1")
	if err != nil || len(list) != 1 || list[0].Path != "live.txt" {
		t.Fatalf("expired scratch must vanish from list: %+v err %v", list, err)
	}
}

func TestScratchNoTTLAndSweep(t *testing.T) {
	m, _ := testManager(t, 0) // ttl 0 = never expires
	ctx := context.Background()

	if _, err := m.ScratchPut(ctx, "s1", "keep.txt", strings.NewReader("k"), ""); err != nil {
		t.Fatal(err)
	}
	row, ok, err := m.meta.GetRow(ctx, scratchKey("s1", "keep.txt"))
	if err != nil || !ok {
		t.Fatalf("raw row: ok=%v err %v", ok, err)
	}
	if !row.ExpiresAt.IsZero() {
		t.Fatalf("ttl=0 must write no deadline, got %v", row.ExpiresAt)
	}

	// Sweep removes only rows with a past deadline.
	past := time.Now().Add(-time.Minute)
	expired := &Entry{Path: "die.txt", Handle: "media:" + strings.Repeat("c", 64), Ts: past.Unix(), Revision: 1}
	if err := m.meta.PutRow(ctx, scratchKey("s2", "die.txt"), mustJSON(t, expired), past); err != nil {
		t.Fatal(err)
	}
	n, err := m.SweepScratch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("sweep removed %d rows, want 1", n)
	}
	if _, err := m.ScratchGet(ctx, "s1", "keep.txt"); err != nil {
		t.Fatalf("ttl=0 record must survive the sweep, got %v", err)
	}
}

func TestReadBlobRoundTrip(t *testing.T) {
	m, _ := testManager(t, time.Hour)
	e := putString(t, m, "user:u1", "proj", "blob.bin", "hello bytes")
	b, err := m.ReadBlob(context.Background(), e.Handle, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello bytes" {
		t.Fatalf("blob %q", b)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestPutHandle: tree records can point at an existing blob without bytes
// moving — the rollback primitive (checkout last-good, re-put by handle,
// commit).
func TestPutHandle(t *testing.T) {
	m, _ := testManager(t, time.Hour)
	ctx := context.Background()

	e1 := putString(t, m, "user:u1", "proj", "src/app.go", "package app")
	e2, err := m.PutHandle(ctx, "user:u1", "proj", "src/app.go", e1.Handle, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if e2.Handle != e1.Handle || e2.Revision != 2 {
		t.Fatalf("PutHandle must re-point the path and bump revision: %+v", e2)
	}
	// Materialization still yields the original bytes.
	b, err := m.ReadBlob(ctx, e2.Handle, 1<<20)
	if err != nil || string(b) != "package app" {
		t.Fatalf("checkout-by-handle bytes %q err %v", b, err)
	}
	// A commit over the re-pointed tree carries the handle.
	c, err := m.Commit(ctx, "user:u1", "proj", "main", "pin")
	if err != nil {
		t.Fatal(err)
	}
	if c.Tree["src/app.go"] != e1.Handle {
		t.Fatalf("commit tree must hold the re-pointed handle: %v", c.Tree)
	}
}

// TestPutHandleRejectsBadHandles: malformed and dangling handles fail at put
// time, not at checkout materialization.
func TestPutHandleRejectsBadHandles(t *testing.T) {
	m, _ := testManager(t, time.Hour)
	ctx := context.Background()

	if _, err := m.PutHandle(ctx, "s", "p", "x", "not-a-handle", ""); err == nil {
		t.Fatal("malformed handle must fail")
	}
	fake := "media:" + strings.Repeat("f", 64)
	if _, err := m.PutHandle(ctx, "s", "p", "x", fake, ""); err == nil {
		t.Fatal("dangling handle must fail (existence probe)")
	}
}

// TestScratchPutHandle: scratch records accept handles too.
func TestScratchPutHandle(t *testing.T) {
	m, _ := testManager(t, time.Hour)
	ctx := context.Background()

	e := putString(t, m, "user:u1", "proj", "blob.txt", "body")
	if _, err := m.ScratchPutHandle(ctx, "sess:X", "copy.txt", e.Handle, "text/plain"); err != nil {
		t.Fatal(err)
	}
	got, err := m.ScratchGet(ctx, "sess:X", "copy.txt")
	if err != nil || got.Handle != e.Handle {
		t.Fatalf("scratch by handle %+v err %v", got, err)
	}
}
