package vm

import "testing"

// Empty tables must encode as objects, not arrays: every op payload table
// crosses the bridge into a Go map[string]any, and an empty [] fails cgo
// unmarshal. This locks the is_array behavior the prelude relies on.
func TestJSONEmptyTableEncodesAsObject(t *testing.T) {
	st := New(1_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	// Eval runs in the main thread and cannot yield, so no op calls here —
	// just pure json.encode assertions.
	if err := st.Eval("@assert", `
local s = json.encode({})
assert(s == "{}", "empty table encoded as " .. tostring(s))
local a = json.encode({1,2,3})
assert(a == "[1,2,3]", "array encoded as " .. tostring(a))
local o = json.encode({a=1})
assert(o == '{"a":1}', "object encoded as " .. tostring(o))
`); err != nil {
		t.Fatalf("is_array/encode behavior wrong: %v", err)
	}
}
