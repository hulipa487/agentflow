// Package builtins ships the default plugins, embedded in the binary.
// Builtins are plugins with the same contract as user plugins; a user file
// may shadow a builtin by name (docs/builtins.md).
package builtins

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
)

//go:embed lua/react.lua
var react string

//go:embed lua/per_chat.lua
var perChat string

//go:embed lua/token_budget.lua
var tokenBudget string

//go:embed lua/routing_table.lua
var routingTable string

//go:embed lua/recency.lua
var recency string

//go:embed lua/fact_extractor.lua
var factExtractor string

//go:embed lua/exec_policy.lua
var execPolicy string

//go:embed lua/ttl.lua
var ttl string

var sources = map[string]string{
	"react":          react,
	"per_chat":       perChat,
	"token_budget":   tokenBudget,
	"routing_table":  routingTable,
	"recency":        recency,
	"fact_extractor": factExtractor,
	"exec_policy":    execPolicy,
	"ttl":            ttl,
}

// SupportChunks returns the support chunks loaded into every session state
// before the loop plugin.
func SupportChunks() []string {
	return []string{tokenBudget, routingTable, recency, factExtractor, execPolicy, ttl}
}

// Resolve turns a loop/route reference into Lua source. "builtin:<name>"
// resolves to an embedded builtin; anything else is a file path. The second
// return value is the file path for hot-reload watching ("" for builtins).
func Resolve(ref string) (src string, watchPath string, err error) {
	if name, ok := strings.CutPrefix(ref, "builtin:"); ok {
		s, found := sources[name]
		if !found {
			return "", "", fmt.Errorf("unknown builtin %q", ref)
		}
		return s, "", nil
	}
	b, err := os.ReadFile(ref)
	if err != nil {
		return "", "", fmt.Errorf("read plugin %s: %w", ref, err)
	}
	return string(b), ref, nil
}
