package session

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/core/pool"
	"agentflow/internal/core/safety"
)

func TestDefaultSafetyInjectsDirectiveAndCapsSupportOffer(t *testing.T) {
	var (
		mu       sync.Mutex
		seenMsgs [][]ChatMessage
		traces   []TraceEvent
	)

	gw := &fakeGW{}
	a := New("main|sess-1",
		Identity{SessionID: "main|sess-1", Agent: "main", Capabilities: map[string]bool{}},
		&Info{Name: "main", Model: "test-model", HistoryBudget: 100},
		gw, nil, nil, nil, safety.New(safety.DefaultProfile()),
		map[string]OpHandler{
			"llm.chat": func(ctx context.Context, op Op) (string, bool) {
				mu.Lock()
				seenMsgs = append(seenMsgs, append([]ChatMessage(nil), op.Messages...))
				mu.Unlock()
				return `{"text":"Here is a calm next step."}`, true
			},
		},
		pool.New(1),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	a.Tracer = func(ev TraceEvent) {
		mu.Lock()
		traces = append(traces, ev)
		mu.Unlock()
	}
	a.LoopSrc = directive() + `function loop()
  while true do
    local m = session.inbox()
    local r = llm.chat({
      {role = "system", content = "You are helpful."},
      {role = "user", content = m.text or ""},
    })
    session.send(r.text or "")
  end
end`

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	// Turn 1: imminent message triggers SteadyDirective system injection and one SupportOffer append.
	a.Mailbox <- Message{ID: "t1", Type: "user", From: "u", Text: "help me right now", Channel: "webhook", ReplyTo: "r1"}
	waitFor(t, "turn 1 reply", 5*time.Second, func() bool { return len(gw.snapshot()) >= 1 })

	// Turn 2: second imminent message still gets SteadyDirective, but SupportOffer is capped (1 per session).
	a.Mailbox <- Message{ID: "t2", Type: "user", From: "u", Text: "help me immediately", Channel: "webhook", ReplyTo: "r2"}
	waitFor(t, "turn 2 reply", 5*time.Second, func() bool { return len(gw.snapshot()) >= 2 })

	// Turn 3: neutral message receives no SteadyDirective injection and no SupportOffer append.
	a.Mailbox <- Message{ID: "t3", Type: "user", From: "u", Text: "summarize Go interfaces", Channel: "webhook", ReplyTo: "r3"}
	waitFor(t, "turn 3 reply", 5*time.Second, func() bool { return len(gw.snapshot()) >= 3 })

	mu.Lock()
	msgsCopy := append([][]ChatMessage(nil), seenMsgs...)
	traceCopy := append([]TraceEvent(nil), traces...)
	mu.Unlock()

	if len(msgsCopy) != 3 {
		t.Fatalf("captured %d llm.chat calls, want 3", len(msgsCopy))
	}
	if len(msgsCopy[0]) != 3 || msgsCopy[0][0].Content != "You are helpful." || msgsCopy[0][1].Role != "system" || !strings.Contains(msgsCopy[0][1].Content, "calmly") {
		t.Fatalf("turn 1 messages missing injected safety directive after leading system prompt: %+v", msgsCopy[0])
	}
	if len(msgsCopy[1]) != 3 || msgsCopy[1][1].Role != "system" || !strings.Contains(msgsCopy[1][1].Content, "calmly") {
		t.Fatalf("turn 2 messages missing injected safety directive: %+v", msgsCopy[1])
	}
	if len(msgsCopy[2]) != 2 {
		t.Fatalf("turn 3 benign message should not receive safety directive injection: %+v", msgsCopy[2])
	}

	sends := gw.snapshot()
	if !strings.Contains(sends[0].text, "local support helpline") {
		t.Fatalf("turn 1 egress missing support offer: %q", sends[0].text)
	}
	if strings.Contains(sends[1].text, "local support helpline") {
		t.Fatalf("turn 2 egress should be capped, got %q", sends[1].text)
	}
	if strings.Contains(sends[2].text, "local support helpline") {
		t.Fatalf("turn 3 benign egress should not include support offer: %q", sends[2].text)
	}

	var ingressSafety, egressSafety bool
	for _, ev := range traceCopy {
		if ev.Kind == "safety" && ev.Op == "ingress" && ev.Turn == "t1" {
			ingressSafety = true
		}
		if ev.Kind == "safety" && ev.Op == "egress" && ev.Turn == "t1" && ev.Err == "support-offer-appended" {
			egressSafety = true
		}
	}
	if !ingressSafety || !egressSafety {
		t.Fatalf("expected safety ingress & egress trace events, got ingress=%v egress=%v (%+v)", ingressSafety, egressSafety, traceCopy)
	}
}
