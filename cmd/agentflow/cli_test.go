package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/config"
)

func TestHoistConfigFlags(t *testing.T) {
	got := hoistConfigFlags([]string{"-config", "deploy/agentflow.yaml", "validate"})
	want := []string{"validate", "-config", "deploy/agentflow.yaml"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("hoist = %v, want %v", got, want)
	}

	// The engine path is untouched.
	got = hoistConfigFlags([]string{"-config", "deploy/agentflow.yaml", "-no-tui"})
	if strings.Join(got, " ") != "-config deploy/agentflow.yaml -no-tui" {
		t.Fatalf("engine args changed: %v", got)
	}

	// init owns -configdir as a boolean alias; do not reorder it.
	got = hoistConfigFlags([]string{"-configdir", "x", "init"})
	if strings.Join(got, " ") != "-configdir x init" {
		t.Fatalf("init args changed: %v", got)
	}
}

func TestHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := cmdHelp(nil, &out, &errOut); code != 0 {
		t.Fatalf("help exit = %d", code)
	}
	for _, want := range []string{"agentflow help", "-config PATH", "-no-console", "commands:"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("general help missing %q", want)
		}
	}
	out.Reset()
	if code := cmdHelp([]string{"config"}, &out, &errOut); code != 0 {
		t.Fatalf("config help exit = %d", code)
	}
	if !strings.Contains(out.String(), "config schema") {
		t.Errorf("config help missing schema: %s", out.String())
	}

	handled, code := runSubcommand([]string{"nonsense"}, &out, &errOut)
	if !handled || code != 2 {
		t.Fatalf("unknown command: handled=%v code=%d", handled, code)
	}
	if !strings.Contains(errOut.String(), "unknown command") {
		t.Errorf("unknown command message: %s", errOut.String())
	}
}

func TestConfigPathAndShow(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agentflow.yaml")
	body := `version: "1"
agents:
  echo:
    loop: ./loops/echo.lua
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := cmdConfigPath([]string{"-config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("config path exit = %d: %s", code, errOut.String())
	}
	if strings.TrimSpace(out.String()) != cfgPath {
		t.Errorf("config path = %q, want %q", out.String(), cfgPath)
	}

	out.Reset()
	errOut.Reset()
	if code := cmdConfigShow([]string{"-config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("config show exit = %d: %s", code, errOut.String())
	}
	wantLoop := filepath.Join(dir, "loops", "echo.lua")
	if !strings.Contains(out.String(), "loop.echo: "+wantLoop) {
		t.Errorf("config show did not resolve the loop against the config file:\n%s", out.String())
	}
}

func TestInitLayout(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	if code := cmdInit([]string{"-dir", dir, "-layout", "dir"}, &out, &errOut); code != 0 {
		t.Fatalf("init exit = %d: %s", code, errOut.String())
	}
	for _, name := range []string{"system.yaml", "channels.yaml", filepath.Join("profiles", "echo.yaml")} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !strings.Contains(out.String(), "agentflow -config") {
		t.Errorf("init output does not point at -config: %s", out.String())
	}
}

// The checked-in example is the schema's front door: keep it compiling and
// validating as the config changes.
func TestExampleConfigValidates(t *testing.T) {
	path := filepath.Join("..", "..", "agentflow.example.yaml")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("example config does not load: %v", err)
	}
	if err := validateLoops(cfg); err != nil {
		t.Fatalf("example config loops do not compile: %v", err)
	}
	if len(cfg.Agents) == 0 {
		t.Fatal("example config has no agents")
	}
}
