package caps

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/core/files"
	"agentflow/internal/core/media"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/session"
)

// testFileManager builds a files.Manager over temp stores for handler tests.
func testFileManager(t *testing.T) *files.Manager {
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
	return files.New(blobs, rt, time.Hour, 1<<20, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func decodeOK(t *testing.T, resp string, ok bool) map[string]any {
	t.Helper()
	if !ok {
		t.Fatalf("op failed: %s", resp)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(resp), &m); err != nil {
		t.Fatalf("resp not JSON: %v (%s)", err, resp)
	}
	if m["ok"] != true {
		t.Fatalf("resp not ok: %s", resp)
	}
	return m
}

// TestFileHandlersScopeIsolation: scope comes from the ctx user stamp — a
// different user (or no user → agent scope) sees nothing.
func TestFileHandlersScopeIsolation(t *testing.T) {
	m := testFileManager(t)
	h := FileHandlers(m, "writer")
	ctx := context.Background()
	u1 := session.WithUserUUID(ctx, "u1")

	resp, ok := h["files.put"](u1, session.Op{Type: "files.put", Project: "p", Path: "a.txt", Content: "hello"})
	decodeOK(t, resp, ok)

	resp, ok = h["files.list"](u1, session.Op{Type: "files.list", Project: "p"})
	if got := decodeOK(t, resp, ok)["entries"].([]any); len(got) != 1 {
		t.Fatalf("u1 list %+v", got)
	}

	// Another user cannot see u1's tree.
	u2 := session.WithUserUUID(ctx, "u2")
	resp, ok = h["files.list"](u2, session.Op{Type: "files.list", Project: "p"})
	if got := decodeOK(t, resp, ok)["entries"].([]any); len(got) != 0 {
		t.Fatalf("cross-user leak: %+v", got)
	}
	resp, ok = h["files.read"](u2, session.Op{Type: "files.read", Project: "p", Path: "a.txt"})
	if ok || !strings.Contains(resp, "not found") {
		t.Fatalf("cross-user read must fail, got ok=%v %s", ok, resp)
	}

	// No user stamp → agent scope, shared by all turns of that agent.
	resp, ok = h["files.put"](ctx, session.Op{Type: "files.put", Project: "p", Path: "b.txt", Content: "x"})
	decodeOK(t, resp, ok)
	resp, ok = h["files.list"](ctx, session.Op{Type: "files.list", Project: "p"})
	if got := decodeOK(t, resp, ok)["entries"].([]any); len(got) != 1 || got[0].(map[string]any)["path"] != "b.txt" {
		t.Fatalf("agent scope list %+v", got)
	}
}

// TestFileHandlersSnapshots: commit/checkout round-trip through the ops.
func TestFileHandlersSnapshots(t *testing.T) {
	m := testFileManager(t)
	h := FileHandlers(m, "writer")
	ctx := session.WithUserUUID(context.Background(), "u1")

	h["files.put"](ctx, session.Op{Type: "files.put", Project: "p", Path: "a.txt", Content: "v1"})
	resp, ok := h["files.commit"](ctx, session.Op{Type: "files.commit", Project: "p", CommitMsg: "first"})
	c1 := decodeOK(t, resp, ok)["commit"].(map[string]any)

	h["files.put"](ctx, session.Op{Type: "files.put", Project: "p", Path: "a.txt", Content: "v2"})
	resp, ok = h["files.checkout"](ctx, session.Op{Type: "files.checkout", Project: "p", Ref: "main"})
	man := decodeOK(t, resp, ok)["manifest"].(map[string]any)
	if man["commit"].(map[string]any)["id"] != c1["id"] {
		t.Fatalf("checkout by ref must resolve to the committed snapshot")
	}
	files := man["files"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["path"] != "a.txt" {
		t.Fatalf("manifest %+v", man)
	}
}

// TestFileHandlersScratch: scratch is keyed by the op's Owner stamp, never by
// anything Lua supplies.
func TestFileHandlersScratch(t *testing.T) {
	m := testFileManager(t)
	h := FileHandlers(m, "writer")
	ctx := session.WithUserUUID(context.Background(), "u1")

	resp, ok := h["files.scratch.put"](ctx, session.Op{Type: "files.scratch.put", Owner: "sess:A", Path: "tmp.txt", Content: "temp"})
	decodeOK(t, resp, ok)
	resp, ok = h["files.scratch.list"](ctx, session.Op{Type: "files.scratch.list", Owner: "sess:A"})
	if got := decodeOK(t, resp, ok)["entries"].([]any); len(got) != 1 {
		t.Fatalf("scratch list %+v", got)
	}
	// Same user, different session owner: invisible.
	resp, ok = h["files.scratch.list"](ctx, session.Op{Type: "files.scratch.list", Owner: "sess:B"})
	if got := decodeOK(t, resp, ok)["entries"].([]any); len(got) != 0 {
		t.Fatalf("cross-session scratch leak: %+v", got)
	}
}

// TestFileHandlersDisabled: a nil manager (store disabled at boot) fails
// every op honestly.
func TestFileHandlersDisabled(t *testing.T) {
	h := FileHandlers(nil, "writer")
	for name := range h {
		resp, ok := h[name](context.Background(), session.Op{Type: name, Project: "p", Path: "a.txt", Content: "x"})
		if ok || !strings.Contains(resp, "unavailable") {
			t.Fatalf("%s on a disabled store: ok=%v %s", name, ok, resp)
		}
	}
}

// TestFilesGate: without the capability every op is a denial naming it.
func TestFilesGate(t *testing.T) {
	m := testFileManager(t)
	gated := Gate(FileHandlers(m, "writer"), "files", "writer", map[string]bool{})
	resp, ok := gated["files.put"](context.Background(), session.Op{Type: "files.put", Project: "p", Path: "a", Content: "x"})
	if ok || !strings.Contains(resp, `lacks the files capability`) {
		t.Fatalf("denial expected, got ok=%v %s", ok, resp)
	}
}

// TestFilesOpsLuaBridge: the full loop-facing surface — put, list, commit,
// checkout, scratch — through the real actor + prelude.
func TestFilesOpsLuaBridge(t *testing.T) {
	m := testFileManager(t)
	handlers := map[string]session.OpHandler{}
	for k, h := range FileHandlers(m, "main") {
		handlers[k] = h
	}

	gw := &schemaGW{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := session.New("main|test",
		session.Identity{SessionID: "main|test", Agent: "main", Capabilities: map[string]bool{"files": true}},
		&session.Info{Name: "main", HistoryBudget: 100},
		gw, nil, nil, nil, nil, handlers, pool.New(4), log)
	a.LoopSrc = `
function loop()
  local msg = session.inbox()
  local p = files.put("proj", "hello.txt", "hi there")
  local c = files.commit("proj", { ref = "main", message = "init" })
  local k = files.checkout("proj", "main")
  local s = files.scratch.put("tmp.txt", "scratch-body")
  local sl = files.scratch.list()
  if p.ok and c.ok and k.ok and s.ok and #sl.entries == 1 and k.manifest.files[1].path == "hello.txt" then
    session.send("ok:" .. p.entry.handle)
  else
    session.send("bad")
  end
end
`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	a.Mailbox <- session.Message{ID: "m1", Type: "user", From: "user:u1", Text: "go", Channel: "webhook", ReplyTo: "1"}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		gw.mu.Lock()
		n := len(gw.sends)
		gw.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	gw.mu.Lock()
	sends := append([]string(nil), gw.sends...)
	gw.mu.Unlock()
	if len(sends) == 0 {
		t.Fatal("no reply from loop")
	}
	if !strings.HasPrefix(sends[0], "ok:media:") {
		t.Fatalf("files ops across the bridge failed: %s", sends[0])
	}

	// The put landed under the inbound sender's user scope.
	u1ctx := session.WithUserUUID(context.Background(), "u1")
	entries, err := m.List(u1ctx, "user:u1", "proj", "")
	if err != nil || len(entries) != 1 {
		t.Fatalf("bridge write must land in the user scope: %+v err %v", entries, err)
	}
}
