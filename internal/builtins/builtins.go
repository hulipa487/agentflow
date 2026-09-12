// Package builtins ships the default plugins, embedded in the binary.
// Builtins are plugins with the same contract as user plugins; a file
// <plugins.dir>/<name>.lua shadows a builtin by name (see SetPluginDir).
package builtins

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
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

// supportOrder fixes the evaluation order of support chunks in every session.
var supportOrder = []string{"token_budget", "routing_table", "recency", "semantic", "fact_extractor", "exec_policy", "ttl"}

// pluginDir is the deployment's plugins.dir, set once at boot via
// SetPluginDir. A file <pluginDir>/<name>.lua shadows the embedded builtin
// of the same name — for loop/route refs (Resolve) and support chunks
// (SupportChunks) alike. Boot-time only; not synchronized.
var pluginDir string

// SetPluginDir points the builtin loader at the deployment's plugins dir.
// Call once at boot, before any Resolve or SupportChunks.
func SetPluginDir(dir string) { pluginDir = dir }

// shadow returns the deployment's override for a builtin name, if one exists.
func shadow(name string) (src, path string, ok bool) {
	if pluginDir == "" {
		return "", "", false
	}
	p := filepath.Join(pluginDir, name+".lua")
	b, err := os.ReadFile(p)
	if err != nil {
		return "", "", false
	}
	return string(b), p, true
}

// SupportChunks returns the support chunks loaded into every session state
// before the loop plugin. A chunk shadowed in plugins.dir loads from disk
// (and is re-read per spawn, so shadow edits take effect on new sessions).
func SupportChunks() []string {
	out := make([]string, 0, len(supportOrder))
	for _, name := range supportOrder {
		if src, _, ok := shadow(name); ok {
			out = append(out, src)
			continue
		}
		out = append(out, sources[name])
	}
	return out
}

// Resolve turns a loop/route reference into Lua source. "builtin:<name>"
// resolves to an embedded builtin unless plugins.dir shadows it (the shadow
// file then also becomes the hot-reload watch path); anything else is a file
// or directory path. The second return value is the watch path for
// hot-reload ("" for unshadowed builtins): a file watches itself; a
// directory is concatenated as its *.lua files in sorted name order and the
// directory is watched as a whole.
func Resolve(ref string) (src string, watchPath string, err error) {
	if name, ok := strings.CutPrefix(ref, "builtin:"); ok {
		if _, found := sources[name]; !found {
			return "", "", fmt.Errorf("unknown builtin %q", ref)
		}
		if src, path, ok := shadow(name); ok {
			return src, path, nil
		}
		return sources[name], "", nil
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
