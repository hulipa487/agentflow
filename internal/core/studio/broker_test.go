package studio

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"agentflow/internal/core/session"
)

type fakeRouter struct {
	mu        sync.Mutex
	onDeliver func(agent, key string, msg session.Message)
	busy      bool
	live      bool
	stopped   []string
}

func (f *fakeRouter) Deliver(agent, key string, msg session.Message) error {
	if f.onDeliver != nil {
		f.onDeliver(agent, key, msg)
	}
	return nil
}

func (f *fakeRouter) StopSession(id string) {
	f.mu.Lock()
	f.stopped = append(f.stopped, id)
	f.mu.Unlock()
}

func (f *fakeRouter) SessionBusy(string) (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.busy, f.live
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestChatStreamsOneTurn: a turn is turn.start → reply → turn.end, and the
// caller sees all three before Chat returns.
func TestChatStreamsOneTurn(t *testing.T) {
	var b *Broker
	r := &fakeRouter{live: true}
	b = NewBroker(r, nil, Config{ChatTimeout: 2 * time.Second}, discard())
	var saw [3]bool
	r.onDeliver = func(agent, key string, msg session.Message) {
		key2 := agent + "|" + key
		b.Trace(session.TraceEvent{SessionID: key2, Kind: "turn.start", Turn: msg.ID})
		_ = b.Deliver(msg.ReplyTo, "hello", nil)
		b.Trace(session.TraceEvent{SessionID: key2, Kind: "turn.end", Turn: msg.ID})
	}

	var events []string
	res, err := b.Chat(context.Background(), ChatRequest{Agent: "echo", Thread: "t1", Text: "hi"}, func(ev ChatEvent) {
		events = append(events, ev.Type)
		switch ev.Type {
		case "trace":
			if ev.Trace.Kind == "turn.start" {
				saw[0] = true
			}
			if ev.Trace.Kind == "turn.end" {
				saw[2] = true
			}
		case "reply":
			saw[1] = true
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !saw[0] || !saw[1] || !saw[2] {
		t.Fatalf("missing events: %v (%v)", saw, events)
	}
	if len(res.Replies) != 1 || res.Replies[0].Text != "hello" {
		t.Fatalf("replies = %+v", res.Replies)
	}
	if res.Thread != "t1" || res.Session != "echo|studio:t1" {
		t.Fatalf("result identity = %+v", res)
	}
}

// TestChatFinishesWhenTheLoopExits: a one-shot loop never parks on the inbox,
// so no turn.end arrives. The busy/live signal must still close the turn.
func TestChatFinishesWhenTheLoopExits(t *testing.T) {
	var b *Broker
	r := &fakeRouter{live: true}
	b = NewBroker(r, nil, Config{ChatTimeout: 5 * time.Second}, discard())
	r.onDeliver = func(agent, key string, msg session.Message) {
		key2 := agent + "|" + key
		b.Trace(session.TraceEvent{SessionID: key2, Kind: "turn.start", Turn: msg.ID})
		_ = b.Deliver(msg.ReplyTo, "done", nil)
		r.mu.Lock()
		r.live = false // the session exited
		r.mu.Unlock()
	}
	res, err := b.Chat(context.Background(), ChatRequest{Agent: "echo", Text: "hi"}, func(ChatEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	if res.TimedOut || len(res.Replies) != 1 {
		t.Fatalf("result = %+v, want one reply and no timeout", res)
	}
}

func TestChatTimesOut(t *testing.T) {
	var b *Broker
	r := &fakeRouter{live: true, busy: true}
	b = NewBroker(r, nil, Config{ChatTimeout: 150 * time.Millisecond}, discard())
	r.onDeliver = func(agent, key string, msg session.Message) {
		b.Trace(session.TraceEvent{SessionID: agent + "|" + key, Kind: "turn.start", Turn: msg.ID})
	}
	res, err := b.Chat(context.Background(), ChatRequest{Agent: "slow", Text: "hi"}, func(ChatEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Fatalf("result = %+v, want timed out", res)
	}
}

func TestChatRefusesEmptyAndUnknownUser(t *testing.T) {
	b := NewBroker(&fakeRouter{live: true}, nil, Config{}, discard())
	if _, err := b.Chat(context.Background(), ChatRequest{Agent: "a", Text: "  "}, func(ChatEvent) {}); err == nil {
		t.Fatal("empty text accepted")
	}
	b2 := NewBroker(&fakeRouter{live: true}, nil, Config{ValidateUser: func(string) bool { return false }}, discard())
	if _, err := b2.Chat(context.Background(), ChatRequest{Agent: "a", Text: "hi", ActAs: "nope"}, func(ChatEvent) {}); err == nil {
		t.Fatal("unknown act-as user accepted")
	}
}

func TestThreadOf(t *testing.T) {
	cases := map[string]string{
		"echo|studio:abc":    "abc",
		"echo|webhook:123":   "",
		"echo":               "",
		"a|b|studio:ignored": "",
	}
	for in, want := range cases {
		if got := ThreadOf(in); got != want {
			t.Errorf("ThreadOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRingIsBoundedAndSequenced(t *testing.T) {
	r := newRing(3)
	for i := 0; i < 5; i++ {
		r.push(session.TraceEvent{Kind: "op.end"})
	}
	got := r.snapshot()
	if len(got) != 3 {
		t.Fatalf("ring kept %d events, want 3", len(got))
	}
	if got[2].Seq != 5 {
		t.Fatalf("last seq = %d, want 5", got[2].Seq)
	}
}

func TestThreadsListsOnlyStudioSessions(t *testing.T) {
	b := NewBroker(&fakeRouter{live: true}, nil, Config{}, discard())
	b.Trace(session.TraceEvent{SessionID: "echo|studio:t1", Kind: "turn.start"})
	b.Trace(session.TraceEvent{SessionID: "echo|webhook:chat9", Kind: "turn.start"})
	threads := b.Threads()
	if len(threads) != 1 || threads[0].Thread != "t1" || threads[0].Agent != "echo" {
		t.Fatalf("threads = %+v", threads)
	}
}

func TestStopThreadUsesTheStudioSessionKey(t *testing.T) {
	r := &fakeRouter{live: true}
	b := NewBroker(r, nil, Config{}, discard())
	b.StopThread("echo", "t1")
	if len(r.stopped) != 1 || r.stopped[0] != "echo|studio:t1" {
		t.Fatalf("stopped = %v", r.stopped)
	}
}
