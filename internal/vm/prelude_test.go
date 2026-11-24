package vm

import "testing"

func TestJSONEmptyObjectAndArrayDistinction(t *testing.T) {
	st := New(1_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	if err := st.Eval("@assert", `
const assert = (c, m) => { if (!c) throw new Error(m || "assertion failed"); };
const s = json.encode({});
assert(s === "{}", "empty object encoded as " + String(s));
const e = json.encode([]);
assert(e === "[]", "empty array encoded as " + String(e));
const a = json.encode([1, 2, 3]);
assert(a === "[1,2,3]", "array encoded as " + String(a));
const o = json.encode({ a: 1 });
assert(o === '{"a":1}', "object encoded as " + String(o));
`); err != nil {
		t.Fatalf("json encode behavior wrong: %v", err)
	}
}
