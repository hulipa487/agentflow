package session

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/builtins"
	"agentflow/internal/core/pool"
)

// logBuf is a race-safe sink for a test that reads what an actor logged while
// the actor goroutine is still writing to it.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestSupportChunkEditThatGoesBadKeepsTheRunningVersion: a plugins.dir shadow
// edited into a version this core cannot serve, after a session is already
// running, must not turn every restart into a crash. The rule for a bad Lua edit
// is that it keeps the version already running (internal/core/reload), and for a
// support chunk the version already running is the one the session loaded at its
// last start — so the restart keeps it, says so at Error, and the loop goes on
// seeing it. The session is restarted the way the reload watcher restarts one.
func TestSupportChunkEditThatGoesBadKeepsTheRunningVersion(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "token_budget.lua")
	if err := os.WriteFile(file, []byte(directive()+"SUPPORT_MARK = \"v1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	builtins.SetPluginDir(dir)
	t.Cleanup(func() { builtins.SetPluginDir("") })

	logs := &logBuf{}
	gw := &fakeGW{}
	a := New("main|test",
		Identity{SessionID: "main|test", Agent: "main", Capabilities: map[string]bool{}},
		&Info{Name: "main", HistoryBudget: 100},
		gw, nil, nil, nil, nil, map[string]OpHandler{}, pool.New(1),
		slog.New(slog.NewTextHandler(logs, nil)))
	a.LoopSrc = directive() + `function loop()
  local _ = session.inbox()
  session.send("mark:" .. tostring(SUPPORT_MARK))
end`

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	turn := func(want string) {
		t.Helper()
		before := len(gw.snapshot())
		a.Mailbox <- Message{ID: "m", Type: "user", From: "u", Text: "go", Channel: "webhook", ReplyTo: "1"}
		waitFor(t, "a turn from the loop", 5*time.Second, func() bool { return len(gw.snapshot()) > before })
		if got := gw.snapshot()[before].text; got != want {
			t.Fatalf("turn %d reported %q, want %q", before, got, want)
		}
	}

	// The session starts on the shadow, and a turn proves it is loaded.
	turn("mark:v1")

	// The edit that breaks it: a chunk declaring no prelude version, which the
	// gate refuses. The reload signal is what a watcher sends after an edit.
	if err := os.WriteFile(file, []byte("SUPPORT_MARK = \"v2\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.Reload()
	waitFor(t, "the restart to refuse the edit", 5*time.Second, func() bool {
		return strings.Contains(logs.String(), "keeping the version already loaded")
	})

	// The session is still the one that works, on the chunk it already had.
	turn("mark:v1")
	if strings.Contains(logs.String(), "MARK:v2") {
		t.Fatalf("the refused chunk reached a live session:\n%s", logs.String())
	}
}

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
