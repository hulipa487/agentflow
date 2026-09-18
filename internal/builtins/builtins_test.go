package builtins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPluginDirShadowsBuiltin: a file <plugins.dir>/<name>.lua wins over the
// embedded builtin — for Resolve (loop/route refs, with a watch path for hot
// reload) and for SupportChunks. Unshadowed builtins fall back to embedded.
func TestPluginDirShadowsBuiltin(t *testing.T) {
	dir := t.TempDir()
	shadowSrc := "-- deployment override\nmemory_recall_handler = function(q, o) return {} end\n"
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
	chunks := SupportChunks()
	if chunks[2] != shadowSrc {
		t.Fatalf("support chunk not shadowed:\n%s", chunks[2])
	}
	if len(chunks) != len(supportOrder) {
		t.Fatalf("chunks = %d, want %d", len(chunks), len(supportOrder))
	}

	// An unshadowed builtin still resolves to the embedded source, unwatched.
	src, watch, err = Resolve("plugin:ttl")
	if err != nil {
		t.Fatal(err)
	}
	if src != sources["ttl"] || watch != "" {
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
	if got := SupportChunks()[2]; got != sources["recency"] {
		t.Fatal("support chunks must be embedded without plugins.dir")
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
