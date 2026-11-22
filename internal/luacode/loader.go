// Package luacode resolves, version-gates, and expands Lua source chunks for
// AgentFlow: single files, multi-file loop directories, named packages
// ("package:<name>" and "-- af-import: <name>"), and registered embedded
// builtins ("plugin:<name>", shadowable via plugins.dir).
//
// This package holds no embedded Lua of its own; the runtime's default
// embedded chunks live in internal/builtins and register themselves via
// SetBuiltins. That keeps core Go packages (session, supervisor, reload,
// looptest) decoupled from shipped Lua scripts.
//
// Every chunk this package hands out — embedded, shadowed, or read from a
// path — has passed the prelude version gate (vm.CheckChunkVersion): it
// declares the prelude API version it targets and that version is the one this
// core provides. Resolve returns a refusal instead of source for a chunk that
// does not, and SupportChunks returns one too, which is what stops a core
// upgrade from silently loading Lua written against a different API.
package luacode

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"agentflow/internal/vm"
)

var (
	mu             sync.RWMutex
	builtinSources map[string]string
	supportOrder   []string
)

// SetBuiltins registers the embedded core-contract Lua chunks and the ordered
// subset loaded into every session before its loop (support chunks).
// Safe for boot-time and concurrent test/runtime inspection.
func SetBuiltins(sources map[string]string, order []string) {
	mu.Lock()
	defer mu.Unlock()
	if sources == nil {
		builtinSources = nil
	} else {
		cp := make(map[string]string, len(sources))
		for k, v := range sources {
			cp[k] = v
		}
		builtinSources = cp
	}
	if order == nil {
		supportOrder = nil
	} else {
		supportOrder = append([]string(nil), order...)
	}
}

// Names returns the registered builtin plugin names as config spells them —
// "plugin:per_chat", not "per_chat".
//
// The "plugin:" prefix marks this vocabulary alone. Returning the config
// spelling lets a caller compare a configured value against this list directly
// and say which vocabulary it belongs to.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(builtinSources))
	for n := range builtinSources {
		out = append(out, "plugin:"+n)
	}
	sort.Strings(out)
	return out
}

// BuiltinNames returns the raw sorted names of all registered builtins
// (without the "plugin:" prefix).
func BuiltinNames() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(builtinSources))
	for n := range builtinSources {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// SupportOrder returns the ordered names of registered support chunks.
func SupportOrder() []string {
	mu.RLock()
	defer mu.RUnlock()
	return append([]string(nil), supportOrder...)
}

// BuiltinSource returns the embedded Lua source for a registered builtin name.
func BuiltinSource(name string) (string, bool) {
	mu.RLock()
	defer mu.RUnlock()
	src, ok := builtinSources[name]
	return src, ok
}

// pluginDir is the deployment's plugins.dir, set at boot via SetPluginDir or
// updated when Studio enables plugin overrides.
var pluginDir string

// SetPluginDir points the loader at the deployment's plugins dir.
func SetPluginDir(dir string) {
	mu.Lock()
	defer mu.Unlock()
	pluginDir = dir
}

// PluginDir returns the active plugins.dir path.
func PluginDir() string {
	mu.RLock()
	defer mu.RUnlock()
	return pluginDir
}

// packages maps a deployment package name to its directory (config packages:).
// A loop imports one with "-- af-import: <name>" or refers to it directly as
// "package:<name>". The package's files are version-gated and concatenated
// before the importing chunk, so a package is shared source, not a runtime
// module: it defines tables/functions the importing loop uses.
var packages map[string]string

// SetPackages installs the deployment's package map.
func SetPackages(m map[string]string) {
	mu.Lock()
	defer mu.Unlock()
	if m == nil {
		packages = nil
		return
	}
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	packages = cp
}

// Packages returns a copy of the registered package directories keyed by name.
func Packages() map[string]string {
	mu.RLock()
	defer mu.RUnlock()
	out := make(map[string]string, len(packages))
	for k, v := range packages {
		out[k] = v
	}
	return out
}

// PackagePaths returns the directories the reload watcher should poll. A
// package edit changes every loop that imports it.
func PackagePaths() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(packages))
	for _, dir := range packages {
		out = append(out, dir)
	}
	sort.Strings(out)
	return out
}

// packageManifest is optional; a package without one loads its *.lua files in
// lexical order. prelude_api pins the package to this core's Lua API.
type packageManifest struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Version     string   `yaml:"version"`
	PreludeAPI  int      `yaml:"prelude_api"`
	Files       []string `yaml:"files"`
}

// packageSource loads one package, recursively expanding its own imports.
// Caller must hold mu.RLock.
func packageSourceLocked(name string, stack []string) (string, string, error) {
	dir, ok := packages[name]
	if !ok {
		return "", "", fmt.Errorf("unknown package %q (declare it under packages:)", name)
	}
	for _, s := range stack {
		if s == name {
			return "", "", fmt.Errorf("package import cycle: %s -> %s", strings.Join(stack, " -> "), name)
		}
	}
	if dir == "" {
		return "", "", fmt.Errorf("package %q has no directory", name)
	}
	files := []string{}
	manifestPath := filepath.Join(dir, "manifest.yaml")
	if b, err := os.ReadFile(manifestPath); err == nil {
		var m packageManifest
		if err := yaml.Unmarshal(b, &m); err != nil {
			return "", "", fmt.Errorf("package %q: manifest: %w", name, err)
		}
		if m.PreludeAPI != 0 && m.PreludeAPI != vm.PreludeVersion {
			return "", "", fmt.Errorf("package %q: manifest prelude_api %d, this core provides %d", name, m.PreludeAPI, vm.PreludeVersion)
		}
		files = append(files, m.Files...)
	} else if !os.IsNotExist(err) {
		return "", "", fmt.Errorf("package %q: manifest: %w", name, err)
	}
	if len(files) == 0 {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return "", "", fmt.Errorf("package %q: %w", name, err)
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".lua") {
				files = append(files, e.Name())
			}
		}
		sort.Strings(files)
	}
	if len(files) == 0 {
		return "", "", fmt.Errorf("package %q: no .lua files", name)
	}
	var parts []string
	for _, f := range files {
		p := f
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", "", fmt.Errorf("package %q: read %s: %w", name, p, err)
		}
		if err := vm.CheckChunkVersion(p, string(b)); err != nil {
			return "", "", err
		}
		expanded, err := expandImportsLocked(string(b), append(stack, name))
		if err != nil {
			return "", "", fmt.Errorf("package %q: %w", name, err)
		}
		parts = append(parts, expanded)
	}
	return strings.Join(parts, "\n"), dir, nil
}

// expandImportsLocked replaces "-- af-import: <name>" lines with the package's
// source. Caller must hold mu.RLock.
func expandImportsLocked(src string, stack []string) (string, error) {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines)+8)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, "-- af-import:"); ok {
			name := strings.TrimSpace(rest)
			if name == "" {
				return "", fmt.Errorf("af-import: missing package name")
			}
			body, _, err := packageSourceLocked(name, stack)
			if err != nil {
				return "", err
			}
			// The package was version-gated on its own file. Its directive
			// must not join the importing chunk's leading comment block: the
			// gate would see two declarations and refuse the whole loop.
			out = append(out, stripPreludeDirective(body))
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n"), nil
}

// stripPreludeDirective removes af-prelude-version lines from embedded package
// source. The package file was already checked; leaving its directive in place
// would make the concatenated loop declare the version twice.
func stripPreludeDirective(src string) string {
	lines := strings.Split(src, "\n")
	out := lines[:0]
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "--") && strings.Contains(t, "af-prelude-version") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// shadowLocked returns the deployment's override for a builtin name, if one exists.
// Caller must hold mu.RLock.
func shadowLocked(name string) (src, path string, ok bool) {
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
	mu.RLock()
	defer mu.RUnlock()
	out := map[string]string{}
	for _, name := range supportOrder {
		if _, path, ok := shadowLocked(name); ok {
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
func SupportChunks() ([]string, error) {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(supportOrder))
	for _, name := range supportOrder {
		src, path, ok := shadowLocked(name)
		if !ok {
			src, path = builtinSources[name], "builtin:"+name
		}
		if err := vm.CheckChunkVersion(path, src); err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, nil
}

// Resolve turns a loop/route reference into Lua source. "plugin:<name>"
// resolves to a registered embedded builtin unless plugins.dir shadows it (the
// shadow file then also becomes the hot-reload watch path); anything else is a
// file or directory path. The second return value is the watch path for
// hot-reload ("" for unshadowed builtins): a file watches itself; a directory
// is concatenated as its *.lua files in sorted name order and the directory is
// watched as a whole.
//
// Every chunk returned has passed the prelude version gate, named in the error
// by the path the chunk was read from. A directory is checked member by member,
// not as the concatenation: only the first member's directive would lead the
// joined source, so a support member written against an older prelude would be
// invisible to a check on the result.
func Resolve(ref string) (src string, watchPath string, err error) {
	mu.RLock()
	defer mu.RUnlock()
	if name, ok := strings.CutPrefix(ref, "package:"); ok {
		return packageSourceLocked(name, nil)
	}
	if name, ok := strings.CutPrefix(ref, "plugin:"); ok {
		if _, found := builtinSources[name]; !found {
			return "", "", fmt.Errorf("unknown plugin %q (builtins are named plugin:<name>, e.g. plugin:per_chat)", ref)
		}
		if src, path, ok := shadowLocked(name); ok {
			if err := vm.CheckChunkVersion(path, src); err != nil {
				return "", "", err
			}
			expanded, err := expandImportsLocked(src, nil)
			if err != nil {
				return "", "", err
			}
			return expanded, path, nil
		}
		if err := vm.CheckChunkVersion("builtin:"+name, builtinSources[name]); err != nil {
			return "", "", err
		}
		expanded, err := expandImportsLocked(builtinSources[name], nil)
		if err != nil {
			return "", "", err
		}
		return expanded, "", nil
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
		expanded, err := expandImportsLocked(string(b), nil)
		if err != nil {
			return "", "", err
		}
		return expanded, ref, nil
	}
	// Directory loop: concatenate all *.lua files (sorted) with a newline
	// between, so a multi-module loop can be authored as separate files.
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
	expanded, err := expandImportsLocked(strings.Join(parts, "\n"), nil)
	if err != nil {
		return "", "", err
	}
	return expanded, ref, nil
}

// CheckSource validates a Lua chunk against the prelude version gate, expands
// any "-- af-import:" directives, and checks Lua syntax/compilation. When
// selfPkg is non-empty it seeds the import cycle stack so a package file
// importing itself fails cleanly.
func CheckSource(name, src, selfPkg string) error {
	if err := vm.CheckChunkVersion(name, src); err != nil {
		return err
	}
	var stack []string
	if selfPkg != "" {
		stack = []string{selfPkg}
	}
	mu.RLock()
	expanded, err := expandImportsLocked(src, stack)
	mu.RUnlock()
	if err != nil {
		return err
	}
	return vm.CompileCheck(name, expanded)
}
