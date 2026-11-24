package vm

import (
	"os"
	"path/filepath"
	"testing"
)

func budgetScript(t *testing.T, body string) {
	t.Helper()
	code, err := os.ReadFile(filepath.Join("..", "builtins", "ts", "token_budget.ts"))
	if err != nil {
		t.Fatalf("read token_budget.ts: %v", err)
	}
	st := New(1_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	if err := st.Eval("@token_budget", string(code)); err != nil {
		t.Fatal(err)
	}
	const helpers = `
function assert(c, m) { if (!c) throw new Error(m || "assertion failed"); }
function big(n) { return "x".repeat(n); }
function call(id, args) { return { id: id, name: "lookup", arguments: args || "{}" }; }
function well_formed(msgs) {
  let i = 0;
  while (i < msgs.length) {
    const m = msgs[i];
    if (m.role === "tool") {
      throw new Error("message " + i + " is a tool result with no assistant turn asking for it (id " + String(m.tool_call_id) + ")");
    }
    if (m.role === "assistant" && Array.isArray(m.tool_calls) && m.tool_calls.length > 0) {
      const want = {};
      for (const c of m.tool_calls) want[c.id] = true;
      let j = i + 1;
      while (j < msgs.length && msgs[j] && msgs[j].role === "tool") {
        assert(want[msgs[j].tool_call_id], "tool result " + String(msgs[j].tool_call_id) + " follows a turn that did not ask for it");
        delete want[msgs[j].tool_call_id];
        j++;
      }
      for (const id of Object.keys(want)) {
        throw new Error("tool call " + id + " was kept without its result");
      }
      i = j;
    } else {
      i++;
    }
  }
}
function contents(msgs) {
  return msgs.map(m => m.label || m.content).join(",");
}
`
	if err := st.Eval("@assert", helpers+body); err != nil {
		t.Fatal(err)
	}
}

func TestTokenBudgetNeverSplitsAToolExchange(t *testing.T) {
	budgetScript(t, `
const msgs = [
  { role: "system", content: "sys" },
  { role: "user", content: big(400), label: "q1" },
  { role: "assistant", content: "", tool_calls: [call("a1"), call("a2")], label: "ask-a" },
  { role: "tool", tool_call_id: "a1", content: big(2000), label: "res-a1" },
  { role: "tool", tool_call_id: "a2", content: big(400), label: "res-a2" },
  { role: "assistant", content: big(400), label: "ans1" },
  { role: "user", content: big(400), label: "q2" },
];
for (let budget = 50; budget <= 1200; budget += 25) {
  const kept = token_budget(msgs, budget);
  well_formed(kept);
  assert(kept[0].role === "system", "budget " + budget + ": the system message was dropped");
  assert(kept[kept.length - 1].label === "q2", "budget " + budget + ": the newest message was dropped");
}
const kept = token_budget(msgs, 400);
assert(contents(kept) === "sys,ans1,q2", "kept [" + contents(kept) + "]");
`)
}

func TestTokenBudgetKeepsTheNewestExchangeWhole(t *testing.T) {
	budgetScript(t, `
const msgs = [
  { role: "system", content: "sys" },
  { role: "user", content: big(4000), label: "old" },
  { role: "user", content: "do it", label: "q" },
  { role: "assistant", content: "", tool_calls: [call("b1"), call("b2")], label: "ask-b" },
  { role: "tool", tool_call_id: "b1", content: big(8000), label: "res-b1" },
  { role: "tool", tool_call_id: "b2", content: "ok", label: "res-b2" },
];
const kept = token_budget(msgs, 100);
well_formed(kept);
assert(contents(kept) === "sys,ask-b,res-b1,res-b2", "kept [" + contents(kept) + "]");
`)
}

func TestTokenBudgetCountsToolCallArguments(t *testing.T) {
	budgetScript(t, `
const msgs = [
  { role: "system", content: "sys" },
  { role: "user", content: big(400), label: "q1" },
  { role: "assistant", content: "", tool_calls: [call("c1", big(4000))], label: "ask-c" },
  { role: "tool", tool_call_id: "c1", content: "ok", label: "res-c1" },
  { role: "user", content: "next", label: "q2" },
];
let kept = token_budget(msgs, 500);
well_formed(kept);
assert(contents(kept) === "sys,q2", "kept [" + contents(kept) + "]");

msgs[2].tool_calls = [{ id: "c1", name: "lookup", args: { text: big(4000) } }];
kept = token_budget(msgs, 500);
assert(contents(kept) === "sys,q2", "table arguments were not counted: kept [" + contents(kept) + "]");
`)
}

func TestTokenBudgetLeavesWhatFitsAlone(t *testing.T) {
	budgetScript(t, `
const msgs = [
  { role: "system", content: "sys" },
  { role: "user", content: "hi", label: "q" },
  { role: "assistant", content: "", tool_calls: [call("d1")], label: "ask-d" },
  { role: "tool", tool_call_id: "d1", content: "ok", label: "res-d1" },
  { role: "assistant", content: "done", label: "ans" },
];
assert(token_budget(msgs, 6000) === msgs, "a history within budget must come back as it is");
assert(token_budget(msgs) === msgs, "the default budget must apply when none is given");

const broken = [
  { role: "system", content: "sys" },
  { role: "user", content: big(4000), label: "old" },
  { role: "tool", tool_call_id: "gone", content: "stray", label: "stray" },
  { role: "user", content: "now", label: "q" },
];
const kept = token_budget(broken, 100);
assert(contents(kept) === "sys,stray,q", "kept [" + contents(kept) + "]");
`)
}
