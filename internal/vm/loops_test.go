package vm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestShippedLoopsParse(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "builtins", "ts", "*.ts"))
	if err != nil {
		t.Fatalf("glob builtins/ts: %v", err)
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

func TestTokenBudgetCountsThinkingBlocks(t *testing.T) {
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
	if err := st.Eval("@assert", `
const assert = (c, m) => { if (!c) throw new Error(m || "assertion failed"); };
const big = (n) => "x".repeat(n);
const msgs = [
  { role: "system", content: "sys" },
  { role: "user", content: big(4000) },
  { role: "assistant", content: "",
    thinking_blocks: [{ type: "thinking", thinking: big(4000), signature: "s" }] },
  { role: "user", content: "recent" },
];
const kept = token_budget(msgs, 1100);
assert(kept.length === 3, "want 3 kept, got " + kept.length);
assert(kept[0].role === "system", "system must survive");
assert(kept[1].thinking_blocks !== undefined, "assistant turn with blocks must survive");
assert(kept[2].content === "recent", "newest message must survive");
for (const m of kept) {
  assert(m.content !== big(4000), "oversized history kept — trimmer ignored thinking_blocks weight");
}
`); err != nil {
		t.Fatalf("token_budget accounting wrong: %v", err)
	}
}
