package builtins

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/vm"
)

// TestPluginDirShadowsBuiltin: a file <plugins.dir>/<name>.lua wins over the
// embedded builtin — for Resolve (loop/route refs, with a watch path for hot
// reload) and for SupportChunks. Unshadowed builtins fall back to embedded.
func TestPluginDirShadowsBuiltin(t *testing.T) {
	dir := t.TempDir()
	shadowSrc := directive() + "memory_recall_handler = function(q, o) return {} end\n"
	if err := os.WriteFile(filepath.Join(dir, "recency.lua"), []byte(shadowSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	SetPluginDir(dir)
	defer SetPluginDir("")

	src, watch, err := Resolve("plugin:recency")
	if err != nil {
		t.Fatal(err)
	}
	if src != shadowSrc {
		t.Fatalf("shadow not used:\n%s", src)
	}
	if watch != filepath.Join(dir, "recency.lua") {
		t.Fatalf("shadow watch path = %q", watch)
	}

	// SupportChunks carries the shadow in recency's position (3rd).
	chunks, err := SupportChunks()
	if err != nil {
		t.Fatal(err)
	}
	if chunks[2] != shadowSrc {
		t.Fatalf("support chunk not shadowed:\n%s", chunks[2])
	}
	if len(chunks) != len(supportOrder) {
		t.Fatalf("chunks = %d, want %d", len(chunks), len(supportOrder))
	}

	// An unshadowed builtin still resolves to the embedded source, unwatched.
	src, watch, err = Resolve("plugin:semantic")
	if err != nil {
		t.Fatal(err)
	}
	if src != sources["semantic"] || watch != "" {
		t.Fatal("unshadowed builtin must use the embedded source")
	}
}

// TestNoPluginDirKeepsEmbedded: with no plugins.dir set, behavior is exactly
// the pre-shadowing loader.
func TestNoPluginDirKeepsEmbedded(t *testing.T) {
	SetPluginDir("")
	src, watch, err := Resolve("plugin:recency")
	if err != nil {
		t.Fatal(err)
	}
	if src != sources["recency"] || watch != "" {
		t.Fatal("embedded builtin must resolve unwatched without plugins.dir")
	}
	chunks, err := SupportChunks()
	if err != nil {
		t.Fatal(err)
	}
	if got := chunks[2]; got != sources["recency"] {
		t.Fatal("support chunks must be embedded without plugins.dir")
	}
}

// TestSupportChunkPathsListsOnlyShadows: the reload watcher polls the paths
// this returns, so it has to name exactly the chunks that are files on disk —
// a shadow. An embedded chunk is not a path and must not be reported as one, or
// the watcher would poll something that does not exist.
func TestSupportChunkPathsListsOnlyShadows(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "recency.lua"), []byte("-- override\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	SetPluginDir(dir)
	defer SetPluginDir("")

	paths := SupportChunkPaths()
	if len(paths) != 1 {
		t.Fatalf("paths = %v, want only the shadowed chunk", paths)
	}
	if got := paths["recency"]; got != filepath.Join(dir, "recency.lua") {
		t.Fatalf("recency path = %q", got)
	}
	// Every other chunk is embedded: no file, nothing to watch.
	for _, name := range supportOrder {
		if name == "recency" {
			continue
		}
		if _, ok := paths[name]; ok {
			t.Fatalf("embedded chunk %s reported as a watched path", name)
		}
	}

	// Without a plugins.dir there are no shadows at all.
	SetPluginDir("")
	if got := SupportChunkPaths(); len(got) != 0 {
		t.Fatalf("paths without plugins.dir = %v", got)
	}
}

// TestUnknownBuiltinStillRejected: shadowing does not invent plugins. The
// error names the prefix the caller actually wrote, and gives a working
// example, because "unknown plugin:per_chat" alone leaves you guessing at the
// spelling.
func TestUnknownBuiltinStillRejected(t *testing.T) {
	SetPluginDir(t.TempDir())
	defer SetPluginDir("")
	if _, _, err := Resolve("plugin:nope"); err == nil || !strings.Contains(err.Error(), "unknown plugin") {
		t.Fatalf("expected unknown builtin error, got %v", err)
	}
}

// TestVersionGateRefusesEveryRoute: each way a chunk can reach a session — a
// shadowed builtin, a single file, a directory, a directory member — is refused
// when the chunk targets another prelude version, or declares none. The gate is
// only worth having if it covers every one of them: a route that skips it is a
// route a core upgrade breaks silently, which is the failure the version exists
// to prevent.
func TestVersionGateRefusesEveryRoute(t *testing.T) {
	stale := "-- af-prelude-version: 0\nfunction loop() end\n"
	undeclared := "function loop() end\n"

	// The directory case is the one worth being careful about: the first member
	// is correct and only the second is stale. Checking the concatenation would
	// pass, because the first member's directive is what leads the joined
	// source.
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "loop.lua"), stale)
	members := filepath.Join(dir, "members")
	if err := os.Mkdir(members, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(members, "10-main.lua"), directive()+"function loop() end\n")
	writeFile(t, filepath.Join(members, "20-tools.lua"), stale)
	writeFile(t, filepath.Join(dir, "undeclared.lua"), undeclared)

	shadows := t.TempDir()
	writeFile(t, filepath.Join(shadows, "recency.lua"), stale)
	SetPluginDir(shadows)
	defer SetPluginDir("")

	cases := []struct {
		name   string
		ref    string
		wantIn []string // fragments the refusal must carry, so it is actionable
	}{
		{"shadowed builtin", "plugin:recency", []string{filepath.Join(shadows, "recency.lua")}},
		{"single file", filepath.Join(dir, "loop.lua"), []string{filepath.Join(dir, "loop.lua")}},
		// The refusal names the *member*, not the directory: that is the file
		// the operator has to open.
		{"directory loop", members, []string{filepath.Join(members, "20-tools.lua")}},
		{"no declaration", filepath.Join(dir, "undeclared.lua"), []string{
			filepath.Join(dir, "undeclared.lua"), "af-prelude-version",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src, watch, err := Resolve(c.ref)
			if err == nil {
				t.Fatalf("Resolve(%s) returned source (watch path %q) for a chunk this core cannot serve", c.ref, watch)
			}
			if !errors.Is(err, vm.ErrPreludeVersion) {
				t.Fatalf("Resolve(%s) failed with %v; want a version refusal the hot-reload path can recognise", c.ref, err)
			}
			if src != "" {
				t.Fatalf("Resolve(%s) returned source alongside the refusal", c.ref)
			}
			for _, want := range c.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal does not mention %q, so it cannot be acted on: %v", want, err)
				}
			}
		})
	}

	// The gate must not refuse what it is supposed to serve.
	if _, _, err := Resolve("plugin:semantic"); err != nil {
		t.Fatalf("an embedded builtin at this core's own version was refused: %v", err)
	}
	writeFile(t, filepath.Join(dir, "ok.lua"), directive()+"function loop() end\n")
	if _, _, err := Resolve(filepath.Join(dir, "ok.lua")); err != nil {
		t.Fatalf("a chunk declaring this core's version was refused: %v", err)
	}
}

// TestVersionGateCoversSupportChunks: the gate reaches the support chunks too,
// shadowed or embedded. A support chunk is not a loop/route ref — nothing
// resolves it — so SupportChunks is the only way one reaches a session, and a
// shadow that skipped the gate there would be Lua running against an API it was
// not written for, in every session of the deployment at once.
func TestVersionGateCoversSupportChunks(t *testing.T) {
	dir := t.TempDir()
	SetPluginDir(dir)
	defer SetPluginDir("")

	// The unexceptional case first: no shadow, so the embedded chunks load.
	if _, err := SupportChunks(); err != nil {
		t.Fatalf("the embedded support chunks were refused: %v", err)
	}

	for _, c := range []struct{ name, src string }{
		{"no declaration", "SUPPORT = 1\n"},
		{"another version", "-- af-prelude-version: 0\nSUPPORT = 1\n"},
		{"declared twice", "-- af-prelude-version: 1\n-- af-prelude-version: 2\nSUPPORT = 1\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			writeFile(t, filepath.Join(dir, "recency.lua"), c.src)
			chunks, err := SupportChunks()
			if err == nil {
				t.Fatalf("a shadowed support chunk this core cannot serve was loaded (%d chunks)", len(chunks))
			}
			if !errors.Is(err, vm.ErrPreludeVersion) {
				t.Fatalf("refusal does not wrap ErrPreludeVersion: %v", err)
			}
			if chunks != nil {
				t.Fatalf("source returned alongside the refusal: %v", chunks)
			}
			// The refusal names the file the operator has to edit.
			if !strings.Contains(err.Error(), filepath.Join(dir, "recency.lua")) {
				t.Fatalf("refusal does not name the shadow: %v", err)
			}
		})
	}

	// The shadow was the only thing wrong: fix it and the chunks load again.
	writeFile(t, filepath.Join(dir, "recency.lua"), directive()+"SUPPORT = 1\n")
	chunks, err := SupportChunks()
	if err != nil {
		t.Fatalf("a shadow declaring this core's version was refused: %v", err)
	}
	if chunks[2] != directive()+"SUPPORT = 1\n" {
		t.Fatalf("the accepted shadow is not the one that loaded:\n%s", chunks[2])
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
