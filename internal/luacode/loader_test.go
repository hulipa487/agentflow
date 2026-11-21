package luacode

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/vm"
)

func directive() string { return fmt.Sprintf("-- af-prelude-version: %d\n", vm.PreludeVersion) }

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func withTestBuiltins(t *testing.T) map[string]string {
	t.Helper()
	prevSources := builtinSources
	prevOrder := supportOrder
	srcs := map[string]string{
		"alpha": directive() + "ALPHA = 1\n",
		"beta":  directive() + "BETA = 2\n",
		"route": directive() + "function loop() end\n",
	}
	SetBuiltins(srcs, []string{"alpha", "beta"})
	t.Cleanup(func() {
		SetBuiltins(prevSources, prevOrder)
		SetPluginDir("")
		SetPackages(nil)
	})
	return srcs
}

func TestPluginDirShadowsRegisteredBuiltin(t *testing.T) {
	srcs := withTestBuiltins(t)
	dir := t.TempDir()
	shadowSrc := directive() + "BETA = 99\n"
	writeFile(t, filepath.Join(dir, "beta.lua"), shadowSrc)
	SetPluginDir(dir)

	src, watch, err := Resolve("plugin:beta")
	if err != nil {
		t.Fatal(err)
	}
	if src != shadowSrc || watch != filepath.Join(dir, "beta.lua") {
		t.Fatalf("got (%q, %q), want shadow", src, watch)
	}

	chunks, err := SupportChunks()
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 || chunks[0] != srcs["alpha"] || chunks[1] != shadowSrc {
		t.Fatalf("unexpected support chunks: %#v", chunks)
	}

	paths := SupportChunkPaths()
	if len(paths) != 1 || paths["beta"] != filepath.Join(dir, "beta.lua") {
		t.Fatalf("unexpected shadow paths: %v", paths)
	}

	// Unshadowed builtin falls back to registered source, unwatched.
	src, watch, err = Resolve("plugin:alpha")
	if err != nil {
		t.Fatal(err)
	}
	if src != srcs["alpha"] || watch != "" {
		t.Fatalf("unshadowed builtin got (%q, %q)", src, watch)
	}
}

func TestUnknownPluginRejected(t *testing.T) {
	withTestBuiltins(t)
	if _, _, err := Resolve("plugin:nope"); err == nil || !strings.Contains(err.Error(), "unknown plugin") {
		t.Fatalf("expected unknown plugin error, got %v", err)
	}
}

func TestVersionGateRefusesEveryRoute(t *testing.T) {
	withTestBuiltins(t)
	stale := "-- af-prelude-version: 0\nfunction loop() end\n"
	undeclared := "function loop() end\n"

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "loop.lua"), stale)
	members := filepath.Join(dir, "members")
	writeFile(t, filepath.Join(members, "10-main.lua"), directive()+"function loop() end\n")
	writeFile(t, filepath.Join(members, "20-tools.lua"), stale)
	writeFile(t, filepath.Join(dir, "undeclared.lua"), undeclared)

	shadows := t.TempDir()
	writeFile(t, filepath.Join(shadows, "beta.lua"), stale)
	SetPluginDir(shadows)

	for _, c := range []struct {
		name   string
		ref    string
		wantIn []string
	}{
		{"shadowed builtin", "plugin:beta", []string{filepath.Join(shadows, "beta.lua")}},
		{"single file", filepath.Join(dir, "loop.lua"), []string{filepath.Join(dir, "loop.lua")}},
		{"directory loop", members, []string{filepath.Join(members, "20-tools.lua")}},
		{"no declaration", filepath.Join(dir, "undeclared.lua"), []string{filepath.Join(dir, "undeclared.lua"), "af-prelude-version"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			src, _, err := Resolve(c.ref)
			if err == nil || !errors.Is(err, vm.ErrPreludeVersion) || src != "" {
				t.Fatalf("Resolve(%s) = (%q, %v); want ErrPreludeVersion", c.ref, src, err)
			}
			for _, want := range c.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("err %v missing %q", err, want)
				}
			}
		})
	}

	if chunks, err := SupportChunks(); err == nil || !errors.Is(err, vm.ErrPreludeVersion) || chunks != nil {
		t.Fatalf("SupportChunks() = (%v, %v); want ErrPreludeVersion", chunks, err)
	}
}

func TestPackageImportAndCycleDetection(t *testing.T) {
	withTestBuiltins(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a", "a.lua"), directive()+"-- af-import: b\nA = B + 1\n")
	writeFile(t, filepath.Join(dir, "b", "b.lua"), directive()+"B = 41\n")
	writeFile(t, filepath.Join(dir, "main.lua"), directive()+"-- af-import: a\nfunction loop() return A end\n")

	SetPackages(map[string]string{
		"a": filepath.Join(dir, "a"),
		"b": filepath.Join(dir, "b"),
	})
	src, watch, err := Resolve(filepath.Join(dir, "main.lua"))
	if err != nil {
		t.Fatal(err)
	}
	if watch != filepath.Join(dir, "main.lua") {
		t.Fatalf("watch = %q", watch)
	}
	if !strings.Contains(src, "B = 41") || !strings.Contains(src, "A = B + 1") {
		t.Fatalf("expanded source missing package bodies:\n%s", src)
	}
	if strings.Count(src, "af-prelude-version") != 1 {
		t.Fatalf("expected stripped duplicate directives, got:\n%s", src)
	}

	// Cycle detection.
	writeFile(t, filepath.Join(dir, "b", "b.lua"), directive()+"-- af-import: a\nB = 1\n")
	if _, _, err := Resolve(filepath.Join(dir, "main.lua")); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}
