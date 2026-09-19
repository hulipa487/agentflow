package vm

import (
	"os"
	"path/filepath"
	"testing"
)

// Builtin loops shipped with the runtime must parse. A syntax slip would only
// surface at session boot; compile-check them here so the gate catches it.
// Product loops (e.g. an orchestrator) live outside this repo and are checked
// there.
func TestShippedLoopsParse(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "builtins", "lua", "*.lua"))
	if err != nil {
		t.Fatalf("glob builtins/lua: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no loop files found to compile-check")
	}
	for _, f := range files {
		code, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if err := CompileCheck(f, string(code)); err != nil {
			t.Errorf("%s does not parse: %v", f, err)
		}
	}
}

// TestTokenBudgetCountsThinkingBlocks: replayed thinking blocks ride outside
// content but are real wire cost on every continuation request — the trimmer
// must see them, or a thinking-enabled loop with a full context slowly
// starves its own history to carry invisible weight.
func TestTokenBudgetCountsThinkingBlocks(t *testing.T) {
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
	if err := st.Eval("@assert", `
local function big(n) local s = ""; for _ = 1, n do s = s .. "x" end; return s end
-- 4000 chars ≈ 1000 est tokens. Budget 1100: system + the assistant turn
-- (1000 tokens of replayed thinking) + the newest message fit; the 1000-token
-- user turn does not. A trimmer blind to thinking_blocks costs the assistant
-- turn at 4 tokens and keeps the oversized user turn too (1023 ≤ 1100),
-- so both 1000-token turns ride into a request that blows the window.
local msgs = {
  { role = "system", content = "sys" },
  { role = "user", content = big(4000) },
  { role = "assistant", content = "",
    thinking_blocks = { { type = "thinking", thinking = big(4000), signature = "s" } } },
  { role = "user", content = "recent" },
}
local kept = token_budget(msgs, 1100)
assert(#kept == 3, "want 3 kept, got " .. #kept)
assert(kept[1].role == "system", "system must survive")
assert(kept[2].thinking_blocks ~= nil, "assistant turn with blocks must survive")
assert(kept[3].content == "recent", "newest message must survive")
for _, m in ipairs(kept) do
  assert(m.content ~= big(4000), "oversized history kept — trimmer ignored thinking_blocks weight")
end
`); err != nil {
		t.Fatalf("token_budget accounting wrong: %v", err)
	}
}
