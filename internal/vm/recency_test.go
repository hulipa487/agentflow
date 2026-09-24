package vm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestRecencyTimeRecallUsesTheBackendsKind: the default recall handler's "time"
// mode forwarded its window but left `kind` as "time", and every backend
// switches on "time_range" — so the one documented recall mode that takes a
// window errored with `unsupported query kind "time"` on all of them.
//
// The handler asks agent.info() for the store list before it builds the query,
// so the test answers that op and then reads the query it produces.
func TestRecencyTimeRecallUsesTheBackendsKind(t *testing.T) {
	code, err := os.ReadFile(filepath.Join("..", "builtins", "lua", "recency.lua"))
	if err != nil {
		t.Fatalf("read recency.lua: %v", err)
	}
	st := New(1_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	if err := st.Eval("@recency", string(code)); err != nil {
		t.Fatal(err)
	}

	status, msg := st.Start("loop", `return memory_recall_handler({
  kind = "time", store = "dialogue",
  from = "2026-01-01T00:00:00Z", to = "2026-02-01T00:00:00Z" })`)
	if status != Yielded {
		t.Fatalf("expected agent.info to yield, got %v: %s", status, msg)
	}
	var first struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(msg), &first); err != nil || first.Type != "agent.info" {
		t.Fatalf("first op = %s (%v); want agent.info", msg, err)
	}

	status, msg = st.Resume(`{"ok":true,"memory":{"stores":{"dialogue":{"backend":"b","table":"t"}}}}`, true)
	if status != Yielded {
		t.Fatalf("expected the query to yield, got %v: %s", status, msg)
	}
	var op struct {
		Type  string `json:"type"`
		Query struct {
			Kind string `json:"kind"`
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"query"`
	}
	if err := json.Unmarshal([]byte(msg), &op); err != nil {
		t.Fatalf("op payload is not JSON (%q): %v", msg, err)
	}
	if op.Type != "store.query" {
		t.Fatalf("op type = %q; want store.query", op.Type)
	}
	if op.Query.Kind != "time_range" {
		t.Fatalf("query.kind = %q; the backends accept \"time_range\", not \"time\"", op.Query.Kind)
	}
	if op.Query.From == "" || op.Query.To == "" {
		t.Fatalf("the window was not forwarded: %s", msg)
	}
}
