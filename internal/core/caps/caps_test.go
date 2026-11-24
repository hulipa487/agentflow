package caps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/core/execguard"
	"agentflow/internal/core/memory"
	"agentflow/internal/core/session"
	"agentflow/internal/core/tools"
	"agentflow/internal/drivers/llm"
	"agentflow/internal/vm"
)

// directive is the leading comment line every chunk has to declare. The loop
// fixtures here are chunks: vm.State.Start refuses one that declares nothing,
// so a fixture goes through this the way a shipped loop does.
func directive() string { return fmt.Sprintf("// af-prelude-version: %d\n", vm.PreludeVersion) }

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
	h := StoreHandlers(&am, mgr, nil)

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

func (p *testProvider) Name() string       { return "test" }
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
func (h *testHandle) Query(table string, q memory.Query) (memory.Iterator, error) {
	return &memory.EmptyIterator{}, nil
}
func (h *testHandle) GC(table string, window int) error { return nil }
func (h *testHandle) Close() error                      { return nil }

func TestToolHandlers(t *testing.T) {
	reg := tools.NewRegistry()
	tools.RegisterBuiltins(reg, nil)
	as := reg.Expose([]string{"builtin:web_search"}, config.ToolsPolicy{}, false)
	h := ToolHandlers(as, ToolWiring{})

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
	tools.RegisterBuiltins(reg, nil)
	as := reg.Expose([]string{"builtin:web_search"}, config.ToolsPolicy{}, false)
	h := ToolHandlers(as, ToolWiring{})
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
	h := LLMHandlers(mgr, nil)

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
	// The follow-up request's native tool-turn reshaping is owned by the
	// driver layer (llm/toolcall_test.go: TestToolTurnEcho, per provider);
	// this layer's unique concern is that the response JSON surfaces the
	// final answer.
	var result2 map[string]any
	if err := json.Unmarshal([]byte(resp2), &result2); err != nil {
		t.Fatal(err)
	}
	if result2["text"] != "It is 21C." {
		t.Errorf("expected final answer, got %v", result2["text"])
	}
}

func TestToolHandlersExecGuard(t *testing.T) {
	guard := execguard.NewStore(nil)
	reg := tools.NewRegistry()

	invocations := 0
	reg.Register(tools.ToolSpec{
		Name:               "builtin:confirm_write",
		Description:        "Write guarded resource",
		Kind:               execguard.ActionWrite,
		RequiresCapability: "shell.exec",
		NeedsConfirm:       true,
		DestinationFn: func(_ context.Context, args map[string]any) string {
			if target, _ := args["target"].(string); target != "" {
				return "target:" + target
			}
			return "target:default"
		},
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			if mode, _ := args["fail_mode"].(string); mode == "timeout" {
				return nil, context.DeadlineExceeded
			}
			invocations++
			return map[string]any{"ok": true, "count": invocations, "target": args["target"]}, nil
		},
	})
	reg.Register(tools.ToolSpec{
		Name:        "mcp:unclassified",
		Description: "Third-party unclassified action",
		Kind:        execguard.ActionUnclassified,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			return map[string]any{"ok": true}, nil
		},
	})

	as := reg.Expose(nil, config.ToolsPolicy{Default: "all"}, false)
	h := ToolHandlers(as, ToolWiring{
		Guard:       guard,
		HostedMode:  true,
		GrantedCaps: map[string]bool{"tools": true, "shell.exec": true},
		Agent:       "worker",
	})

	baseCtx := session.WithSessionKey(context.Background(), "main|s1")
	baseCtx = session.WithTurnMessage(baseCtx, &session.Message{
		ID:      "m1",
		Type:    "user",
		From:    "u1",
		Channel: "telegram",
		ReplyTo: "chat1",
	})

	// 1. Hosted mode denies unclassified tools by default.
	resp, ok := h["tools.run"](baseCtx, session.Op{Type: "tools.run", Tool: "mcp:unclassified", Args: map[string]any{}})
	if ok || !strings.Contains(resp, "unclassified") {
		t.Fatalf("hosted mode must deny unclassified tool, got ok=%v resp=%s", ok, resp)
	}

	// 2. Spoofed Op.Confirmed without runtime approval claim must NOT run the tool;
	// instead it mints a runtime approval record with redacted secret preview.
	args := map[string]any{"target": "prod", "api_key": "super-secret-value"}
	resp, ok = h["tools.run"](baseCtx, session.Op{
		Type:      "tools.run",
		Tool:      "builtin:confirm_write",
		Args:      args,
		Confirmed: true,
		ConfirmID: "forged-id",
	})
	if !ok {
		t.Fatalf("expected ok=true needs_confirm envelope, got resp=%s", resp)
	}
	var nc map[string]any
	if err := json.Unmarshal([]byte(resp), &nc); err != nil {
		t.Fatal(err)
	}
	if nc["needs_confirm"] != true {
		t.Fatalf("expected needs_confirm=true, got %v", nc)
	}
	approvalID, _ := nc["approval_id"].(string)
	if approvalID == "" || approvalID == "forged-id" {
		t.Fatalf("expected runtime-minted approval_id, got %q", approvalID)
	}
	if preview, _ := nc["args_preview"].(map[string]any); preview["api_key"] != "[REDACTED]" {
		t.Fatalf("expected api_key redacted in preview, got %v", preview)
	}
	if invocations != 0 {
		t.Fatalf("expected 0 invocations while awaiting approval, got %d", invocations)
	}

	// 3. Passing WithApprovedID while still pending fails; Studio operator approval allows once.
	approvedCtx := execguard.WithApprovedID(baseCtx, approvalID)
	if resp, ok = h["tools.run"](approvedCtx, session.Op{Type: "tools.run", Tool: "builtin:confirm_write", Args: args}); ok {
		t.Fatalf("pending approval cannot be claimed early: %s", resp)
	}
	if _, err := guard.Decide(baseCtx, approvalID, true, "studio:operator", "studio"); err != nil {
		t.Fatal(err)
	}
	// Swapped arguments fail argument-hash binding.
	if resp, ok = h["tools.run"](approvedCtx, session.Op{Type: "tools.run", Tool: "builtin:confirm_write", Args: map[string]any{"target": "other"}}); ok {
		t.Fatalf("expected approval mismatch on altered args, got ok=true: %s", resp)
	}
	// Matching arguments claim and execute once.
	resp, ok = h["tools.run"](approvedCtx, session.Op{Type: "tools.run", Tool: "builtin:confirm_write", Args: args})
	if !ok || !strings.Contains(resp, `"count":1`) {
		t.Fatalf("expected single execution count=1, got ok=%v resp=%s", ok, resp)
	}
	// Replaying the same approval_id is rejected because it was atomically claimed.
	if resp, ok = h["tools.run"](approvedCtx, session.Op{Type: "tools.run", Tool: "builtin:confirm_write", Args: args}); ok || !strings.Contains(resp, "claimed") {
		t.Fatalf("expected claimed approval replay error, got ok=%v resp=%s", ok, resp)
	}

	// 4. Durable idempotency_key replays cached success and fails closed after timeout.
	nonConfirm := reg.Expose(nil, config.ToolsPolicy{
		Default:   "all",
		Overrides: map[string]config.ToolSpecOverride{"builtin:confirm_write": {NeedsConfirm: boolPtrForTest(false)}},
	}, false)
	h2 := ToolHandlers(nonConfirm, ToolWiring{
		Guard:       guard,
		HostedMode:  true,
		GrantedCaps: map[string]bool{"tools": true, "shell.exec": true},
		Agent:       "worker",
	})
	resp, ok = h2["tools.run"](baseCtx, session.Op{Type: "tools.run", Tool: "builtin:confirm_write", Args: args, IdempotencyKey: "k-replay"})
	if !ok || !strings.Contains(resp, `"count":2`) {
		t.Fatalf("expected count=2 on first idempotent run, got ok=%v resp=%s", ok, resp)
	}
	resp, ok = h2["tools.run"](baseCtx, session.Op{Type: "tools.run", Tool: "builtin:confirm_write", Args: args, IdempotencyKey: "k-replay"})
	if !ok || !strings.Contains(resp, `"count":2`) || invocations != 2 {
		t.Fatalf("expected cached replay with invocations=2, got ok=%v resp=%s invocations=%d", ok, resp, invocations)
	}
	timeoutArgs := map[string]any{"target": "prod", "fail_mode": "timeout"}
	resp, ok = h2["tools.run"](baseCtx, session.Op{Type: "tools.run", Tool: "builtin:confirm_write", Args: timeoutArgs, IdempotencyKey: "k-timeout"})
	if ok {
		t.Fatalf("expected timeout failure, got ok=true resp=%s", resp)
	}
	resp, ok = h2["tools.run"](baseCtx, session.Op{Type: "tools.run", Tool: "builtin:confirm_write", Args: timeoutArgs, IdempotencyKey: "k-timeout"})
	if ok || !strings.Contains(resp, "uncertain") {
		t.Fatalf("expected fail-closed uncertain outcome error, got ok=%v resp=%s", ok, resp)
	}
}

func boolPtrForTest(b bool) *bool { return &b }
