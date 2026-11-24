package builtins

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/luacode"
	"agentflow/internal/vm"
)

// TestPluginDirShadowsBuiltin: a file <plugins.dir>/<name>.ts (or .js) wins
// over the embedded builtin — for Resolve (loop/route refs, with a watch path
// for hot reload) and for SupportChunks. Unshadowed builtins fall back to embedded.
func TestPluginDirShadowsBuiltin(t *testing.T) {
	Register()
	dir := t.TempDir()
	shadowSrc := directive() + "function memory_recall_handler(_q: unknown, _o: unknown) { return []; }\n"
	if err := os.WriteFile(filepath.Join(dir, "recency.ts"), []byte(shadowSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	luacode.SetPluginDir(dir)
	defer luacode.SetPluginDir("")

	src, watch, err := luacode.Resolve("plugin:recency")
	if err != nil {
		t.Fatal(err)
	}
	if src != shadowSrc {
		t.Fatalf("shadow not used:\n%s", src)
	}
	if watch != filepath.Join(dir, "recency.ts") {
		t.Fatalf("shadow watch path = %q", watch)
	}

	// SupportChunks carries the shadow in recency's position (3rd).
	chunks, err := luacode.SupportChunks()
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
	src, watch, err = luacode.Resolve("plugin:semantic")
	if err != nil {
		t.Fatal(err)
	}
	if src != sources["semantic"] || watch != "" {
		t.Fatal("unshadowed builtin must use the embedded source")
	}
}

// TestNoPluginDirKeepsEmbedded: with no plugins.dir set, embedded builtins
// resolve unwatched.
func TestNoPluginDirKeepsEmbedded(t *testing.T) {
	Register()
	luacode.SetPluginDir("")
	src, watch, err := luacode.Resolve("plugin:recency")
	if err != nil {
		t.Fatal(err)
	}
	if src != sources["recency"] || watch != "" {
		t.Fatal("embedded builtin must resolve unwatched without plugins.dir")
	}
	chunks, err := luacode.SupportChunks()
	if err != nil {
		t.Fatal(err)
	}
	if got := chunks[2]; got != sources["recency"] {
		t.Fatal("support chunks must be embedded without plugins.dir")
	}
}

// TestSupportChunkPathsListsOnlyShadows: the reload watcher polls the paths
// this returns, so it only reports shadowed chunks on disk.
func TestSupportChunkPathsListsOnlyShadows(t *testing.T) {
	Register()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "recency.ts"), []byte("// override\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	luacode.SetPluginDir(dir)
	defer luacode.SetPluginDir("")

	paths := luacode.SupportChunkPaths()
	if len(paths) != 1 {
		t.Fatalf("paths = %v, want only the shadowed chunk", paths)
	}
	if got := paths["recency"]; got != filepath.Join(dir, "recency.ts") {
		t.Fatalf("recency path = %q", got)
	}
	for _, name := range supportOrder {
		if name == "recency" {
			continue
		}
		if _, ok := paths[name]; ok {
			t.Fatalf("embedded chunk %s reported as a watched path", name)
		}
	}

	luacode.SetPluginDir("")
	if got := luacode.SupportChunkPaths(); len(got) != 0 {
		t.Fatalf("paths without plugins.dir = %v", got)
	}
}

func TestUnknownBuiltinStillRejected(t *testing.T) {
	Register()
	luacode.SetPluginDir(t.TempDir())
	defer luacode.SetPluginDir("")
	if _, _, err := luacode.Resolve("plugin:nope"); err == nil || !strings.Contains(err.Error(), "unknown plugin") {
		t.Fatalf("expected unknown builtin error, got %v", err)
	}
}

func TestVersionGateCoversSupportChunks(t *testing.T) {
	Register()
	dir := t.TempDir()
	luacode.SetPluginDir(dir)
	defer luacode.SetPluginDir("")

	if _, err := luacode.SupportChunks(); err != nil {
		t.Fatalf("the embedded support chunks were refused: %v", err)
	}

	for _, c := range []struct{ name, src string }{
		{"no declaration", "var SUPPORT = 1;\n"},
		{"another version", "// af-prelude-version: 0\nvar SUPPORT = 1;\n"},
		{"declared twice", "// af-prelude-version: 1\n// af-prelude-version: 2\nvar SUPPORT = 1;\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			writeFile(t, filepath.Join(dir, "recency.ts"), c.src)
			chunks, err := luacode.SupportChunks()
			if err == nil {
				t.Fatalf("a shadowed support chunk this core cannot serve was loaded (%d chunks)", len(chunks))
			}
			if !errors.Is(err, vm.ErrPreludeVersion) {
				t.Fatalf("refusal does not wrap ErrPreludeVersion: %v", err)
			}
			if chunks != nil {
				t.Fatalf("source returned alongside the refusal: %v", chunks)
			}
			if !strings.Contains(err.Error(), filepath.Join(dir, "recency.ts")) {
				t.Fatalf("refusal does not name the shadow: %v", err)
			}
		})
	}

	writeFile(t, filepath.Join(dir, "recency.ts"), directive()+"var SUPPORT = 1;\n")
	chunks, err := luacode.SupportChunks()
	if err != nil {
		t.Fatalf("a shadow declaring this core's version was refused: %v", err)
	}
	if chunks[2] != directive()+"var SUPPORT = 1;\n" {
		t.Fatalf("the accepted shadow is not the one that loaded:\n%s", chunks[2])
	}
}

// TestPerChatRoutesWithoutDurableMode verifies per_chat handles null router.state.get
// without failing on a first message of a new chat.
func TestPerChatRoutesWithoutDurableMode(t *testing.T) {
	Register()
	luacode.SetPluginDir("")
	src, _, err := luacode.Resolve("plugin:per_chat")
	if err != nil {
		t.Fatal(err)
	}

	st := vm.New(5_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}

	stub := `
let calls = 0;
session.inbox = function() {
  calls++;
  if (calls > 1) throw new Error("stop-loop");
  return { message: { id: "m1", type: "user", from: "alice", channel: "webhook" }, agent: "echo" };
};
af.op = function(req) {
  if (req.type === "router.state.get") return null;
  if (req.type === "deliver") return true;
  return null;
};
`
	if err := st.Eval("@route-stub", stub); err != nil {
		t.Fatal(err)
	}
	status, msg := st.Start("loop", "@per_chat", src)
	if status != vm.Failed || !strings.Contains(msg, "stop-loop") {
		t.Fatalf("route did not survive the missing mode and reach its next inbox: status=%v msg=%q", status, msg)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
