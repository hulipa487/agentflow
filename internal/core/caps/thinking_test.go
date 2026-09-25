package caps

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/llm"
)

// TestLLMChatThinking: the thinking level on the op reaches the provider
// request, and an invalid level fails the call rather than being dropped.
func TestLLMChatThinking(t *testing.T) {
	var bodies [][]byte
	srv := recordBodyServer(t, &bodies)

	mgr := llm.NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := LLMHandlers(mgr, nil)

	resp, ok := h["llm.chat"](context.Background(), session.Op{
		Type:     "llm.chat",
		Messages: []session.ChatMessage{{Role: "user", Content: "hi"}},
		Thinking: "high",
	})
	if !ok {
		t.Fatalf("llm.chat failed: %s", resp)
	}
	if len(bodies) != 1 || !strings.Contains(string(bodies[0]), `"reasoning_effort":"high"`) {
		t.Fatalf("request missing the thinking level: %v", bodies)
	}

	bad, ok := h["llm.chat"](context.Background(), session.Op{
		Type:     "llm.chat",
		Messages: []session.ChatMessage{{Role: "user", Content: "hi"}},
		Thinking: "banana",
	})
	if ok {
		t.Fatalf("an invalid thinking level must fail the call, got success: %s", bad)
	}
	if !strings.Contains(bad, "invalid thinking level") {
		t.Fatalf("error should name the problem, got: %s", bad)
	}
}

// TestThinkingSurvivesTheLuaBridge: a loop passing thinking in llm.chat opts
// must have it land on the provider request — with_opts copies arbitrary opt
// keys into the op, and the op carries it across the bridge.
func TestThinkingSurvivesTheLuaBridge(t *testing.T) {
	var bodies [][]byte
	srv := recordBodyServer(t, &bodies)

	mgr := llm.NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handlers := map[string]session.OpHandler{}
	for k, h := range LLMHandlers(mgr, nil) {
		handlers[k] = h
	}

	gw := &schemaGW{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := session.New("main|test",
		session.Identity{SessionID: "main|test", Agent: "main", Capabilities: map[string]bool{"llm.chat": true}},
		&session.Info{Name: "main", HistoryBudget: 100},
		gw, nil, nil, nil, nil, handlers, pool.New(1), log)
	a.LoopSrc = `
function loop()
  local msg = session.inbox()
  local ok, reply = pcall(llm.chat, { { role = "user", content = "hi" } }, { thinking = "xhigh" })
  if ok then session.send("done") else session.send("err: " .. tostring(reply)) end
end
`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	a.Mailbox <- session.Message{ID: "m1", Type: "user", From: "u", Text: "go", Channel: "webhook", ReplyTo: "1"}

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
	if sends[0] != "done" {
		t.Fatalf("loop failed: %s", sends[0])
	}
	if len(bodies) != 1 || !strings.Contains(string(bodies[0]), `"reasoning_effort":"xhigh"`) {
		t.Fatalf("thinking did not survive the bridge: %v", bodies)
	}
}

// TestThinkingCaptureSurvivesTheLuaBridge: reasoning content the provider
// sends surfaces to the loop as reply.thinking — the audit surface. The stub
// answers with xAI-style reasoning_content on an openai-shaped stream.
func TestThinkingCaptureSurvivesTheLuaBridge(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"chain of thought\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\ndata: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	mgr := llm.NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handlers := map[string]session.OpHandler{}
	for k, h := range LLMHandlers(mgr, nil) {
		handlers[k] = h
	}

	gw := &schemaGW{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := session.New("main|test",
		session.Identity{SessionID: "main|test", Agent: "main", Capabilities: map[string]bool{"llm.chat": true}},
		&session.Info{Name: "main", HistoryBudget: 100},
		gw, nil, nil, nil, nil, handlers, pool.New(1), log)
	a.LoopSrc = `
function loop()
  local msg = session.inbox()
  local ok, reply = pcall(llm.chat, { { role = "user", content = "hi" } })
  if ok then session.send("T:" .. (reply.thinking or "NONE")) else session.send("err: " .. tostring(reply)) end
end
`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	a.Mailbox <- session.Message{ID: "m1", Type: "user", From: "u", Text: "go", Channel: "webhook", ReplyTo: "1"}

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
	if sends[0] != "T:chain of thought" {
		t.Fatalf("reply.thinking did not survive the bridge: %s", sends[0])
	}
}

// TestThinkingBlocksRoundTrip: a loop passes reply.thinking_blocks back on
// the assistant turn and the next provider request replays them verbatim,
// ahead of tool_use — the Anthropic-ready continuation shape, stateless
// across the bridge.
func TestThinkingBlocksRoundTrip(t *testing.T) {
	sse := `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"because"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig9"}}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"calc"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":9}}

event: message_stop
data: {"type":"message_stop"}

`
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()

	mgr := llm.NewManager(map[string]config.Model{
		"default": {Provider: "anthropic", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handlers := map[string]session.OpHandler{}
	for k, h := range LLMHandlers(mgr, nil) {
		handlers[k] = h
	}

	gw := &schemaGW{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := session.New("main|test",
		session.Identity{SessionID: "main|test", Agent: "main", Capabilities: map[string]bool{"llm.chat": true}},
		&session.Info{Name: "main", HistoryBudget: 100},
		gw, nil, nil, nil, nil, handlers, pool.New(1), log)
	a.LoopSrc = `
function loop()
  local msg = session.inbox()
  local ok1, r1 = pcall(llm.chat, { { role = "user", content = "q" } }, { thinking = "low" })
  if not ok1 then session.send("err1: " .. tostring(r1)) return end
  local ok2, r2 = pcall(llm.chat, {
    { role = "user", content = "q" },
    { role = "assistant", content = "", tool_calls = r1.tool_calls, thinking_blocks = r1.thinking_blocks },
    { role = "tool", tool_call_id = r1.tool_calls[1].id, content = "42" },
  }, { thinking = "low" })
  if ok2 then session.send("done2") else session.send("err2: " .. tostring(r2)) end
end
`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	a.Mailbox <- session.Message{ID: "m1", Type: "user", From: "u", Text: "go", Channel: "webhook", ReplyTo: "1"}

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
	if sends[0] != "done2" {
		t.Fatalf("round-trip loop failed: %s", sends[0])
	}
	if len(bodies) != 2 {
		t.Fatalf("expected 2 provider calls, got %d", len(bodies))
	}
	cont := string(bodies[1])
	// The tool result rides back as a content block rather than a bare JSON
	// string: the official SDK models tool_result content as blocks only and
	// has no string variant, and the Messages API accepts both forms. Its
	// llm-package twin asserts the same shape.
	for _, want := range []string{`"thinking":"because"`, `"signature":"sig9"`, `"tool_use_id":"tu_1"`, `"content":[{"text":"42","type":"text"}]`} {
		if !strings.Contains(cont, want) {
			t.Fatalf("continuation request missing %q\nbody: %s", want, cont)
		}
	}
	if i, j := strings.Index(cont, `"thinking":"because"`), strings.Index(cont, `"tool_use"`); i > j {
		t.Fatalf("thinking block must precede tool_use\nbody: %s", cont)
	}
}
