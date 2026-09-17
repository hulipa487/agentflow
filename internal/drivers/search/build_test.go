package search

import (
	"bytes"
	"io"
	"log/slog"
	"testing"

	"agentflow/internal/config"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestBuildSkipsUnresolvableKeyedEngine: a keyed engine whose credential does
// not resolve is skipped with a warning (captured here), not a boot error;
// resolution comes from the env or the credential store.
func TestBuildSkipsUnresolvableKeyedEngine(t *testing.T) {
	cfg := config.Search{
		Default: "youtube",
		Engines: map[string]config.SearchEngine{
			"youtube":       {APIKey: "${MISSING_YT_KEY}"},
			"github":        {},                      // unkeyed engine stays
			"stackoverflow": {APIKey: "literal-key"}, // literal stays
		},
	}

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	// No env, no store: yt is skipped, the rest survive, and the default
	// falls back to the sole keyed survivor path (none here -> "").
	set, err := Build(cfg, &config.Resolver{}, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set.Engines["youtube"]; ok {
		t.Fatal("unresolvable keyed engine must be skipped")
	}
	if _, ok := set.Engines["github"]; !ok {
		t.Fatal("unkeyed engine must survive")
	}
	if _, ok := set.Engines["stackoverflow"]; !ok {
		t.Fatal("literal-key engine must survive")
	}
	if set.Default != "" {
		t.Fatalf("default pointing at a skipped engine must clear, got %q", set.Default)
	}
	out := buf.String()
	if !contains(out, "MISSING_YT_KEY") || !contains(out, "skipped") {
		t.Fatalf("warning must name the missing credential: %s", out)
	}

	// Env resolution brings the engine back.
	t.Setenv("MISSING_YT_KEY", "now-present")
	set, err = Build(cfg, &config.Resolver{}, testLog())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set.Engines["youtube"]; !ok {
		t.Fatal("env-resolved engine must build")
	}
	if set.Default != "youtube" {
		t.Fatalf("default restored, got %q", set.Default)
	}
}

// TestBuildSkipsKeylessKeyedEngine: an engine with no key at all degrades the
// same way (the constructor's key requirement never surfaces as a boot
// failure).
func TestBuildSkipsKeylessKeyedEngine(t *testing.T) {
	cfg := config.Search{Engines: map[string]config.SearchEngine{"doubao": {}}}
	set, err := Build(cfg, &config.Resolver{}, testLog())
	if err != nil {
		t.Fatal(err)
	}
	if !set.Empty() {
		t.Fatalf("keyless keyed engine must be skipped, got %v", set.Names())
	}
}

func contains(s, sub string) bool {
	return bytes.Contains([]byte(s), []byte(sub))
}
