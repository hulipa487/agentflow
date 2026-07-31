package caps

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/core/memory"
	"agentflow/internal/core/session"
	"agentflow/internal/core/tools"
	"agentflow/internal/drivers/llm"
)

func TestStoreHandlers(t *testing.T) {
	reg := memory.NewRegistry()
	reg.RegisterProvider(&testProvider{data: map[string]any{}})
	reg.AddBackend("test", "test", nil)
	ctx := context.Background()
	if err := reg.Open(ctx); err != nil {
		t.Fatal(err)
	}
	mgr := memory.NewManager(reg, slog.New(slog.NewTextHandler(nil, nil)))
	am := memory.AgentMemory{
		Tables: map[string]memory.StoreBinding{"t": {Backend: "test", Table: "t"}},
	}
	h := StoreHandlers(&am, mgr)

	op := session.Op{Type: "store.put", Table: "t", Key: "k1", Value: map[string]any{"x": 1}, TTL: 0}
	resp, ok := h["store.put"](context.Background(), op)
	if !ok || resp != "true" {
		t.Fatalf("put failed: %s ok=%v", resp, ok)
	}

	op = session.Op{Type: "store.get", Table: "t", Key: "k1"}
	resp, ok = h["store.get"](context.Background(), op)
	if !ok {
		t.Fatalf("get failed: %s", resp)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(resp), &got); err != nil {
		t.Fatal(err)
	}
	if !got["found"].(bool) {
		t.Fatalf("expected found true, got %v", got)
	}
}

type testProvider struct{ data map[string]any }

func (p *testProvider) Name() string { return "test" }
func (p *testProvider) Features() []string { return []string{"kv"} }
func (p *testProvider) Open(config map[string]any) (memory.BackendHandle, error) {
	return &testHandle{data: p.data}, nil
}

type testHandle struct{ data map[string]any }

func (h *testHandle) Put(table, key string, value any, opts memory.PutOpts) error {
	h.data[key] = value
	return nil
}
func (h *testHandle) Get(table, key string) (any, bool, error) {
	v, ok := h.data[key]
	return v, ok, nil
}
func (h *testHandle) Delete(table, key string) error { return nil }
func (h *testHandle) Query(table string, q memory.Query) (memory.Iterator, error) { return &memory.EmptyIterator{}, nil }
func (h *testHandle) GC(table string, window int) error { return nil }
func (h *testHandle) Close() error { return nil }

func TestToolHandlers(t *testing.T) {
	reg := tools.NewRegistry()
	tools.RegisterBuiltins(reg)
	as := reg.Expose([]string{"builtin:web_search"}, config.ToolsPolicy{}, false)
	h := ToolHandlers(as)

	resp, ok := h["tools.list"](context.Background(), session.Op{})
	if !ok {
		t.Fatalf("tools.list failed: %s", resp)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(resp), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(list))
	}

	resp, ok = h["tools.run"](context.Background(), session.Op{Tool: "builtin:web_search", Args: map[string]any{"query": "foo"}})
	if !ok {
		t.Fatalf("tools.run returned ok unexpectedly")
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		t.Fatal(err)
	}
	if result["ok"] != false || result["unavailable"] != true {
		t.Fatalf("expected unavailable, got %v", result)
	}
}

func TestForbiddenTool(t *testing.T) {
	reg := tools.NewRegistry()
	tools.RegisterBuiltins(reg)
	as := reg.Expose([]string{"builtin:web_search"}, config.ToolsPolicy{}, false)
	h := ToolHandlers(as)
	resp, ok := h["tools.run"](context.Background(), session.Op{Tool: "builtin:unknown", Args: map[string]any{}})
	if ok {
		t.Fatalf("expected run to fail")
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		t.Fatal(err)
	}
	if _, has := result["error"]; !has {
		t.Fatalf("expected error field, got %v", result)
	}
}

// TestLLMChatToolRoundTrip verifies the caps layer maps Op.Tools into provider
// tool defs, surfaces reply.tool_calls in the response, and echoes a tool turn
// (assistant tool_calls + tool result) back into the follow-up request.
func TestLLMChatToolRoundTrip(t *testing.T) {
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if len(bodies) == 1 {
			// First request (tools attached): return a tool call.
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]}}]}

data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}

data: [DONE]

`))
			return
		}
		// Follow-up (tool turn echoed): return the final answer.
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"It is 21C."}}]}

data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":20,"completion_tokens":6}}

data: [DONE]

`))
	}))
	defer srv.Close()

	mgr := llm.NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := LLMHandlers(mgr)

	tools := []session.ToolSpec{{
		Name:        "get_weather",
		Description: "Get weather",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
	}}
	resp, ok := h["llm.chat"](context.Background(), session.Op{
		Type:       "llm.chat",
		Messages:   []session.ChatMessage{{Role: "user", Content: "weather?"}},
		Tools:      tools,
		ToolChoice: "auto",
	})
	if !ok {
		t.Fatalf("llm.chat failed: %s", resp)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		t.Fatal(err)
	}
	calls, _ := result["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool_call in response, got %v", result["tool_calls"])
	}
	// Request 1 carried the tool defs + tool_choice.
	if !strings.Contains(string(bodies[0]), `"get_weather"`) || !strings.Contains(string(bodies[0]), `"tool_choice"`) {
		t.Errorf("first request missing tools/tool_choice: %s", bodies[0])
	}

	// Now simulate the loop's follow-up: echo the assistant tool_calls turn +
	// a tool result, and confirm the request reshapes them natively.
	resp2, ok := h["llm.chat"](context.Background(), session.Op{
		Type: "llm.chat",
		Messages: []session.ChatMessage{
			{Role: "user", Content: "weather?"},
			{Role: "assistant", ToolCalls: []session.ToolCallSpec{{ID: "call_1", Name: "get_weather", Args: map[string]any{"city": "Paris"}}}},
			{Role: "tool", ToolCallID: "call_1", ToolResult: map[string]any{"temp": "21C"}},
		},
		Tools: tools,
	})
	if !ok {
		t.Fatalf("follow-up llm.chat failed: %s", resp2)
	}
	body := string(bodies[1])
	for _, want := range []string{`"tool_calls"`, `"call_1"`, `"role":"tool"`, `"tool_call_id":"call_1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("follow-up request missing %q\nbody: %s", want, body)
		}
	}
	var result2 map[string]any
	if err := json.Unmarshal([]byte(resp2), &result2); err != nil {
		t.Fatal(err)
	}
	if result2["text"] != "It is 21C." {
		t.Errorf("expected final answer, got %v", result2["text"])
	}
}
