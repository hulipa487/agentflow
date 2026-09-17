package session

import (
	"context"
	"sync"
	"testing"
)

func TestIsConfirmRequest(t *testing.T) {
	if isConfirmRequest(`{"ok":false,"needs_confirm":true,"tool":"test"}`) != true {
		t.Fatal("expected confirm request")
	}
	if isConfirmRequest(`{"ok":false,"needs_confirm":false}`) != false {
		t.Fatal("expected not confirm request")
	}
	if isConfirmRequest(`{"ok":true}`) != false {
		t.Fatal("expected not confirm request")
	}
	if isConfirmRequest(`not json`) != false {
		t.Fatal("expected not confirm request for bad JSON")
	}
}

func TestOwnerContext(t *testing.T) {
	ctx := WithOwner(context.Background(), "session-42")
	if OwnerFromCtx(ctx) != "session-42" {
		t.Fatalf("expected session-42, got %q", OwnerFromCtx(ctx))
	}
}

// TestPromptRegistryConcurrentReadWrite: a reload watcher republishes one key
// while every session snapshots the whole registry on each agent.config()
// call. Those are different goroutines on the same map, which without the
// registry's lock is a fatal "concurrent map read and map write" — not merely
// a data race — so this exercises the locking directly.
func TestPromptRegistryConcurrentReadWrite(t *testing.T) {
	reg := NewPromptRegistry(map[string]string{"kb_header": "v0"})
	const n = 2000

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < n; j++ {
				if s := reg.Snapshot(); s["kb_header"] == "" {
					t.Error("snapshot lost a key that is never deleted")
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < n; j++ {
			reg.Set("kb_header", "v1")
		}
	}()
	wg.Wait()
}

// TestPromptRegistryNilSafe: an Info built without a registry (builtin loops,
// tests) must render as an empty table rather than panicking.
func TestPromptRegistryNilSafe(t *testing.T) {
	var reg *PromptRegistry
	if s := reg.Snapshot(); len(s) != 0 {
		t.Fatalf("nil registry snapshot = %v", s)
	}
	if _, ok := reg.Get("anything"); ok {
		t.Fatal("nil registry reported a key")
	}
	reg.Set("k", "v") // must not panic
}
