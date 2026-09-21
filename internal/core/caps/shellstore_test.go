package caps

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"agentflow/internal/core/runtime"
	"agentflow/internal/drivers/shell"
)

// shellStoreHarness is the registry over a real store — the rows a fleet's
// instances read and write — plus the store itself, so a test can look at what
// landed in the table.
func shellStoreHarness(t *testing.T) (ShellStore, runtime.Store, context.Context) {
	t.Helper()
	st, err := runtime.OpenSQLite(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return ShellStore{Store: st}, st, context.Background()
}

// A record is written once and read back whole: everything an instance that
// takes a session over needs to reach the container is in it, and nothing it
// does not need is.
func TestShellStoreRoundTrip(t *testing.T) {
	reg, _, ctx := shellStoreHarness(t)

	rec := shell.Record{
		ID: "h-1", Owner: "bot|chat-1", Provider: "docker", Image: "alpine:3.20",
		State: shell.HandleRunning, Container: "c-1", Instance: "instance-a",
		CreatedAt: 100, LiveAt: 200,
	}
	if err := reg.Save(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, ok, err := reg.Load(ctx, "h-1")
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if got.Owner != rec.Owner || got.Container != rec.Container || got.Instance != rec.Instance ||
		got.Provider != rec.Provider || got.Image != rec.Image || got.State != rec.State ||
		got.CreatedAt != rec.CreatedAt || got.LiveAt != rec.LiveAt {
		t.Fatalf("record did not round-trip:\n got %+v\nwant %+v", got, rec)
	}
	// Saving again replaces: a handle's record is one row, not a log.
	rec.LiveAt = 300
	if err := reg.Save(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := reg.Load(ctx, "h-1"); got.LiveAt != 300 {
		t.Fatalf("the second save did not replace the first: %+v", got)
	}
	if _, ok, err := reg.Load(ctx, "h-never"); ok || err != nil {
		t.Fatalf("an unknown handle = ok=%v err=%v", ok, err)
	}
}

// Owner filtering is what both callers of List ask for: the reclaim pass wants
// every handle in the deployment, and an instance resolving a session's shell
// wants that session's.
func TestShellStoreFiltersByOwner(t *testing.T) {
	reg, _, ctx := shellStoreHarness(t)
	for _, rec := range []shell.Record{
		{ID: "h-1", Owner: "bot|chat-1", Provider: "docker", State: shell.HandleRunning, Container: "c-1"},
		{ID: "h-2", Owner: "bot|chat-1", Provider: "docker", State: shell.HandleRunning, Container: "c-2"},
		{ID: "h-3", Owner: "bot|chat-2", Provider: "ssh", State: shell.HandleRunning, Host: "box:22"},
	} {
		if err := reg.Save(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}

	all, err := reg.List(ctx, "")
	if err != nil || len(all) != 3 {
		t.Fatalf("deployment-wide list = %d records, err=%v", len(all), err)
	}
	mine, err := reg.List(ctx, "bot|chat-1")
	if err != nil || len(mine) != 2 {
		t.Fatalf("owner list = %d records, err=%v", len(mine), err)
	}
	for _, rec := range mine {
		if rec.Owner != "bot|chat-1" {
			t.Fatalf("owner list carried another session's handle: %+v", rec)
		}
	}
	if none, err := reg.List(ctx, "bot|nobody"); err != nil || len(none) != 0 {
		t.Fatalf("a session with no shells = %d records, err=%v", len(none), err)
	}

	if err := reg.Delete(ctx, "h-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := reg.Load(ctx, "h-1"); ok {
		t.Fatal("a deleted record is still readable")
	}
	// Deleting what is already gone is not an error: releasing a resource twice
	// is one state.
	if err := reg.Delete(ctx, "h-1"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

// The registry shares the runtime store's row table with the file store's
// metadata and a session's state, so its prefix has to be its own: a handle
// listing that swept up another subsystem's rows would hand the reclaim pass
// records it does not understand.
func TestShellStoreKeepsItsOwnRows(t *testing.T) {
	reg, st, ctx := shellStoreHarness(t)

	if err := st.PutRow(ctx, "t|user:x|proj|file.txt", "not a handle", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRow(ctx, "session|bot|chat-1|state|cursor", "3", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Save(ctx, shell.Record{ID: "h-1", Owner: "bot|chat-1", Provider: "docker", State: shell.HandleRunning}); err != nil {
		t.Fatal(err)
	}

	all, err := reg.List(ctx, "")
	if err != nil || len(all) != 1 || all[0].ID != "h-1" {
		t.Fatalf("the handle list picked up other subsystems' rows: %+v err=%v", all, err)
	}
	rows, err := st.ListRows(ctx, shellRowPrefix)
	if err != nil || len(rows) != 1 {
		t.Fatalf("prefix scan = %d rows, err=%v", len(rows), err)
	}
}
