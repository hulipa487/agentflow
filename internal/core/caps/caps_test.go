package caps

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/core/memory"
	"agentflow/internal/core/session"
	"agentflow/internal/core/tools"
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
