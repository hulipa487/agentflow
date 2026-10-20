package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveSourceFileAndDir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "agentflow.yaml")
	if err := os.WriteFile(file, []byte("version: \"1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveSource(file, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.IsDir || got.Path != file || got.Dir != dir {
		t.Fatalf("file source = %+v", got)
	}
	if got.ConfigDir() != "" || got.ConfigPath() != file {
		t.Fatalf("file accessors = %q/%q", got.ConfigDir(), got.ConfigPath())
	}

	got, err = ResolveSource("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsDir || got.Dir != dir {
		t.Fatalf("dir source = %+v", got)
	}
	if got.ConfigDir() != dir || got.ConfigPath() != "" {
		t.Fatalf("dir accessors = %q/%q", got.ConfigDir(), got.ConfigPath())
	}

	// -config accepts a directory too, so the two spellings agree.
	got, err = ResolveSource(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsDir || got.Dir != dir {
		t.Fatalf("-config directory = %+v", got)
	}
}

func TestResolveSourceDiscovery(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if _, err := ResolveSource("", ""); err == nil {
		t.Fatal("ResolveSource succeeded with no config present")
	}

	if err := os.WriteFile("config.yaml", []byte("version: \"1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveSource("", "")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Legacy || got.Path != filepath.Join(dir, "config.yaml") {
		t.Fatalf("legacy discovery = %+v", got)
	}

	// agentflow.yaml wins when both exist.
	if err := os.WriteFile("agentflow.yaml", []byte("version: \"1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = ResolveSource("", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Legacy || got.Path != filepath.Join(dir, "agentflow.yaml") {
		t.Fatalf("canonical discovery = %+v", got)
	}
}

func TestResolveSourceRejectsConflictAndMissing(t *testing.T) {
	if _, err := ResolveSource("a", "b"); err == nil {
		t.Fatal("both -config and -configdir accepted")
	}
	if _, err := ResolveSource(filepath.Join(t.TempDir(), "missing.yaml"), ""); err == nil {
		t.Fatal("missing explicit config accepted")
	}
}

// A single-file config resolves its loop and prompt paths against its own
// directory, not the process CWD.
func TestLoadRebasesSingleFilePaths(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "loops"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "sys.md"), []byte("be brief"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "agentflow.yaml")
	body := `version: "1"
prompts:
  sys: { file: ./prompts/sys.md }
agents:
  echo:
    loop: ./loops/echo.lua
    instructions: { prompt: sys }
gateway:
  route: ./routes/main.lua
plugins:
  dir: ./plugins
packages:
  shared: ./packages/shared
runtime:
  persistence: sqlite://./data/agentflow.db
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	want := func(rel string) string { return filepath.Join(dir, rel) }
	if got := cfg.Agents["echo"].Loop; got != want("loops/echo.lua") {
		t.Errorf("agent loop = %q, want %q", got, want("loops/echo.lua"))
	}
	if got := cfg.Prompts["sys"].File; got != want("prompts/sys.md") {
		t.Errorf("prompt file = %q, want %q", got, want("prompts/sys.md"))
	}
	if got := cfg.Gateway.Route; got != want("routes/main.lua") {
		t.Errorf("route = %q, want %q", got, want("routes/main.lua"))
	}
	if got := cfg.Plugins.Dir; got != want("plugins") {
		t.Errorf("plugins dir = %q, want %q", got, want("plugins"))
	}
	if got := cfg.Packages["shared"]; got != want("packages/shared") {
		t.Errorf("package = %q, want %q", got, want("packages/shared"))
	}
	if got, wantP := cfg.Runtime.Persistence, "sqlite://"+want("data/agentflow.db"); got != wantP {
		t.Errorf("persistence = %q, want %q", got, wantP)
	}
}

func TestRebasePersistenceSchemes(t *testing.T) {
	base := t.TempDir()
	if got, want := rebasePersistence(base, "postgres://u:p@db.example/app"), "postgres://u:p@db.example/app"; got != want {
		t.Errorf("postgres DSN = %q, want %q", got, want)
	}
	if got, want := rebasePersistence(base, "sqlite://./data/app.db"), "sqlite://"+filepath.Join(base, "data/app.db"); got != want {
		t.Errorf("sqlite target = %q, want %q", got, want)
	}
	if got, want := rebasePersistence(base, "file://./logs"), "file://"+filepath.Join(base, "logs"); got != want {
		t.Errorf("file target = %q, want %q", got, want)
	}
	if got := rebasePersistence(base, "relative/path.db"); got != filepath.Join(base, "relative/path.db") {
		t.Errorf("bare path = %q", got)
	}
}

// An empty deployment is a control-plane deployment: it must load so Studio
// can create the first agent, while references to a missing agent still fail.
func TestLoadAllowsNoAgents(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agentflow.yaml")
	body := "version: \"1\"\nruntime:\n  persistence: sqlite://./data/empty.db\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("empty deployment did not load: %v", err)
	}
	if len(cfg.Agents) != 0 {
		t.Fatalf("agents = %d, want 0", len(cfg.Agents))
	}
}
