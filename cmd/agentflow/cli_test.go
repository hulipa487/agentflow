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
	got = hoistConfigFlags([]string{"-config", "deploy/agentflow.yaml", "-log-level", "debug"})
	if strings.Join(got, " ") != "-config deploy/agentflow.yaml -log-level debug" {
		t.Fatalf("engine args changed: %v", got)
	}

	// init takes no -config, so a leading one is left for init to refuse.
	got = hoistConfigFlags([]string{"-config", "x", "init"})
	if strings.Join(got, " ") != "-config x init" {
		t.Fatalf("init args changed: %v", got)
	}

	// Group commands get it after the whole subcommand.
	got = hoistConfigFlags([]string{"-config", "d/", "config", "show"})
	if strings.Join(got, " ") != "config show -config d/" {
		t.Fatalf("group hoist = %v", got)
	}
}

func TestHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := cmdHelp(nil, &out, &errOut); code != 0 {
		t.Fatalf("help exit = %d", code)
	}
	for _, want := range []string{"agentflow help", "commands:", "run ", "support-bundle"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("general help missing %q", want)
		}
	}
	// The engine's flags are run's help, generated from its flag set.
	out.Reset()
	if code := cmdHelp([]string{"run"}, &out, &errOut); code != 0 {
		t.Fatalf("run help exit = %d", code)
	}
	for _, want := range []string{"usage: agentflow run [flags]", "-config", "-log-level", "-force-single"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("run help missing %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if code := cmdHelp([]string{"config"}, &out, &errOut); code != 0 {
		t.Fatalf("config help exit = %d", code)
	}
	if !strings.Contains(out.String(), "schema") || !strings.Contains(out.String(), "show") {
		t.Errorf("config help missing its commands: %s", out.String())
	}
	out.Reset()
	if code := cmdHelp([]string{"config", "show"}, &out, &errOut); code != 0 {
		t.Fatalf("config show help exit = %d", code)
	}
	if !strings.Contains(out.String(), "usage: agentflow config show [flags]") || !strings.Contains(out.String(), "-json") {
		t.Errorf("config show help: %s", out.String())
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
	if code := cmdInit([]string{dir, "-layout", "dir"}, &out, &errOut); code != 0 {
		t.Fatalf("init exit = %d: %s", code, errOut.String())
	}
	for _, name := range []string{"system.yaml", "channels.yaml", filepath.Join("profiles", "echo.yaml")} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !strings.Contains(out.String(), "agentflow run -config "+dir) {
		t.Errorf("init output does not point at run -config: %s", out.String())
	}

}

// What init writes must load on the binary that wrote it, in both layouts:
// a starter loop declaring a stale prelude version is a first run that fails.
func TestInitOutputValidates(t *testing.T) {
	for _, layout := range []string{"file", "dir"} {
		t.Run(layout, func(t *testing.T) {
			dir := t.TempDir()
			var out, errOut bytes.Buffer
			if code := cmdInit([]string{dir, "-layout", layout}, &out, &errOut); code != 0 {
				t.Fatalf("init exit = %d: %s", code, errOut.String())
			}
			// -config DIR loads either layout: the file inside it, or the
			// config directory.
			args := []string{"-config", dir}
			out.Reset()
			errOut.Reset()
			if code := cmdValidate(args, &out, &errOut); code != 0 {
				t.Fatalf("validate on init output exit = %d: %s%s", code, out.String(), errOut.String())
			}
		})
	}
}

// The checked-in example is the schema's front door: keep it loading and
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
}

func TestLoopCheckAndLoopTestCLI(t *testing.T) {
	dir := t.TempDir()
	loopPath := filepath.Join(dir, "echo.lua")
	loopSrc := "-- af-prelude-version: 1\nfunction loop()\n  local msg = session.inbox()\n  session.send(\"echo: \" .. (msg.text or \"\"))\nend\n"
	if err := os.WriteFile(loopPath, []byte(loopSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := cmdLoopCheck([]string{loopPath}, &out, &errOut); code != 0 {
		t.Fatalf("loop check exit = %d: %s%s", code, out.String(), errOut.String())
	}

	scenPath := filepath.Join(dir, "echo.yaml")
	scen := "loop: " + filepath.ToSlash(loopPath) + "\nturns:\n  - in: \"hi\"\n    expect_sends:\n      - \"echo: hi\"\n"
	if err := os.WriteFile(scenPath, []byte(scen), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := cmdLoopTest([]string{scenPath}, &out, &errOut); code != 0 {
		t.Fatalf("loop test exit = %d: %s%s", code, out.String(), errOut.String())
	}
}
