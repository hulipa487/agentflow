package studio

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "agentflow/internal/builtins"
	"agentflow/internal/config"
	"agentflow/internal/luacode"
	"agentflow/internal/vm"
)

func pluginsFixture(t *testing.T, systemYAML string) (*Plugins, string, *pluginHookCalls) {
	t.Helper()
	t.Cleanup(func() {
		luacode.SetPluginDir("")
		luacode.SetPackages(nil)
	})
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "system.yaml"), systemYAML)
	writeFile(t, filepath.Join(dir, "loops", "chat.ts"), "// af-prelude-version: 2\nfunction loop() {}\n")
	writeFile(t, filepath.Join(dir, "profiles", "chat.yaml"), "name: chat\nloop: ./loops/chat.ts\nsafety: none\n")
	cfg, err := config.LoadDir(dir, safetyLog())
	if err != nil {
		t.Fatal(err)
	}
	luacode.SetPluginDir(cfg.Plugins.Dir)
	luacode.SetPackages(cfg.Packages)
	calls := &pluginHookCalls{}
	p := NewPlugins(cfg, dir, "", PluginHooks{
		OnReloadSupport:  func() { calls.support++ },
		OnReloadRoute:    func() { calls.route++ },
		OnReloadPackages: func() { calls.packages++ },
	}, safetyLog())
	return p, dir, calls
}

type pluginHookCalls struct {
	support  int
	route    int
	packages int
}

const minimalPluginSystem = `version: "1"
runtime:
  persistence: sqlite://./data/agentflow.db
profiles:
  safety:
    none: { filters: [] }
`

func findBuiltin(t *testing.T, list PluginsList, name string) BuiltinPluginView {
	t.Helper()
	for _, b := range list.Builtins {
		if b.Name == name {
			return b
		}
	}
	t.Fatalf("builtin %q not found in %+v", name, list.Builtins)
	return BuiltinPluginView{}
}

func TestPluginsBuiltinLifecycle(t *testing.T) {
	p, dir, calls := pluginsFixture(t, minimalPluginSystem)

	list, err := p.List()
	if err != nil {
		t.Fatal(err)
	}
	if !list.Editable || list.PluginDirConfigured || list.PreludeVersion != vm.PreludeVersion {
		t.Fatalf("unexpected list header: %+v", list)
	}
	tb := findBuiltin(t, list, "token_budget")
	if tb.Kind != "support" || tb.Shadowed || tb.Source == "" {
		t.Fatalf("unexpected token_budget view: %+v", tb)
	}
	pc := findBuiltin(t, list, "per_chat")
	if pc.Kind != "route" || pc.Shadowed {
		t.Fatalf("unexpected per_chat view: %+v", pc)
	}

	// Saving a shadow auto-enables plugins.dir: ./plugins in system.yaml and
	// fires OnReloadSupport (not OnReloadRoute).
	customBudget := "// af-prelude-version: 2\nfunction token_budget(messages, budget) {\n  return messages;\n}\n"
	list, err = p.SaveBuiltin("token_budget", customBudget, 0)
	if err != nil {
		t.Fatal(err)
	}
	if calls.support != 1 || calls.route != 0 {
		t.Fatalf("hook counts after support save = %+v", *calls)
	}
	if !list.PluginDirConfigured {
		t.Fatal("expected PluginDirConfigured to be true after saving shadow")
	}
	tb = findBuiltin(t, list, "token_budget")
	if !tb.Shadowed || tb.MTime == 0 || tb.Source != customBudget {
		t.Fatalf("saved builtin mismatch: %+v", tb)
	}
	if _, err := os.Stat(filepath.Join(dir, "plugins", "token_budget.ts")); err != nil {
		t.Fatalf("shadow file missing on disk: %v", err)
	}
	sysBytes, err := os.ReadFile(filepath.Join(dir, "system.yaml"))
	if err != nil || !strings.Contains(string(sysBytes), "dir: ./plugins") {
		t.Fatalf("system.yaml missing plugins.dir: %s (%v)", string(sysBytes), err)
	}

	// Stale mtime returns ErrPluginConflict.
	if _, err := p.SaveBuiltin("token_budget", customBudget, tb.MTime-1); !errors.Is(err, ErrPluginConflict) {
		t.Fatalf("expected ErrPluginConflict on stale save, got %v", err)
	}

	// Saving route override fires OnReloadRoute.
	routeSrc := "// af-prelude-version: 2\nfunction route(msg) {\n  return null;\n}\n"
	if _, err := p.SaveBuiltin("per_chat", routeSrc, 0); err != nil {
		t.Fatal(err)
	}
	if calls.route != 1 {
		t.Fatalf("expected route reload count 1, got %+v", *calls)
	}

	// Resetting removes the shadow file and restores embedded source.
	list, err = p.ResetBuiltin("token_budget", tb.MTime)
	if err != nil {
		t.Fatal(err)
	}
	if calls.support != 2 {
		t.Fatalf("expected support reload count 2, got %+v", *calls)
	}
	tb = findBuiltin(t, list, "token_budget")
	if tb.Shadowed || tb.MTime != 0 || tb.Source != tb.BuiltinSource {
		t.Fatalf("reset builtin still shadowed: %+v", tb)
	}

	// Invalid JS/TS is rejected without touching disk.
	if _, err := p.SaveBuiltin("token_budget", "// af-prelude-version: 2\nfunction broken(\n", 0); err == nil {
		t.Fatal("expected syntax error on broken script")
	}
}

func TestPluginsPackagesLifecycle(t *testing.T) {
	p, dir, calls := pluginsFixture(t, minimalPluginSystem)

	src := "// af-prelude-version: 2\nvar M = { hi() { return 'ok'; } };\n"
	list, err := p.CreatePackage("shared", "helpers.ts", src)
	if err != nil {
		t.Fatal(err)
	}
	if calls.packages != 1 {
		t.Fatalf("expected packages hook 1, got %+v", *calls)
	}
	if len(list.Packages) != 1 || list.Packages[0].Name != "shared" || !list.Packages[0].Managed {
		t.Fatalf("unexpected packages list: %+v", list.Packages)
	}
	if len(list.Packages[0].Files) != 1 || list.Packages[0].Files[0].Name != "helpers.ts" {
		t.Fatalf("unexpected package files: %+v", list.Packages[0].Files)
	}

	// Path traversal / invalid filenames are rejected.
	if _, err := p.SavePackageFile("shared", "../escape.ts", src, 0); err == nil {
		t.Fatal("expected path traversal rejection")
	}
	if _, err := p.CreatePackage("../bad", "a.ts", src); err == nil {
		t.Fatal("expected invalid package name rejection")
	}

	// Deleting the only file is refused.
	onlyMTime := list.Packages[0].Files[0].MTime
	if _, err := p.DeletePackageFile("shared", "helpers.ts", onlyMTime); err == nil {
		t.Fatal("expected refusal when deleting the sole .ts file")
	}

	// Add a second file, then delete helpers.ts succeeds.
	extraSrc := "// af-prelude-version: 2\nvar E = {};\n"
	list, err = p.SavePackageFile("shared", "extra.ts", extraSrc, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Packages[0].Files) != 2 {
		t.Fatalf("expected 2 package files, got %d", len(list.Packages[0].Files))
	}
	list, err = p.DeletePackageFile("shared", "helpers.ts", onlyMTime)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Packages[0].Files) != 1 || list.Packages[0].Files[0].Name != "extra.ts" {
		t.Fatalf("unexpected files after delete: %+v", list.Packages[0].Files)
	}

	// When an agent loop imports the package, DeletePackage refuses.
	writeFile(t, filepath.Join(dir, "loops", "chat.ts"), "// af-prelude-version: 2\n// af-import: shared\nfunction loop() {}\n")
	if _, err := p.DeletePackage("shared"); err == nil || !strings.Contains(err.Error(), "chat") {
		t.Fatalf("expected import refusal naming agent chat, got %v", err)
	}

	// After removing the import, DeletePackage cleans config and managed dir.
	writeFile(t, filepath.Join(dir, "loops", "chat.ts"), "// af-prelude-version: 2\nfunction loop() {}\n")
	list, err = p.DeletePackage("shared")
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Packages) != 0 {
		t.Fatalf("expected no packages left, got %+v", list.Packages)
	}
	if _, err := os.Stat(filepath.Join(dir, "packages", "shared")); !os.IsNotExist(err) {
		t.Fatalf("expected managed package dir removed, got err=%v", err)
	}
}
