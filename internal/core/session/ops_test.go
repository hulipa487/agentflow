package session

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/core/gateway"
	"agentflow/internal/core/media"
	"agentflow/internal/core/pool"
	"agentflow/internal/vm"
)

// directive is the leading comment line every chunk has to declare. The loop
// fixtures in this package are chunks: vm.State.Start refuses one that declares
// nothing, so they go through this the way a shipped loop does.
func directive() string { return fmt.Sprintf("-- af-prelude-version: %d\n", vm.PreludeVersion) }

// Observations cross the Lua boundary through a fake gateway: loops report
// outcomes with session.send, and pushes arrive as direct sends.
type sendRec struct {
	channel, replyTo, text string
}

type fakeGW struct {
	mu      sync.Mutex
	sends   []sendRec
	replies []gateway.Reply
	deltas  []string
	turns   []endTurnRec
}

func (f *fakeGW) Send(channel, replyTo, text string, _ []media.Part) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, sendRec{channel, replyTo, text})
	return nil
}

// SendReply records the answer as a send so a test asserting on gateway egress
// sees it. The structure itself is asserted through the reply recorder where a
// test cares.
func (f *fakeGW) SendReply(channel, replyTo string, r gateway.Reply) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, sendRec{channel, replyTo, r.Text})
	f.replies = append(f.replies, r)
	return nil
}

func (f *fakeGW) SendDelta(channel, replyTo, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deltas = append(f.deltas, text)
	return nil
}

func (f *fakeGW) EndTurn(channel, replyTo string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turns = append(f.turns, endTurnRec{channel: channel, replyTo: replyTo, err: err})
}

type endTurnRec struct {
	channel, replyTo string
	err              error
}

func (f *fakeGW) snapshot() []sendRec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sendRec, len(f.sends))
	copy(out, f.sends)
	return out
}

func (f *fakeGW) repliesSnapshot() []gateway.Reply {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]gateway.Reply, len(f.replies))
	copy(out, f.replies)
	return out
}

func (f *fakeGW) deltasSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.deltas))
	copy(out, f.deltas)
	return out
}

func (f *fakeGW) turnsSnapshot() []endTurnRec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]endTurnRec, len(f.turns))
	copy(out, f.turns)
	return out
}

func newTestActor(t *testing.T, caps map[string]bool, gw Gateway, loopSrc string) (*Actor, context.CancelFunc) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New("main|test",
		Identity{SessionID: "main|test", Agent: "main", Capabilities: caps},
		&Info{Name: "main", HistoryBudget: 100},
		gw, nil, nil, nil, nil, map[string]OpHandler{}, pool.New(1), log)
	a.LoopSrc = directive() + loopSrc
	ctx, cancel := context.WithCancel(context.Background())
	go a.Run(ctx)
	t.Cleanup(cancel)
	return a, cancel
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSessionPushRequiresCapability(t *testing.T) {
	gw := &fakeGW{}
	a, _ := newTestActor(t, map[string]bool{}, gw, `
function loop()
  local msg = session.inbox()
  local ok, err = pcall(session.push, "telegram", "123", "hello")
  session.send(tostring(ok) .. ":" .. tostring(err))
end
`)
	a.Mailbox <- Message{ID: "1", Type: "user", From: "u", Text: "go", Channel: "webhook", ReplyTo: "r1"}

	waitFor(t, "loop report", 5*time.Second, func() bool { return len(gw.snapshot()) > 0 })
	sends := gw.snapshot()
	if len(sends) != 1 {
		t.Fatalf("expected exactly one gateway send (the report), got %d: %+v", len(sends), sends)
	}
	if sends[0].channel != "webhook" || sends[0].replyTo != "r1" {
		t.Fatalf("report went to wrong recipient: %+v", sends[0])
	}
	if !strings.HasPrefix(sends[0].text, "false:") || !strings.Contains(sends[0].text, "channel.push") {
		t.Fatalf("expected capability denial, got %q", sends[0].text)
	}
}

func TestSessionPushDelivers(t *testing.T) {
	gw := &fakeGW{}
	a, _ := newTestActor(t, map[string]bool{"channel.push": true}, gw, `
function loop()
  local msg = session.inbox()
  local ok, err = pcall(session.push, "telegram", "123", "background update")
  session.send(tostring(ok))
end
`)
	a.Mailbox <- Message{ID: "1", Type: "user", From: "u", Text: "go", Channel: "webhook", ReplyTo: "r1"}

	waitFor(t, "push + report", 5*time.Second, func() bool { return len(gw.snapshot()) >= 2 })
	sends := gw.snapshot()
	if sends[0].channel != "telegram" || sends[0].replyTo != "123" || sends[0].text != "background update" {
		t.Fatalf("bad push: %+v", sends[0])
	}
	if sends[1].text != "true" {
		t.Fatalf("expected push to succeed, report says %q", sends[1].text)
	}
}

func TestSessionExitTerminates(t *testing.T) {
	gw := &fakeGW{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New("worker|test",
		Identity{SessionID: "worker|test", Agent: "worker", Capabilities: map[string]bool{}},
		&Info{Name: "worker", HistoryBudget: 100},
		gw, nil, nil, nil, nil, map[string]OpHandler{}, pool.New(1), log)
	a.LoopSrc = directive() + `function loop() session.exit() end`

	exits := make(chan EndReason, 4)
	a.OnExit = func(id Identity, reason EndReason) { exits <- reason }

	// Deliberately no cancel: if Run restarted the loop instead of honoring
	// session.exit, OnExit would never fire and the wait below would time out.
	go a.Run(context.Background())

	select {
	case reason := <-exits:
		if reason != EndTerminated {
			t.Fatalf("expected EndTerminated, got %q", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("actor did not terminate after session.exit")
	}
	select {
	case <-exits:
		t.Fatal("OnExit fired more than once")
	default:
	}
}

func TestAgentInfoIncludesIdentity(t *testing.T) {
	gw := &fakeGW{}
	a, _ := newTestActor(t, map[string]bool{}, gw, `
function loop()
  local msg = session.inbox()
  local info = agent.info()
  session.send(info.address .. "|" .. info.session_id)
end
`)
	a.Mailbox <- Message{ID: "1", Type: "user", From: "u", Text: "go", Channel: "webhook", ReplyTo: "r1"}

	waitFor(t, "info report", 5*time.Second, func() bool { return len(gw.snapshot()) > 0 })
	got := gw.snapshot()[0].text
	want := "session:main|test|main|test"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

// fakeUsers is a UserResolver stub for push_user tests.
type fakeUsers struct {
	lookup map[string]struct{ channel, replyTo string }
}

func (f *fakeUsers) LookupUser(uuid string) (string, string, bool) {
	c, ok := f.lookup[uuid]
	if !ok {
		return "", "", false
	}
	return c.channel, c.replyTo, true
}

// newUserTestActor is newTestActor with an injected UserResolver.
func newUserTestActor(t *testing.T, caps map[string]bool, gw Gateway, users UserResolver, loopSrc string) (*Actor, context.CancelFunc) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New("main|test",
		Identity{SessionID: "main|test", Agent: "main", Capabilities: caps},
		&Info{Name: "main", HistoryBudget: 100},
		gw, nil, nil, users, nil, map[string]OpHandler{}, pool.New(1), log)
	a.LoopSrc = directive() + loopSrc
	ctx, cancel := context.WithCancel(context.Background())
	go a.Run(ctx)
	t.Cleanup(cancel)
	return a, cancel
}

func TestSessionPushUserResolvesAndDelivers(t *testing.T) {
	gw := &fakeGW{}
	users := &fakeUsers{lookup: map[string]struct{ channel, replyTo string }{
		"u_abc": {"telegram", "12345"},
	}}
	a, _ := newUserTestActor(t, map[string]bool{"channel.push": true}, gw, users, `
function loop()
  local msg = session.inbox()
  local ok, err = pcall(session.push_user, "u_abc", "reaching out")
  session.send(tostring(ok) .. ":" .. tostring(err))
end
`)
	a.Mailbox <- Message{ID: "1", Type: "user", From: "user:u_abc", Text: "go", Channel: "webhook", ReplyTo: "r1"}

	waitFor(t, "push + report", 5*time.Second, func() bool { return len(gw.snapshot()) >= 2 })
	sends := gw.snapshot()
	if sends[0].channel != "telegram" || sends[0].replyTo != "12345" || sends[0].text != "reaching out" {
		t.Fatalf("bad push_user: %+v", sends[0])
	}
	if sends[1].text != "true:true" {
		t.Fatalf("expected push to succeed (true:true), report says %q", sends[1].text)
	}
}

func TestSessionPushUserUnknownUUID(t *testing.T) {
	gw := &fakeGW{}
	users := &fakeUsers{lookup: map[string]struct{ channel, replyTo string }{}}
	a, _ := newUserTestActor(t, map[string]bool{"channel.push": true}, gw, users, `
function loop()
  local msg = session.inbox()
  local ok, err = pcall(session.push_user, "u_nope", "x")
  session.send(tostring(ok) .. ":" .. tostring(err))
end
`)
	a.Mailbox <- Message{ID: "1", Type: "user", From: "user:u_abc", Text: "go", Channel: "webhook", ReplyTo: "r1"}

	waitFor(t, "report", 5*time.Second, func() bool { return len(gw.snapshot()) > 0 })
	sends := gw.snapshot()
	if !strings.HasPrefix(sends[0].text, "false:") || !strings.Contains(sends[0].text, "unknown user") {
		t.Fatalf("expected unknown-user error, got %q", sends[0].text)
	}
}

func TestSessionPushUserRequiresCapability(t *testing.T) {
	gw := &fakeGW{}
	users := &fakeUsers{lookup: map[string]struct{ channel, replyTo string }{
		"u_abc": {"telegram", "12345"},
	}}
	// No channel.push capability.
	a, _ := newUserTestActor(t, map[string]bool{}, gw, users, `
function loop()
  local msg = session.inbox()
  local ok, err = pcall(session.push_user, "u_abc", "x")
  session.send(tostring(ok) .. ":" .. tostring(err))
end
`)
	a.Mailbox <- Message{ID: "1", Type: "user", From: "user:u_abc", Text: "go", Channel: "webhook", ReplyTo: "r1"}

	waitFor(t, "report", 5*time.Second, func() bool { return len(gw.snapshot()) > 0 })
	sends := gw.snapshot()
	if !strings.HasPrefix(sends[0].text, "false:") || !strings.Contains(sends[0].text, "channel.push") {
		t.Fatalf("expected capability denial, got %q", sends[0].text)
	}
}

func TestSessionPushUserRequiresIdentity(t *testing.T) {
	gw := &fakeGW{}
	// users is nil — identity layer disabled.
	a, _ := newUserTestActor(t, map[string]bool{"channel.push": true}, gw, nil, `
function loop()
  local msg = session.inbox()
  local ok, err = pcall(session.push_user, "u_abc", "x")
  session.send(tostring(ok) .. ":" .. tostring(err))
end
`)
	a.Mailbox <- Message{ID: "1", Type: "user", From: "user:u_abc", Text: "go", Channel: "webhook", ReplyTo: "r1"}

	waitFor(t, "report", 5*time.Second, func() bool { return len(gw.snapshot()) > 0 })
	sends := gw.snapshot()
	if !strings.HasPrefix(sends[0].text, "false:") || !strings.Contains(sends[0].text, "identity not enabled") {
		t.Fatalf("expected identity-not-enabled error, got %q", sends[0].text)
	}
}
