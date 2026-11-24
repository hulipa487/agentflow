package session

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"agentflow/internal/core/pool"
	"agentflow/internal/core/safety"
)

// allowFilter is a filter that changes nothing but makes the profile able to
// drop, which is what D9 keys on: any non-empty profile is treated as able to
// veto an answer.
type allowFilter struct{}

func (allowFilter) Name() string { return "allow" }
func (allowFilter) Ingress(_ context.Context, in safety.IngressInput) safety.IngressResult {
	return safety.IngressResult{Text: in.Message}
}
func (allowFilter) Egress(_ context.Context, in safety.EgressInput) safety.EgressResult {
	return safety.EgressResult{Text: in.Text}
}

func newSafetyActor(t *testing.T, gw Gateway, proc *safety.Dispatcher, loopSrc string) (*Actor, context.CancelFunc) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New("main|test",
		Identity{SessionID: "main|test", Agent: "main"},
		&Info{Name: "main", HistoryBudget: 100},
		gw, nil, nil, nil, proc, map[string]OpHandler{}, pool.New(2), log)
	a.LoopSrc = directive() + loopSrc
	ctx, cancel := context.WithCancel(context.Background())
	go a.Run(ctx)
	t.Cleanup(cancel)
	return a, cancel
}

// TestReplyTwiceIsAnError: the channel has one answer to send; a second reply
// reaches Lua as a normal op error, which a loop can pcall.
func TestReplyTwiceIsAnError(t *testing.T) {
	gw := &fakeGW{}
	a, _ := newSafetyActor(t, gw, safety.New(safety.None), `
function loop() {
  session.inbox();
  session.reply({ text: "one", finish_reason: "stop" });
  try {
    session.reply({ text: "two" });
    session.send("true|");
  } catch (e) {
    session.send("false|" + (e instanceof Error ? e.message : String(e)));
  }
  while (true) session.inbox();
}
`)
	a.Mailbox <- Message{ID: "m1", Type: "user", From: "u", Text: "go", Channel: "test", ReplyTo: "r1"}
	waitFor(t, "the structured reply", 5*time.Second, func() bool { return len(gw.repliesSnapshot()) == 1 })
	waitFor(t, "the report", 5*time.Second, func() bool { return len(gw.snapshot()) >= 1 })
	if got := gw.repliesSnapshot()[0].Text; got != "one" {
		t.Fatalf("reply text = %q; want one", got)
	}
	sends := gw.snapshot()
	last := sends[len(sends)-1].text
	if !strings.HasPrefix(last, "false|") || !strings.Contains(last, "already sent") {
		t.Fatalf("second reply report = %q; want a false|already-sent error", last)
	}
}

// TestDeltasHeldWhenSafetyCanDrop: with a profile that can veto the answer, the
// fragments are accumulated and released as one checked text at end of turn.
func TestDeltasHeldWhenSafetyCanDrop(t *testing.T) {
	gw := &fakeGW{}
	disp := safety.New(&safety.Profile{Name: "test", Filters: []safety.Filter{allowFilter{}}})
	a, _ := newSafetyActor(t, gw, disp, `
function loop() {
  session.inbox();
  session.send_delta("Hello ");
  session.send_delta("world");
  while (true) session.inbox();
}
`)
	a.Mailbox <- Message{ID: "m1", Type: "user", From: "u", Text: "go", Channel: "test", ReplyTo: "r1"}
	waitFor(t, "the released delta", 5*time.Second, func() bool { return len(gw.deltasSnapshot()) == 1 })
	if got := gw.deltasSnapshot()[0]; got != "Hello world" {
		t.Fatalf("released text = %q; want the whole checked answer", got)
	}
	if n := len(gw.turnsSnapshot()); n != 1 {
		t.Fatalf("EndTurn calls = %d; want 1", n)
	}
}

// TestDeltasRelayLiveWithNoSafety: the default (no profile that can drop)
// relays each fragment as it arrives.
func TestDeltasRelayLiveWithNoSafety(t *testing.T) {
	gw := &fakeGW{}
	a, _ := newSafetyActor(t, gw, safety.New(safety.None), `
function loop() {
  session.inbox();
  session.send_delta("Hello ");
  session.send_delta("world");
  while (true) session.inbox();
}
`)
	a.Mailbox <- Message{ID: "m1", Type: "user", From: "u", Text: "go", Channel: "test", ReplyTo: "r1"}
	waitFor(t, "two live deltas", 5*time.Second, func() bool { return len(gw.deltasSnapshot()) == 2 })
	got := gw.deltasSnapshot()
	if got[0] != "Hello " || got[1] != "world" {
		t.Fatalf("live deltas = %v", got)
	}
}

// TestEndTurnOnNormalEndCrashAndCancel covers the three end-of-turn paths the
// channel contract names: parking on the inbox, a script crash mid-turn, and a
// cancellation mid-turn.
func TestEndTurnOnNormalEndCrashAndCancel(t *testing.T) {
	t.Run("normal", func(t *testing.T) {
		gw := &fakeGW{}
		a, _ := newSafetyActor(t, gw, safety.New(safety.None), `
function loop() {
  while (true) {
    const m = session.inbox();
    session.send(m.text);
  }
}
`)
		a.Mailbox <- Message{ID: "m1", Type: "user", From: "u", Text: "go", Channel: "test", ReplyTo: "r1"}
		waitFor(t, "EndTurn", 5*time.Second, func() bool { return len(gw.turnsSnapshot()) == 1 })
		if err := gw.turnsSnapshot()[0].err; err != nil {
			t.Fatalf("EndTurn error = %v; want nil", err)
		}
	})

	t.Run("crash", func(t *testing.T) {
		gw := &fakeGW{}
		a, _ := newSafetyActor(t, gw, safety.New(safety.None), `
function loop() {
  session.inbox();
  throw new Error("boom");
}
`)
		a.Mailbox <- Message{ID: "m1", Type: "user", From: "u", Text: "go", Channel: "test", ReplyTo: "r1"}
		waitFor(t, "EndTurn", 5*time.Second, func() bool { return len(gw.turnsSnapshot()) == 1 })
		if err := gw.turnsSnapshot()[0].err; err == nil {
			t.Fatal("a crashed turn must report an error")
		}
	})

	t.Run("cancel", func(t *testing.T) {
		gw := &fakeGW{}
		a, cancel := newSafetyActor(t, gw, safety.New(safety.None), `
function loop() {
  session.inbox();
  time.sleep(5);
  session.send("late");
}
`)
		a.Mailbox <- Message{ID: "m1", Type: "user", From: "u", Text: "go", Channel: "test", ReplyTo: "r1"}
		waitFor(t, "the turn to start", 5*time.Second, func() bool { return a.Busy() })
		cancel()
		waitFor(t, "the interrupted EndTurn", 5*time.Second, func() bool { return len(gw.turnsSnapshot()) == 1 })
		if err := gw.turnsSnapshot()[0].err; err == nil {
			t.Fatal("a cancelled turn must report an error")
		}
	})
}

// TestARetriedMessageIdStillEndsItsTurn: a retried request is the same message
// (same id) served as a new turn. End-of-turn is single-shot per turn, not per
// message id, or the second turn would never be closed and its caller would
// wait out its timeout.
func TestARetriedMessageIdStillEndsItsTurn(t *testing.T) {
	gw := &fakeGW{}
	a, _ := newSafetyActor(t, gw, safety.New(safety.None), `
function loop() {
  while (true) {
    const m = session.inbox();
    session.send(m.text);
  }
}
`)
	a.Mailbox <- Message{ID: "msg-same", Type: "user", From: "u", Text: "one", Channel: "test", ReplyTo: "r1"}
	waitFor(t, "the first EndTurn", 5*time.Second, func() bool { return len(gw.turnsSnapshot()) == 1 })
	a.Mailbox <- Message{ID: "msg-same", Type: "user", From: "u", Text: "two", Channel: "test", ReplyTo: "r2"}
	waitFor(t, "the second EndTurn", 5*time.Second, func() bool { return len(gw.turnsSnapshot()) == 2 })
}
