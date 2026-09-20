package caps

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"agentflow/internal/core/memory"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/volatile"
)

// scopedStoreTest builds StoreHandlers over one volatile store with a
// user-scoped binding, returning the handlers plus per-context wrappers.
func scopedStoreTest(t *testing.T) map[string]session.OpHandler {
	t.Helper()
	reg := memory.NewRegistry()
	reg.RegisterProvider(volatile.Provider{})
	reg.AddBackend("v", "volatile", nil)
	if err := reg.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr := memory.NewManager(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	am := memory.AgentMemory{
		Tables: map[string]memory.StoreBinding{
			"t": {Backend: "v", Table: "t", Scoping: "user"},
		},
	}
	return StoreHandlers(&am, mgr)
}

func putVal(t *testing.T, h map[string]session.OpHandler, ctx context.Context, key, val string) {
	t.Helper()
	resp, ok := h["store.put"](ctx, session.Op{Type: "store.put", Table: "t", Key: key, Value: val})
	if !ok || resp != "true" {
		t.Fatalf("store.put %s failed: %s ok=%v", key, resp, ok)
	}
}

func getVal(t *testing.T, h map[string]session.OpHandler, ctx context.Context, key string) (string, bool) {
	t.Helper()
	resp, ok := h["store.get"](ctx, session.Op{Type: "store.get", Table: "t", Key: key})
	if !ok {
		t.Fatalf("store.get %s failed: %s", key, resp)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(resp), &got); err != nil {
		t.Fatal(err)
	}
	s, _ := got["value"].(string)
	return s, got["found"].(bool)
}

func userCtx(uuid string) context.Context {
	return session.WithUserUUID(context.Background(), uuid)
}

func maintCtx() context.Context {
	return session.WithProvenanceKind(context.Background(), "scheduler")
}

// TestStoreScopeWire: the engine-enforced boundary end to end through the op
// handlers — two users isolated, service rows shared, maintenance reads all.
func TestStoreScopeWire(t *testing.T) {
	h := scopedStoreTest(t)
	u1, u2 := userCtx("u1"), userCtx("u2")

	putVal(t, h, u1, "dialogue:last", "A-private")
	putVal(t, h, u2, "dialogue:last", "B-private")

	if v, ok := getVal(t, h, u1, "dialogue:last"); !ok || v != "A-private" {
		t.Fatalf("u1 must read own row, got %q ok=%v", v, ok)
	}
	if v, ok := getVal(t, h, u2, "dialogue:last"); !ok || v != "B-private" {
		t.Fatalf("u2 must read own row (never u1's), got %q ok=%v", v, ok)
	}

	// A service-context write (agent hop: no user, no provenance) is readable
	// by both users but isolated per value above.
	svc := context.Background()
	putVal(t, h, svc, "kb:shared", "fleet")
	if v, ok := getVal(t, h, u1, "kb:shared"); !ok || v != "fleet" {
		t.Fatalf("u1 must read service rows, got %q ok=%v", v, ok)
	}

	// The maintenance context (engine-fired cron) reads every scope: both
	// users' private rows come back scope-visible in one query.
	resp, ok := h["store.query"](maintCtx(), session.Op{Type: "store.query", Query: memory.Query{Kind: "all", Table: "t"}})
	if !ok {
		t.Fatalf("maintenance query failed: %s", resp)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		t.Fatal(err)
	}
	recs := result["records"].([]any)
	seen := map[string]bool{}
	for _, r := range recs {
		m := r.(map[string]any)
		seen[m["key"].(string)] = true
	}
	for _, want := range []string{"user:u1|dialogue:last", "user:u2|dialogue:last", "service|kb:shared"} {
		if !seen[want] {
			t.Fatalf("maintenance query missing %q; saw %v", want, seen)
		}
	}
}

// TestStoreScopeInteractiveQueryStrips: interactive queries return clean keys
// and never surface another user's rows.
func TestStoreScopeInteractiveQueryStrips(t *testing.T) {
	h := scopedStoreTest(t)
	u1, u2 := userCtx("u1"), userCtx("u2")

	putVal(t, h, u1, "turn:a", "1")
	putVal(t, h, u2, "turn:b", "2")

	resp, ok := h["store.query"](u1, session.Op{Type: "store.query", Query: memory.Query{Kind: "prefix", Prefix: "turn:", Table: "t"}})
	if !ok {
		t.Fatalf("query failed: %s", resp)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		t.Fatal(err)
	}
	recs := result["records"].([]any)
	if len(recs) != 1 {
		t.Fatalf("u1 query must return exactly its own row, got %v", recs)
	}
	m := recs[0].(map[string]any)
	if m["key"] != "turn:a" {
		t.Fatalf("query keys must be scope-stripped, got %q", m["key"])
	}
}
