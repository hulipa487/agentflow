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

//go:embed lua/per_chat.lua
var perChat string

//go:embed lua/token_budget.lua
var tokenBudget string

//go:embed lua/routing_table.lua
var routingTable string

//go:embed lua/recency.lua
var recency string

//go:embed lua/semantic.lua
var semantic string

//go:embed lua/fact_extractor.lua
var factExtractor string

//go:embed lua/exec_policy.lua
var execPolicy string

//go:embed lua/ttl.lua
var ttl string

var sources = map[string]string{
	"per_chat":       perChat,
	"token_budget":   tokenBudget,
	"routing_table":  routingTable,
	"recency":        recency,
	"semantic":       semantic,
	"fact_extractor": factExtractor,
	"exec_policy":    execPolicy,
	"ttl":            ttl,
}

// SupportChunks returns the support chunks loaded into every session state
// before the loop plugin.
func SupportChunks() []string {
	return []string{tokenBudget, routingTable, recency, semantic, factExtractor, execPolicy, ttl}
}

// Resolve turns a loop/route reference into Lua source. "builtin:<name>"
// resolves to an embedded builtin; anything else is a file or directory path.
// The second return value is the watch path for hot-reload ("" for builtins):
// a file watches itself; a directory is concatenated as its *.lua files in
// sorted name order and the directory is watched as a whole.
func Resolve(ref string) (src string, watchPath string, err error) {
	if name, ok := strings.CutPrefix(ref, "builtin:"); ok {
		s, found := sources[name]
		if !found {
			return "", "", fmt.Errorf("unknown builtin %q", ref)
		}
		return s, "", nil
	}
	fi, err := os.Stat(ref)
	if err != nil {
		return "", "", fmt.Errorf("read plugin %s: %w", ref, err)
	}
	if !fi.IsDir() {
		b, err := os.ReadFile(ref)
		if err != nil {
			return "", "", fmt.Errorf("read plugin %s: %w", ref, err)
		}
		return string(b), ref, nil
	}
	// Directory loop: concatenate all *.lua files (sorted) with a newline
	// between, so a multi-module loop can be authored as separate files. The
	// order is lexical — name modules with a leading digit (10_identity.lua)
	// or rely on a single `loop.lua` entry that runs last.
	entries, err := os.ReadDir(ref)
	if err != nil {
		return "", "", fmt.Errorf("read loop dir %s: %w", ref, err)
	}
	var parts []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lua") {
			continue
		}
		b, err := os.ReadFile(ref + "/" + e.Name())
		if err != nil {
			return "", "", fmt.Errorf("read %s/%s: %w", ref, e.Name(), err)
		}
		parts = append(parts, string(b))
	}
	if len(parts) == 0 {
		return "", "", fmt.Errorf("loop dir %s has no .lua files", ref)
	}
	return strings.Join(parts, "\n"), ref, nil
}
