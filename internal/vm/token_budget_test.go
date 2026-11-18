package vm

import (
	"os"
	"path/filepath"
	"testing"
)

// budgetScript runs a Lua chunk against the shipped token_budget support
// chunk. The chunk gets two helpers: big(n), a string of n characters (four
// characters estimate as one token), and well_formed(messages), which raises
// unless every tool result directly follows the assistant turn that asked for
// it and every such turn has all of its results — the shape a provider
// accepts.
func budgetScript(t *testing.T, body string) {
	t.Helper()
	code, err := os.ReadFile(filepath.Join("..", "builtins", "lua", "token_budget.lua"))
	if err != nil {
		t.Fatalf("read token_budget.lua: %v", err)
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
local function big(n) return string.rep("x", n) end
local function call(id, args) return { id = id, name = "lookup", arguments = args or "{}" } end
local function well_formed(msgs)
  local i = 1
  while i <= #msgs do
    local m = msgs[i]
    if m.role == "tool" then
      error("message " .. i .. " is a tool result with no assistant turn asking for it (id " .. tostring(m.tool_call_id) .. ")")
    end
    if m.role == "assistant" and type(m.tool_calls) == "table" and #m.tool_calls > 0 then
      local want = {}
      for _, c in ipairs(m.tool_calls) do want[c.id] = true end
      local j = i + 1
      while msgs[j] and msgs[j].role == "tool" do
        assert(want[msgs[j].tool_call_id], "tool result " .. tostring(msgs[j].tool_call_id) .. " follows a turn that did not ask for it")
        want[msgs[j].tool_call_id] = nil
        j = j + 1
      end
      for id in pairs(want) do error("tool call " .. id .. " was kept without its result") end
      i = j
    else
      i = i + 1
    end
  end
end
local function contents(msgs)
  local out = {}
  for _, m in ipairs(msgs) do out[#out + 1] = m.label or m.content end
  return table.concat(out, ",")
end
`
	if err := st.Eval("@assert", helpers+body); err != nil {
		t.Fatal(err)
	}
}

// The bug this file exists for: the trimmer dropped the oldest messages one
// at a time, so the cut could fall between an assistant turn's tool calls and
// their results. The request then opened with a tool result nobody asked for,
// which a provider that validates pairing rejects outright — and the loop sees
// it as a failed model call on a conversation that was fine a moment before.
func TestTokenBudgetNeverSplitsAToolExchange(t *testing.T) {
	budgetScript(t, `
-- Each exchange: an assistant turn asking for two tools, and two results.
-- Sizes are chosen so that, counted message by message, the budget runs out
-- in the middle of the first exchange.
local msgs = {
  { role = "system", content = "sys" },
  { role = "user", content = big(400), label = "q1" },
  { role = "assistant", content = "", tool_calls = { call("a1"), call("a2") }, label = "ask-a" },
  { role = "tool", tool_call_id = "a1", content = big(2000), label = "res-a1" },
  { role = "tool", tool_call_id = "a2", content = big(400), label = "res-a2" },
  { role = "assistant", content = big(400), label = "ans1" },
  { role = "user", content = big(400), label = "q2" },
}
for budget = 50, 1200, 25 do
  local kept = token_budget(msgs, budget)
  local ok, err = pcall(well_formed, kept)
  assert(ok, "budget " .. budget .. " kept [" .. contents(kept) .. "]: " .. tostring(err))
  assert(kept[1].role == "system", "budget " .. budget .. ": the system message was dropped")
  assert(kept[#kept].label == "q2", "budget " .. budget .. ": the newest message was dropped")
end
-- With room for everything but the oversized result, the whole exchange goes
-- and the rest stays: an exchange is kept or dropped as one.
local kept = token_budget(msgs, 400)
assert(contents(kept) == "sys,ans1,q2", "kept [" .. contents(kept) .. "]")
`)
}

// A tool loop calls the trimmer between rounds, when the newest message is a
// tool result. "Keep the newest message" then has to mean the whole exchange
// it belongs to: keeping the result alone is the orphan again.
func TestTokenBudgetKeepsTheNewestExchangeWhole(t *testing.T) {
	budgetScript(t, `
local msgs = {
  { role = "system", content = "sys" },
  { role = "user", content = big(4000), label = "old" },
  { role = "user", content = "do it", label = "q" },
  { role = "assistant", content = "", tool_calls = { call("b1"), call("b2") }, label = "ask-b" },
  { role = "tool", tool_call_id = "b1", content = big(8000), label = "res-b1" },
  { role = "tool", tool_call_id = "b2", content = "ok", label = "res-b2" },
}
-- Far too small for the exchange. It is kept anyway, whole, and everything
-- older goes: an over-budget request the provider can read beats a small one
-- it rejects.
local kept = token_budget(msgs, 100)
well_formed(kept)
assert(contents(kept) == "sys,ask-b,res-b1,res-b2", "kept [" .. contents(kept) .. "]")
`)
}

// A turn's tool calls are sent back to the model on every later request, so
// their arguments are real prompt weight. Costing the turn at its (usually
// empty) content let a history of large tool calls ride in for free.
func TestTokenBudgetCountsToolCallArguments(t *testing.T) {
	budgetScript(t, `
local msgs = {
  { role = "system", content = "sys" },
  { role = "user", content = big(400), label = "q1" },
  { role = "assistant", content = "", tool_calls = { call("c1", big(4000)) }, label = "ask-c" },
  { role = "tool", tool_call_id = "c1", content = "ok", label = "res-c1" },
  { role = "user", content = "next", label = "q2" },
}
-- ~1000 tokens of arguments do not fit in 500; a trimmer that does not count
-- them keeps the exchange.
local kept = token_budget(msgs, 500)
well_formed(kept)
assert(contents(kept) == "sys,q2", "kept [" .. contents(kept) .. "]")
-- Arguments given as a table (the parsed form) are counted too.
msgs[3].tool_calls = { { id = "c1", name = "lookup", args = { text = big(4000) } } }
kept = token_budget(msgs, 500)
assert(contents(kept) == "sys,q2", "table arguments were not counted: kept [" .. contents(kept) .. "]")
`)
}

func TestTokenBudgetLeavesWhatFitsAlone(t *testing.T) {
	budgetScript(t, `
local msgs = {
  { role = "system", content = "sys" },
  { role = "user", content = "hi", label = "q" },
  { role = "assistant", content = "", tool_calls = { call("d1") }, label = "ask-d" },
  { role = "tool", tool_call_id = "d1", content = "ok", label = "res-d1" },
  { role = "assistant", content = "done", label = "ans" },
}
assert(token_budget(msgs, 6000) == msgs, "a history within budget must come back as it is")
assert(token_budget(msgs) == msgs, "the default budget must apply when none is given")

-- A history that arrives already broken is not this function's to mend: it
-- trims, it does not rewrite. What it must not do is make it worse or fail.
local broken = {
  { role = "system", content = "sys" },
  { role = "user", content = big(4000), label = "old" },
  { role = "tool", tool_call_id = "gone", content = "stray", label = "stray" },
  { role = "user", content = "now", label = "q" },
}
local kept = token_budget(broken, 100)
assert(contents(kept) == "sys,stray,q", "kept [" .. contents(kept) .. "]")
`)
}
