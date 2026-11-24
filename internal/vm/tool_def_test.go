package vm

import (
	"strings"
	"testing"
)

func TestToolDefValidation(t *testing.T) {
	st := New(1_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	if err := st.Eval("@def", `tool.def({ name: "script:x", handler: (_args) => ({ ok: true }) });`); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	cases := []struct {
		name, src, want string
	}{
		{"no spec", `tool.def();`, "spec must be"},
		{"no name", `tool.def({ handler: () => {} });`, "spec.name"},
		{"empty name", `tool.def({ name: "", handler: () => {} });`, "spec.name"},
		{"no handler", `tool.def({ name: "script:y" });`, "spec.handler"},
		{"bad params", `tool.def({ name: "script:z", handler: () => {}, params: 42 });`, "spec.params"},
	}
	for _, tc := range cases {
		err := st.Eval("@def", tc.src)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: expected %q error, got %v", tc.name, tc.want, err)
		}
	}
}
