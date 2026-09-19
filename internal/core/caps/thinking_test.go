package caps

import (
	"context"
	"io"
	"log/slog"
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
