package vm

import (
	"strings"
	"testing"
)

// TestToolDefValidation: tool.def rejects malformed specs at declaration time.
// tool.def mutates only a prelude upvalue (no op crossing), so it runs on the
// main thread under plain Eval.
func TestToolDefValidation(t *testing.T) {
	st := New(1_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	if err := st.Eval("@def", `tool.def({ name = "lua:x", handler = function(args) return { ok = true } end })`); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	cases := []struct {
		name, src, want string
	}{
		{"no spec", `tool.def()`, "spec must be a table"},
		{"no name", `tool.def({ handler = function() end })`, "spec.name"},
		{"empty name", `tool.def({ name = "", handler = function() end })`, "spec.name"},
		{"no handler", `tool.def({ name = "lua:y" })`, "spec.handler"},
		{"bad params", `tool.def({ name = "lua:z", handler = function() end, params = 42 })`, "spec.params"},
	}
	for _, tc := range cases {
		err := st.Eval("@def", tc.src)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: expected %q error, got %v", tc.name, tc.want, err)
		}
	}
}
