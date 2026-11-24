// Package builtins embeds the runtime's core-contract Lua chunks and registers
// them with internal/luacode.
//
// Only chunks the runtime core itself requires or defines as standard prelude
// helpers live here:
//   - per_chat       (default gateway.route handler when none is configured)
//   - routing_table  (default memory.write routing table called by prelude.go)
//   - recency        (default memory.recall handler called by prelude.go)
//   - semantic       (embedding recall handler dispatched by prelude.go)
//   - token_budget   (official prelude stdlib helper declared in af.d.lua)
//
// Reference/example loops and optional helpers (such as openai_passthrough and
// fact_extractor) live in agentflow-studio (`examples/`) instead of being
// embedded in the binary.
//
// Generic Lua resolution, package imports, shadowing via plugins.dir, and
// prelude version gating live in internal/luacode so core Go packages do not
// depend on embedded Lua scripts.
package builtins

import (
	_ "embed"
	"sort"

	"agentflow/internal/luacode"
)

//go:embed ts/per_chat.ts
var perChat string

//go:embed ts/token_budget.ts
var tokenBudget string

//go:embed ts/routing_table.ts
var routingTable string

//go:embed ts/recency.ts
var recency string

//go:embed ts/semantic.ts
var semantic string

var sources = map[string]string{
	"per_chat":      perChat,
	"token_budget":  tokenBudget,
	"routing_table": routingTable,
	"recency":       recency,
	"semantic":      semantic,
}

// supportOrder fixes the evaluation order of support chunks in every session.
//
// Only chunks that the prelude calls directly or exposes as part of the
// documented Lua stdlib belong here. Unreferenced example heuristics (like
// fact_extractor, exec_policy, and ttl) are kept out of the binary.
var supportOrder = []string{"token_budget", "routing_table", "recency", "semantic"}

func init() {
	Register()
}

// Register installs the embedded core-contract Lua chunks into internal/luacode.
// Safe to call explicitly at boot (or after a test replaces luacode builtins).
func Register() {
	luacode.SetBuiltins(sources, supportOrder)
}

// Names returns the embedded builtin plugin names as config spells them —
// "plugin:per_chat", not "per_chat".
func Names() []string {
	out := make([]string, 0, len(sources))
	for n := range sources {
		out = append(out, "plugin:"+n)
	}
	sort.Strings(out)
	return out
}
