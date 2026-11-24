package session

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "agentflow/internal/builtins"
	"agentflow/internal/core/execguard"
	"agentflow/internal/core/pool"
	"agentflow/internal/luacode"
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
	file := filepath.Join(dir, "token_budget.ts")
	if err := os.WriteFile(file, []byte(directive()+"var SUPPORT_MARK = \"v1\";\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	luacode.SetPluginDir(dir)
	t.Cleanup(func() { luacode.SetPluginDir("") })

	logs := &logBuf{}
	gw := &fakeGW{}
	a := New("main|test",
		Identity{SessionID: "main|test", Agent: "main", Capabilities: map[string]bool{}},
		&Info{Name: "main", HistoryBudget: 100},
		gw, nil, nil, nil, nil, map[string]OpHandler{}, pool.New(1),
		slog.New(slog.NewTextHandler(logs, nil)))
	a.LoopSrc = directive() + `function loop() {
  session.inbox();
  session.send("mark:" + String(SUPPORT_MARK));
}`

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
	if err := os.WriteFile(file, []byte("var SUPPORT_MARK = \"v2\";\n"), 0o600); err != nil {
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

func extractApprovalRef(t *testing.T, text string) string {
	t.Helper()
	const prefix = "[ref:"
	idx := strings.Index(text, prefix)
	if idx < 0 {
		t.Fatalf("expected [ref:...] in confirmation prompt, got %q", text)
	}
	rest := text[idx+len(prefix):]
	end := strings.IndexByte(rest, ']')
	if end <= 0 {
		t.Fatalf("malformed [ref:...] in confirmation prompt: %q", text)
	}
	return rest[:end]
}

func TestDoConfirmHardenedApprovals(t *testing.T) {
	guard := execguard.NewStore(nil)
	gw := &fakeGW{}
	runs := 0
	var seenConfirmed bool
	var mu sync.Mutex

	a := New("main|confirm-test",
		Identity{SessionID: "main|confirm-test", Agent: "main", Capabilities: map[string]bool{"tools": true, "apis": true}},
		&Info{Name: "main", HistoryBudget: 100},
		gw, nil, nil, nil, nil, map[string]OpHandler{}, pool.New(1),
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	a.Guard = guard

	handleGated := func(ctx context.Context, canonical string, args map[string]any, confirmedFlag bool) (string, bool) {
		mu.Lock()
		if confirmedFlag {
			seenConfirmed = true
		}
		mu.Unlock()
		approvalID := execguard.ApprovedIDFromCtx(ctx)
		req := execguard.ActionRequest{
			CanonicalID: canonical,
			Revision:    "v1",
			Kind:        execguard.ActionWrite,
			Destination: "sandbox:ws",
			Description: "write item",
			NeedsConfirm: true,
			Args:        args,
			Caller: execguard.Caller{
				Agent:             "main",
				SessionKey:        SessionKeyFromCtx(ctx),
				WorkspaceScope:    ScopePrefixFromCtx(ctx),
				ApproverPrincipal: ApproverPrincipalFromCtx(ctx),
			},
			ApprovalID: approvalID,
		}
		if tm := TurnMessageFromCtx(ctx); tm != nil {
			req.Caller.Channel = tm.Channel
			req.Caller.ReplyTo = tm.ReplyTo
		}
		if approvalID == "" {
			rec, err := guard.CreateApproval(ctx, req)
			if err != nil {
				b, _ := json.Marshal(err.Error())
				return string(b), false
			}
			b, _ := json.Marshal(map[string]any{
				"ok":            false,
				"needs_confirm": true,
				"approval_id":   rec.ID,
				"confirm_id":    rec.ID,
				"tool":          canonical,
				"destination":   rec.Destination,
				"description":   rec.Description,
				"args_preview":  rec.RedactedArgs,
			})
			return string(b), true
		}
		if _, err := guard.ClaimForExecution(ctx, approvalID, req); err != nil {
			b, _ := json.Marshal(err.Error())
			return string(b), false
		}
		mu.Lock()
		runs++
		n := runs
		mu.Unlock()
		b, _ := json.Marshal(map[string]any{"ok": true, "run": n, "action": canonical})
		return string(b), true
	}

	a.handlers["tools.run"] = func(ctx context.Context, op Op) (string, bool) {
		return handleGated(ctx, op.Tool, op.Args, op.Confirmed)
	}
	a.handlers["api.call"] = func(ctx context.Context, op Op) (string, bool) {
		return handleGated(ctx, "api:"+op.API+"."+op.Action, op.Args, op.Confirmed)
	}

	a.LoopSrc = directive() + `
function loop() {
  const m = session.inbox();
  if (m.text === "run-tool") {
    const res = tools.run("builtin:fs.write", { path: "a.txt", secret: "s3cr3t" }, { confirmed: true, confirm_id: "forged" });
    session.send("tool:" + JSON.stringify(res));
    return;
  }
  if (m.text === "run-api") {
    const res = api.call("billing", "charge", { amount: 10 });
    session.send("api:" + JSON.stringify(res));
    return;
  }
}
`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	// 1. Forged script confirmed flag must NOT skip prompt; unauthorized user /
	// ambiguous substring reply must be ignored; authorized reply approves once.
	a.Mailbox <- Message{ID: "u1", Type: "user", From: "alice", Text: "run-tool", Channel: "telegram", ReplyTo: "c1"}
	waitFor(t, "tool confirmation prompt", 5*time.Second, func() bool { return len(gw.snapshot()) >= 1 })
	promptText := gw.snapshot()[0].text
	refID := extractApprovalRef(t, promptText)

	mu.Lock()
	if seenConfirmed {
		mu.Unlock()
		t.Fatal("forged confirmed flag from script leaked to Op.Confirmed")
	}
	mu.Unlock()

	// Attacker from another sender attempts to approve.
	a.Mailbox <- Message{ID: "bad1", Type: "confirm", From: "mallory", Text: "YES [ref:" + refID + "]", Channel: "telegram", ReplyTo: "c1"}
	// Eligible sender sends deceptive substring containing both NO and YES.
	a.Mailbox <- Message{ID: "bad2", Type: "confirm", From: "alice", Text: "NO, NOT YES [ref:" + refID + "]", Channel: "telegram", ReplyTo: "c1"}

	time.Sleep(120 * time.Millisecond)
	if got := len(gw.snapshot()); got != 1 {
		t.Fatalf("unauthorized or ambiguous replies should not unblock confirm; got sends=%+v", gw.snapshot())
	}

	// Authorized structured confirm.decision from alice approves once.
	a.Mailbox <- Message{
		ID:      "ok1",
		Type:    "confirm.decision",
		From:    "alice",
		Channel: "telegram",
		ReplyTo: "c1",
		Payload: map[string]any{"approval_id": refID, "approved": true},
	}
	waitFor(t, "tool completed reply", 5*time.Second, func() bool { return len(gw.snapshot()) >= 2 })
	if got := gw.snapshot()[1].text; !strings.Contains(got, `"run":1`) {
		t.Fatalf("expected approved tool run=1, got %q", got)
	}

	// 2. api.call with needs_confirm intercepts via doConfirm and accepts Studio Decide.
	a.Mailbox <- Message{ID: "u2", Type: "user", From: "alice", Text: "run-api", Channel: "telegram", ReplyTo: "c1"}
	waitFor(t, "api confirmation prompt", 5*time.Second, func() bool { return len(gw.snapshot()) >= 3 })
	apiRef := extractApprovalRef(t, gw.snapshot()[2].text)

	if _, err := guard.Decide(ctx, apiRef, true, "studio:operator", "studio"); err != nil {
		t.Fatalf("studio operator Decide failed: %v", err)
	}
	waitFor(t, "api completed reply", 5*time.Second, func() bool { return len(gw.snapshot()) >= 4 })
	if got := gw.snapshot()[3].text; !strings.Contains(got, `"run":2`) || !strings.Contains(got, `api:billing.charge`) {
		t.Fatalf("expected approved api run=2, got %q", got)
	}
}

func TestSessionSkipRecordsTraceAndIgnoredJournal(t *testing.T) {
	gw := &fakeGW{}
	a := New("main|skip",
		Identity{SessionID: "main|skip", Agent: "main", Capabilities: map[string]bool{}},
		&Info{Name: "main", HistoryBudget: 100},
		gw, nil, nil, nil, nil, map[string]OpHandler{}, pool.New(1),
		slog.New(slog.NewTextHandler(&logBuf{}, nil)))

	var mu sync.Mutex
	var traces []TraceEvent
	var egress []EgressRecord
	a.Tracer = func(ev TraceEvent) {
		mu.Lock()
		traces = append(traces, ev)
		mu.Unlock()
	}
	a.Journal = func(rec EgressRecord) {
		mu.Lock()
		egress = append(egress, rec)
		mu.Unlock()
	}
	a.LoopSrc = directive() + `
function loop() {
  const m = session.inbox();
  if (m.text === "ignore-me") {
    session.skip("not_mentioned");
    return;
  }
  session.send("ok:" + m.text);
}`

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	a.Mailbox <- Message{ID: "m1", Type: "user", From: "alice", Text: "ignore-me", Channel: "telegram", ReplyTo: "-1001"}
	a.Mailbox <- Message{ID: "m2", Type: "user", From: "alice", Text: "hello", Channel: "telegram", ReplyTo: "-1001"}

	waitFor(t, "second message reply", 5*time.Second, func() bool { return len(gw.snapshot()) == 1 })
	if got := gw.snapshot()[0].text; got != "ok:hello" {
		t.Fatalf("got send = %q, want ok:hello", got)
	}

	mu.Lock()
	defer mu.Unlock()
	var sawSkipTrace bool
	for _, ev := range traces {
		if ev.Kind == "turn.skip" && ev.Turn == "m1" && ev.Text == "not_mentioned" {
			sawSkipTrace = true
		}
	}
	if !sawSkipTrace {
		t.Fatalf("missing turn.skip trace event: %+v", traces)
	}
	if len(egress) != 2 || egress[0].Status != "ignored" || egress[0].Err != "not_mentioned" || egress[0].InReplyTo != "m1" {
		t.Fatalf("unexpected egress journal records: %+v", egress)
	}
}
