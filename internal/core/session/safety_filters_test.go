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

type routeTestClassifier struct{}

func (routeTestClassifier) Classify(_ context.Context, req safety.ClassifyRequest) (safety.Verdict, error) {
	if strings.Contains(req.Text, "complex") {
		return safety.Verdict{Label: "complex", Score: 0.95, Model: "guard"}, nil
	}
	return safety.Verdict{Label: "safe", Score: 0.99, Model: "guard"}, nil
}

func TestSafetyRedirectClassifierAndLuaFilterActor(t *testing.T) {
	lf, err := safety.NewLuaFilter(safety.LuaFilterOptions{
		Name:    "legal_router",
		Ingress: true,
		Source: `
function ingress(input)
  if string.find(string.lower(input.message), "legal", 1, true) then
    return { redirect = "Legal-Pro", directive = "Cite statutes." }
  end
  return nil
end
`,
	})
	if err != nil {
		t.Fatalf("NewLuaFilter: %v", err)
	}
	clf := safety.NewClassifierFilter(safety.ClassifierOptions{
		Name:       "complexity",
		Classifier: routeTestClassifier{},
		Ingress:    true,
		Egress:     false,
		Threshold:  0.7,
		Categories: map[string]string{"safe": "allow", "complex": "redirect:Strong-Model"},
		Timeout:    time.Second,
		OnError:    "block",
	})

	type callRec struct {
		op         string
		opModel    string
		agentModel string
		msgs       []ChatMessage
	}
	var (
		mu         sync.Mutex
		calls      []callRec
		traces     []TraceEvent
		streamStep int
	)

	gw := &fakeGW{}
	a := New("main|redirect-1",
		Identity{SessionID: "main|redirect-1", Agent: "main", Capabilities: map[string]bool{}},
		&Info{Name: "main", Model: "default-fast", HistoryBudget: 100},
		gw, nil, nil, nil,
		safety.New(&safety.Profile{Name: "routing", Filters: []safety.Filter{clf, lf}}),
		map[string]OpHandler{
			"llm.chat": func(ctx context.Context, op Op) (string, bool) {
				mu.Lock()
				calls = append(calls, callRec{
					op:         op.Type,
					opModel:    op.Model,
					agentModel: AgentModelFromCtx(ctx),
					msgs:       append([]ChatMessage(nil), op.Messages...),
				})
				mu.Unlock()
				return `{"text":"chat-ok"}`, true
			},
			"llm.stream.open": func(ctx context.Context, op Op) (string, bool) {
				mu.Lock()
				calls = append(calls, callRec{
					op:         op.Type,
					opModel:    op.Model,
					agentModel: AgentModelFromCtx(ctx),
					msgs:       append([]ChatMessage(nil), op.Messages...),
				})
				streamStep = 0
				mu.Unlock()
				return `{"id":"s1"}`, true
			},
			"llm.stream.next": func(ctx context.Context, op Op) (string, bool) {
				mu.Lock()
				defer mu.Unlock()
				if streamStep == 0 {
					streamStep++
					return `{"delta":"stream-ok","done":false}`, true
				}
				return `{"done":true}`, true
			},
			"llm.stream.close": func(ctx context.Context, op Op) (string, bool) {
				return `true`, true
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
    local info = agent.info()
    local msgs = {{role = "user", content = m.text or ""}}
    if string.find(m.text or "", "legal", 1, true) then
      local next_delta = llm.stream(msgs, { model = "loop-fast" })
      local out = ""
      while true do
        local d = next_delta()
        if not d then break end
        out = out .. d
      end
      session.send((info.model or "") .. "|" .. out)
    else
      local r1 = llm.chat(msgs)
      local r2 = llm.chat(msgs, { model = "loop-explicit" })
      session.send((info.model or "") .. "|" .. (info.agent_model or "") .. "|" .. (r1.text or "") .. "|" .. (r2.text or ""))
    end
  end
end`

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	// Turn 1: classifier redirects both default and explicit llm.chat calls to Strong-Model.
	a.Mailbox <- Message{ID: "t1", Type: "user", From: "u", Text: "solve complex equation", Channel: "webhook", ReplyTo: "r1"}
	waitFor(t, "turn 1 reply", 5*time.Second, func() bool { return len(gw.snapshot()) >= 1 })

	// Turn 2: Lua filter redirects llm.stream.open to Legal-Pro and injects directive.
	a.Mailbox <- Message{ID: "t2", Type: "user", From: "u", Text: "review legal filing", Channel: "webhook", ReplyTo: "r2"}
	waitFor(t, "turn 2 reply", 5*time.Second, func() bool { return len(gw.snapshot()) >= 2 })

	// Turn 3: benign message clears redirect; implicit chat falls back to default-fast, explicit uses loop-explicit.
	a.Mailbox <- Message{ID: "t3", Type: "user", From: "u", Text: "hello world", Channel: "webhook", ReplyTo: "r3"}
	waitFor(t, "turn 3 reply", 5*time.Second, func() bool { return len(gw.snapshot()) >= 3 })

	mu.Lock()
	gotCalls := append([]callRec(nil), calls...)
	gotTraces := append([]TraceEvent(nil), traces...)
	mu.Unlock()
	sends := gw.snapshot()

	if sends[0].text != "Strong-Model|default-fast|chat-ok|chat-ok" {
		t.Fatalf("turn 1 info mismatch: %q", sends[0].text)
	}
	if sends[1].text != "Legal-Pro|stream-ok" {
		t.Fatalf("turn 2 info mismatch: %q", sends[1].text)
	}
	if sends[2].text != "default-fast||chat-ok|chat-ok" {
		t.Fatalf("turn 3 info mismatch (redirect must clear after turn): %q", sends[2].text)
	}

	if len(gotCalls) != 5 {
		t.Fatalf("got %d LLM calls, want 5: %+v", len(gotCalls), gotCalls)
	}
	// Turn 1: both llm.chat calls forced to Strong-Model.
	for i := 0; i < 2; i++ {
		if gotCalls[i].opModel != "Strong-Model" || gotCalls[i].agentModel != "Strong-Model" {
			t.Fatalf("turn 1 call %d = %+v; want opModel=Strong-Model agentModel=Strong-Model", i, gotCalls[i])
		}
	}
	// Turn 2: llm.stream.open forced to Legal-Pro + safety directive injected.
	if gotCalls[2].op != "llm.stream.open" || gotCalls[2].opModel != "Legal-Pro" || gotCalls[2].agentModel != "Legal-Pro" {
		t.Fatalf("turn 2 stream call = %+v; want Legal-Pro", gotCalls[2])
	}
	if len(gotCalls[2].msgs) != 2 || gotCalls[2].msgs[0].Role != "system" || !strings.Contains(gotCalls[2].msgs[0].Content, "Cite statutes.") {
		t.Fatalf("turn 2 stream missing injected directive: %+v", gotCalls[2].msgs)
	}
	// Turn 3: redirect cleared; implicit llm.chat carries AgentModelFromCtx="default-fast", explicit keeps opModel="loop-explicit".
	if gotCalls[3].opModel != "" || gotCalls[3].agentModel != "default-fast" {
		t.Fatalf("turn 3 implicit call = %+v; want opModel=\"\" agentModel=default-fast", gotCalls[3])
	}
	if gotCalls[4].opModel != "loop-explicit" || gotCalls[4].agentModel != "default-fast" {
		t.Fatalf("turn 3 explicit call = %+v; want opModel=loop-explicit agentModel=default-fast", gotCalls[4])
	}

	var t1SafetyRedirect, t1OpModel, t2SafetyRedirect, t2OpModel bool
	for _, ev := range gotTraces {
		if ev.Turn == "t1" && ev.Kind == "safety" && ev.Op == "ingress" && ev.Safety != nil && ev.Safety.RedirectModel == "Strong-Model" {
			t1SafetyRedirect = true
		}
		if ev.Turn == "t1" && ev.Kind == "op.start" && ev.Op == "llm.chat" && ev.Model == "Strong-Model" {
			t1OpModel = true
		}
		if ev.Turn == "t2" && ev.Kind == "safety" && ev.Op == "ingress" && ev.Safety != nil && ev.Safety.RedirectModel == "Legal-Pro" {
			t2SafetyRedirect = true
		}
		if ev.Turn == "t2" && ev.Kind == "op.start" && ev.Op == "llm.stream.open" && ev.Model == "Legal-Pro" {
			t2OpModel = true
		}
	}
	if !t1SafetyRedirect || !t1OpModel || !t2SafetyRedirect || !t2OpModel {
		t.Fatalf("missing redirect trace fields (t1Safety=%v t1Op=%v t2Safety=%v t2Op=%v): %+v",
			t1SafetyRedirect, t1OpModel, t2SafetyRedirect, t2OpModel, gotTraces)
	}
}
