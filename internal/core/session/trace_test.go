package session

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"agentflow/internal/core/pool"
)

// TestTracerEmitsTurnOpAndReplyEvents: the observer sees the turn boundary,
// the ops inside it, and the egress — which is the whole contract the Studio
// timeline is built on. Payload capture is opt-in, so the same test also pins
// that text is absent by default.
func TestTracerEmitsTurnOpAndReplyEvents(t *testing.T) {
	run := func(t *testing.T, payloads bool) []TraceEvent {
		t.Helper()
		gw := &fakeGW{}
		a := New("main|test",
			Identity{SessionID: "main|test", Agent: "main", Capabilities: map[string]bool{}},
			&Info{Name: "main", HistoryBudget: 100},
			gw, nil, nil, nil, nil, map[string]OpHandler{}, pool.New(1),
			slog.New(slog.NewTextHandler(io.Discard, nil)))
		a.TracePayloads = payloads
		a.LoopSrc = directive() + `function loop()
  while true do
    local m = session.inbox()
    session.send("echo:" .. (m.text or ""))
  end
end`

		var mu sync.Mutex
		var events []TraceEvent
		a.Tracer = func(ev TraceEvent) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go a.Run(ctx)

		a.Mailbox <- Message{ID: "m1", Type: "user", From: "u", Text: "hi", Channel: "webhook", ReplyTo: "r1"}
		waitFor(t, "the echo reply", 5*time.Second, func() bool { return len(gw.snapshot()) > 0 })
		waitFor(t, "the turn to close", 5*time.Second, func() bool {
			mu.Lock()
			defer mu.Unlock()
			for _, ev := range events {
				if ev.Kind == "turn.end" && ev.Turn == "m1" {
					return true
				}
			}
			return false
		})
		mu.Lock()
		defer mu.Unlock()
		return append([]TraceEvent(nil), events...)
	}

	t.Run("metadata", func(t *testing.T) {
		events := run(t, false)
		var start, reply, end bool
		for _, ev := range events {
			switch {
			case ev.Kind == "turn.start" && ev.Turn == "m1":
				start = true
				if ev.Text != "" {
					t.Errorf("payload capture is off, but turn.start carried %q", ev.Text)
				}
			case ev.Kind == "reply" && ev.Turn == "m1":
				reply = true
				if !ev.OK || ev.Text != "" {
					t.Errorf("reply event = ok:%v text:%q, want ok and no text", ev.OK, ev.Text)
				}
			case ev.Kind == "turn.end" && ev.Turn == "m1":
				end = true
			}
		}
		if !start || !reply || !end {
			t.Fatalf("missing events: start=%v reply=%v end=%v (%+v)", start, reply, end, events)
		}
	})

	t.Run("payloads", func(t *testing.T) {
		events := run(t, true)
		var start, reply bool
		for _, ev := range events {
			switch {
			case ev.Kind == "turn.start" && ev.Turn == "m1":
				start = ev.Text == "hi"
			case ev.Kind == "reply" && ev.Turn == "m1":
				reply = ev.Text == "echo:hi"
			}
		}
		if !start || !reply {
			t.Fatalf("payload capture missing text: start=%v reply=%v (%+v)", start, reply, events)
		}
	})
}
