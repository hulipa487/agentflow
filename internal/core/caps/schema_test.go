package caps

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/gateway"
	"agentflow/internal/core/media"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/session"
	"agentflow/internal/core/tools"
	"agentflow/internal/drivers/llm"
)

// TestLLMChatDropsMangledRequired: a tool schema whose `required` crossed Lua
// as an empty table decodes back as an object — invalid JSON Schema that
// strict providers (xAI) 400 on. The llm.chat boundary must drop it.
func TestLLMChatDropsMangledRequired(t *testing.T) {
	var bodies [][]byte
	srv := recordBodyServer(t, &bodies)

	mgr := llm.NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := LLMHandlers(mgr, nil)

	resp, ok := h["llm.chat"](context.Background(), session.Op{
		Type:     "llm.chat",
		Messages: []session.ChatMessage{{Role: "user", Content: "hi"}},
		Tools: []session.ToolSpec{{
			Name:        "mock_read",
			Description: "Read a document",
			// What a Go `"required": []string{}` becomes after a Lua round-trip.
			Parameters: map[string]any{"type": "object", "required": map[string]any{}},
		}},
	})
	if !ok {
		t.Fatalf("llm.chat failed: %s", resp)
	}
	body := bodies[0]
	if strings.Contains(string(body), `"required":{}`) || strings.Contains(string(body), `"required": {}`) {
		t.Fatalf("request emitted object-form required\n%s", body)
	}
	if strings.Contains(string(body), `"required"`) {
		t.Fatalf("empty required should be absent entirely\n%s", body)
	}
}

// schemaGW is a minimal session.Gateway for the VM round-trip test.
type schemaGW struct {
	mu    sync.Mutex
	sends []string
}

func (g *schemaGW) Send(channel, replyTo, text string, attachments []media.Part) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sends = append(g.sends, text)
	return nil
}

func (g *schemaGW) SendReply(channel, replyTo string, r gateway.Reply) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sends = append(g.sends, r.Text)
	return nil
}

func (g *schemaGW) SendDelta(channel, replyTo, text string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sends = append(g.sends, text)
	return nil
}

func (g *schemaGW) EndTurn(channel, replyTo string, err error) {}

// TestToolSchemaVMRoundTrip: register a tool whose schema carries
// `"required": []string{}`, list it through tools.list inside a real Luau VM,
// feed the defs into llm.chat, and assert the provider request never carries
// `"required": {}` — the round trip that 400'd strict providers.
func TestToolSchemaVMRoundTrip(t *testing.T) {
	var bodies [][]byte
	srv := recordBodyServer(t, &bodies)

	reg := tools.NewRegistry()
	reg.Register(tools.ToolSpec{
		Name:        "builtin:mock_read",
		Description: "Read a document",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
			"required":   []string{},
		},
		Invoke: func(ctx context.Context, args map[string]any) (any, error) { return nil, nil },
	})
	agentSet := reg.Expose([]string{"builtin:mock_read"}, config.ToolsPolicy{}, false)

	mgr := llm.NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	handlers := map[string]session.OpHandler{}
	for k, h := range ToolHandlers(agentSet, ToolWiring{}) {
		handlers[k] = h
	}
	for k, h := range LLMHandlers(mgr, nil) {
		handlers[k] = h
	}

	gw := &schemaGW{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := session.New("main|test",
		session.Identity{SessionID: "main|test", Agent: "main", Capabilities: map[string]bool{"llm.chat": true}},
		&session.Info{Name: "main", HistoryBudget: 100},
		gw, nil, nil, nil, nil, handlers, pool.New(1), log)
	a.LoopSrc = directive() + `
function loop() {
  session.inbox();
  const defs = tools.list();
  const ctools = [];
  for (const d of defs) {
    const f = d["function"];
    ctools.push({ name: f.name, description: f.description, parameters: f.parameters });
  }
  try {
    llm.chat([{ role: "user", content: "hi" }], { model: "default", tools: ctools });
    session.send("done");
  } catch (e) {
    session.send("err: " + (e instanceof Error ? e.message : String(e)));
  }
}
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
	body := bodies[0]
	if !strings.Contains(string(body), "mock_read") {
		t.Fatalf("provider request missing the tool\n%s", body)
	}
	if strings.Contains(string(body), `"required":{}`) || strings.Contains(string(body), `"required": {}`) {
		t.Fatalf("provider request emitted object-form required\n%s", body)
	}
}
