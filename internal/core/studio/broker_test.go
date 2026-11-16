package studio

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
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

// TestChatEndsWhenTheLoopFails: a loop that fails mid-turn sends no turn.end
// and no reply. The chat must end on the failure, with an error the caller
// can show, instead of running out the timeout and reporting "timed out".
func TestChatEndsWhenTheLoopFails(t *testing.T) {
	var b *Broker
	r := &fakeRouter{live: true}
	b = NewBroker(r, nil, Config{ChatTimeout: 30 * time.Second}, discard())
	r.onDeliver = func(agent, key string, msg session.Message) {
		sk := agent + "|" + key
		b.Trace(session.TraceEvent{SessionID: sk, Kind: "turn.start", Turn: msg.ID})
		// A reply sent before the failure still belongs to the turn.
		_ = b.Deliver(msg.ReplyTo, "partial", nil)
		b.Trace(session.TraceEvent{SessionID: sk, Kind: "error", Err: "attempt to index nil with 'text'"})
	}
	var events []string
	var errText string
	start := time.Now()
	res, err := b.Chat(context.Background(), ChatRequest{Agent: "a", Text: "hi"}, func(ev ChatEvent) {
		events = append(events, ev.Type)
		if ev.Type == "error" {
			errText = ev.Err
		}
	})
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("chat took %s: it waited for the timeout", took)
	}
	var loopErr *LoopError
	if !errors.As(err, &loopErr) || loopErr.Reason != "attempt to index nil with 'text'" {
		t.Fatalf("err = %v, want a LoopError carrying the loop's message", err)
	}
	if res.TimedOut {
		t.Fatal("a failed loop was reported as a timeout")
	}
	if len(res.Replies) != 1 || res.Replies[0].Text != "partial" {
		t.Fatalf("replies = %+v, want the one sent before the failure", res.Replies)
	}
	// The error is the last word: the reply is flushed before it, and no
	// "done" follows. (Replies and trace events travel on separate channels,
	// so their order relative to each other is not fixed.)
	if len(events) != 4 || events[len(events)-1] != "error" {
		t.Fatalf("events = %v, want two traces and a reply, then the error", events)
	}
	for _, ev := range events {
		if ev == "done" {
			t.Fatalf("events = %v: a failed turn was also reported done", events)
		}
	}
	if !strings.Contains(errText, "attempt to index nil") {
		t.Fatalf("error event = %q", errText)
	}
}

// TestChatOutlivesAnEarlierTurnsFailure: a failure traced before this turn
// started belongs to an earlier message. The loop restarts and then answers
// ours, so the chat must keep waiting.
func TestChatOutlivesAnEarlierTurnsFailure(t *testing.T) {
	var b *Broker
	r := &fakeRouter{live: true}
	b = NewBroker(r, nil, Config{ChatTimeout: 5 * time.Second}, discard())
	r.onDeliver = func(agent, key string, msg session.Message) {
		sk := agent + "|" + key
		b.Trace(session.TraceEvent{SessionID: sk, Kind: "error", Err: "left over from the previous turn"})
		b.Trace(session.TraceEvent{SessionID: sk, Kind: "turn.start", Turn: msg.ID})
		_ = b.Deliver(msg.ReplyTo, "hello", nil)
		b.Trace(session.TraceEvent{SessionID: sk, Kind: "turn.end", Turn: msg.ID})
	}
	res, err := b.Chat(context.Background(), ChatRequest{Agent: "a", Text: "hi"}, func(ChatEvent) {})
	if err != nil {
		t.Fatalf("an earlier turn's failure ended this one: %v", err)
	}
	if len(res.Replies) != 1 || res.TimedOut {
		t.Fatalf("result = %+v, want one reply", res)
	}
}

// TestChatEndsWhenSafetySkipsTheTurn: a classifier block skips the turn
// outright (the refusal is sent, then the session waits for the next
// message), so neither turn.start nor turn.end is traced.
func TestChatEndsWhenSafetySkipsTheTurn(t *testing.T) {
	var b *Broker
	r := &fakeRouter{live: true} // not busy: parked on the inbox again
	b = NewBroker(r, nil, Config{ChatTimeout: 30 * time.Second}, discard())
	r.onDeliver = func(agent, key string, msg session.Message) {
		sk := agent + "|" + key
		b.Trace(session.TraceEvent{SessionID: sk, Kind: "safety", Op: "ingress", Turn: msg.ID, Err: "classifier:block"})
		_ = b.Deliver(msg.ReplyTo, "I can't help with that.", nil)
	}
	start := time.Now()
	res, err := b.Chat(context.Background(), ChatRequest{Agent: "a", Text: "hi"}, func(ChatEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("chat took %s: it waited for the timeout", took)
	}
	if res.TimedOut || len(res.Replies) != 1 {
		t.Fatalf("result = %+v, want the refusal and no timeout", res)
	}
}

// TestChatWaitsThroughALegacySafetyDrop: a non-classifier drop empties the
// message but still runs the turn. The session is busy from dequeue to
// turn.end, so the refusal trace alone must not end the chat early.
func TestChatWaitsThroughALegacySafetyDrop(t *testing.T) {
	var b *Broker
	r := &fakeRouter{live: true, busy: true}
	b = NewBroker(r, nil, Config{ChatTimeout: 5 * time.Second}, discard())
	r.onDeliver = func(agent, key string, msg session.Message) {
		sk := agent + "|" + key
		b.Trace(session.TraceEvent{SessionID: sk, Kind: "safety", Op: "ingress", Turn: msg.ID, Err: "signal-gate"})
		go func() {
			// Longer than the broker's poll tick: an early finish would
			// return before any of this is traced.
			time.Sleep(350 * time.Millisecond)
			b.Trace(session.TraceEvent{SessionID: sk, Kind: "turn.start", Turn: msg.ID})
			_ = b.Deliver(msg.ReplyTo, "late", nil)
			b.Trace(session.TraceEvent{SessionID: sk, Kind: "turn.end", Turn: msg.ID})
		}()
	}
	res, err := b.Chat(context.Background(), ChatRequest{Agent: "a", Text: "hi"}, func(ChatEvent) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Replies) != 1 || res.Replies[0].Text != "late" {
		t.Fatalf("result = %+v: the chat ended before the dropped turn ran", res)
	}
}

// TestCheckIsWhatChatRefuses: Check and Chat agree, so a caller that checked
// first never sees Chat refuse the same request.
func TestCheckIsWhatChatRefuses(t *testing.T) {
	b := NewBroker(&fakeRouter{live: true}, nil, Config{ValidateUser: func(id string) bool { return id == "u_known" }}, discard())
	if err := b.Check(ChatRequest{Agent: "a", Text: " "}); !errors.Is(err, ErrEmptyText) {
		t.Fatalf("empty text: %v", err)
	}
	if err := b.Check(ChatRequest{Agent: "a", Text: "hi", ActAs: "u_gone"}); err == nil || !strings.Contains(err.Error(), "u_gone") {
		t.Fatalf("unknown act-as: %v", err)
	}
	if err := b.Check(ChatRequest{Agent: "a", Text: "hi", ActAs: " u_known "}); err != nil {
		t.Fatalf("known act-as refused: %v", err)
	}
	if err := b.Check(ChatRequest{Agent: "a", Text: "hi"}); err != nil {
		t.Fatalf("tenant-less turn refused: %v", err)
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
