// Package builtins ships the default plugins, embedded in the binary.
// Builtins are plugins with the same contract as user plugins; a file
// <plugins.dir>/<name>.lua shadows a builtin by name (see SetPluginDir).
//
// Every chunk this package hands out — embedded, shadowed, or read from a
// path — has passed the prelude version gate (vm.CheckChunkVersion): it
// declares the prelude API version it targets and that version is the one this
// core provides. Resolve returns a refusal instead of source for a chunk that
// does not, and SupportChunks — the other way a chunk leaves this package, for
// the chunks every session loads before its loop — returns one too, which is
// what stops a core upgrade from silently loading Lua written against a
// different API. There is no compatibility window, by design — see
// vm.PreludeVersion.
package builtins

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agentflow/internal/vm"
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

var sources = map[string]string{
	"per_chat":       perChat,
	"token_budget":   tokenBudget,
	"routing_table":  routingTable,
	"recency":        recency,
	"semantic":       semantic,
	"fact_extractor": factExtractor,
}

// supportOrder fixes the evaluation order of support chunks in every session.
//
// exec_policy and ttl used to be here. Both were shadowable, listed and loaded
// into every session's Lua state, and neither was ever called: shell.exec goes
// straight to the manager, and shell_before_exec_policy/shell_ttl had no caller
// in Go or in Lua. A shipped plugin whose entry point nothing invokes is a
// promise the engine does not keep, so they are gone — and the docs now say
// what is true, that shell commands are gated by the capability alone.
var supportOrder = []string{"token_budget", "routing_table", "recency", "semantic", "fact_extractor"}

// Names returns the builtin plugin names as config spells them —
// "plugin:per_chat", not "per_chat".
//
// The "plugin:" prefix marks this vocabulary alone. It used to be "builtin:",
// which loop names, tool names (builtin:web_search) and memory provider names
// (builtin:sqlite) all spelled: a name from one was never valid in another's
// field, and nothing in the string said which it was. The prefix now also
// settles the loop-vs-path question outright — a path never begins with
// "plugin:", where before a file named "per_chat" was unreachable.
//
// Returning the config spelling is what lets a caller compare a configured
// value against this list directly and say which vocabulary it belongs to.
func Names() []string {
	out := make([]string, 0, len(sources))
	for n := range sources {
		out = append(out, "plugin:"+n)
	}
	sort.Strings(out)
	return out
}

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

// SupportChunkPaths returns the file backing each support chunk that
// plugins.dir overrides, keyed by chunk name. Only a shadow has a path: an
// embedded chunk has no file, so there is nothing on disk to watch. The
// deployment's reload watcher polls these, which is what makes a shadowed
// support chunk hot-reloadable rather than new-session-only.
func SupportChunkPaths() map[string]string {
	out := map[string]string{}
	for _, name := range supportOrder {
		if _, path, ok := shadow(name); ok {
			out[name] = path
		}
	}
	return out
}

// SupportChunks returns the support chunks loaded into every session state
// before the loop plugin, or a prelude version refusal for one this core cannot
// serve. A chunk shadowed in plugins.dir loads from disk (and is re-read on
// every session start — the first one and each reload restart — so a shadow
// edit takes effect on the next restart of every session).
//
// The gate is applied here as well as in Resolve because a support chunk is not
// a loop/route ref: nothing resolves it, so this call is the only way one
// reaches a session, and a version refusal has to come out of here or it does
// not exist for them. The embedded chunks are checked too — the package doc's
// promise covers everything this package hands out, and a shadow is not the
// only way a chunk can be wrong: a core upgrade moves the embedded chunks with
// it, and a build whose own Lua is out of step should refuse to load rather
// than run.
//
// The name in a refusal is what the operator sees: the shadow's path, or the
// same "builtin:<name>" spelling Resolve uses for an embedded chunk.
func SupportChunks() ([]string, error) {
	out := make([]string, 0, len(supportOrder))
	for _, name := range supportOrder {
		src, path, ok := shadow(name)
		if !ok {
			src, path = sources[name], "builtin:"+name
		}
		if err := vm.CheckChunkVersion(path, src); err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, nil
}

// Resolve turns a loop/route reference into Lua source. "plugin:<name>"
// resolves to an embedded builtin unless plugins.dir shadows it (the shadow
// file then also becomes the hot-reload watch path); anything else is a file
// or directory path. The second return value is the watch path for
// hot-reload ("" for unshadowed builtins): a file watches itself; a
// directory is concatenated as its *.lua files in sorted name order and the
// directory is watched as a whole.
//
// Every chunk returned has passed the prelude version gate, named in the error
// by the path the chunk was read from. A directory is checked member by member,
// not as the concatenation: only the first member's directive would lead the
// joined source, so a support member written against an older prelude would be
// invisible to a check on the result.
func Resolve(ref string) (src string, watchPath string, err error) {
	if name, ok := strings.CutPrefix(ref, "plugin:"); ok {
		if _, found := sources[name]; !found {
			return "", "", fmt.Errorf("unknown plugin %q (builtins are named plugin:<name>, e.g. plugin:per_chat)", ref)
		}
		if src, path, ok := shadow(name); ok {
			if err := vm.CheckChunkVersion(path, src); err != nil {
				return "", "", err
			}
			return src, path, nil
		}
		// The embedded chunk is checked like any other. A failure here is a
		// broken build rather than a broken deployment — the conformance suite
		// (TestPreludeContract) refuses to ship one — and it is better to
		// refuse to start than to run a core whose own Lua is out of step.
		if err := vm.CheckChunkVersion("builtin:"+name, sources[name]); err != nil {
			return "", "", err
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
		if err := vm.CheckChunkVersion(ref, string(b)); err != nil {
			return "", "", err
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
		member := filepath.Join(ref, e.Name())
		b, err := os.ReadFile(member)
		if err != nil {
			return "", "", fmt.Errorf("read %s: %w", member, err)
		}
		if err := vm.CheckChunkVersion(member, string(b)); err != nil {
			return "", "", err
		}
		parts = append(parts, string(b))
	}
	if len(parts) == 0 {
		return "", "", fmt.Errorf("loop dir %s has no .lua files", ref)
	}
	return strings.Join(parts, "\n"), ref, nil
}
