package studio

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/vm"
)

func testLoop(body string) string {
	return fmt.Sprintf("-- af-prelude-version: %d\n", vm.PreludeVersion) + body
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestConfigDirRoundTrip: a studio agent is written as a profile fragment plus
// loop and prompt files, loads back, and deletes cleanly.
func TestConfigDirRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "system.yaml"), "version: \"1\"\n")

	ws := NewWorkspace(dir, "", "", nil, discard())
	agent := config.Agent{Loop: "ignored", Capabilities: []string{"tools", "llm.chat"}}
	loop := testLoop("function loop()\n  local m = session.inbox()\n  session.send(m.text)\nend\n")
	files, err := ws.Save("echo", agent, loop, "Be terse.", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if files.Mode != "configdir" || !files.Managed {
		t.Fatalf("files = %+v", files)
	}
	if !fileExists(files.LoopPath) || !fileExists(files.InstructionsPath) {
		t.Fatalf("loop/prompt not written: %+v", files)
	}
	raw, err := os.ReadFile(files.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), managedMarker) {
		t.Fatalf("profile lacks the managed marker:\n%s", raw)
	}

	got, loop2, instr, f2, err := ws.Load("echo")
	if err != nil {
		t.Fatal(err)
	}
	if got.Loop != "loops/echo.lua" {
		t.Errorf("persisted loop = %q", got.Loop)
	}
	if got.Instructions.File != "prompts/echo.md" {
		t.Errorf("persisted instructions = %+v", got.Instructions)
	}
	if loop2 != loop || instr != "Be terse." {
		t.Errorf("loaded sources differ")
	}
	if !f2.Managed {
		t.Errorf("loaded workspace is not marked managed")
	}

	// The whole directory still loads with the fragment present.
	if _, err := config.LoadDir(dir, discard()); err != nil {
		t.Fatalf("configdir does not load: %v", err)
	}

	removed, err := ws.Remove("echo", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 3 {
		t.Errorf("removed %v, want profile+loop+prompt", removed)
	}
	if _, _, _, _, err := ws.Load("echo"); err == nil {
		t.Error("agent still loads after removal")
	}
}

func TestConfigDirRefusesUnmanagedProfile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "system.yaml"), "version: \"1\"\n")
	writeFile(t, filepath.Join(dir, "profiles", "hand.yaml"), "name: hand\nloop: ./hand.lua\n")

	ws := NewWorkspace(dir, "", "", nil, discard())
	agent := config.Agent{Loop: "x"}
	_, err := ws.Save("hand", agent, testLoop("function loop() end\n"), "", 0, false)
	if err != ErrNotManaged {
		t.Fatalf("Save error = %v, want ErrNotManaged", err)
	}
	if _, err := ws.Remove("hand", false); err != ErrNotManaged {
		t.Fatalf("Remove error = %v, want ErrNotManaged", err)
	}
	if _, err := ws.Save("hand", agent, testLoop("function loop() end\n"), "", 0, true); err != nil {
		t.Fatalf("force Save: %v", err)
	}
}

// TestSingleFileRoundTrip: a single-file deployment gets the agent added to
// agents: and the loop/prompt under the studio workspace.
func TestSingleFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agentflow.yaml")
	writeFile(t, cfgPath, "version: \"1\"\nagents:\n  base:\n    loop: ./base.lua\n")

	ws := NewWorkspace("", cfgPath, filepath.Join(dir, "studio"), nil, discard())
	agent := config.Agent{Loop: "ignored"}
	loop := testLoop("function loop()\n  local m = session.inbox()\n  session.send(m.text)\nend\n")
	files, err := ws.Save("lab", agent, loop, "Be brief.", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !fileExists(files.LoopPath) {
		t.Fatalf("loop not written at %s", files.LoopPath)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("persisted config does not load: %v", err)
	}
	lab, ok := cfg.Agents["lab"]
	if !ok {
		t.Fatal("agent lab missing from the persisted config")
	}
	if !filepath.IsAbs(lab.Loop) {
		t.Errorf("single-file loop path = %q, want absolute", lab.Loop)
	}
	if lab.Instructions.File == "" {
		t.Errorf("instructions path missing")
	}

	if _, err := ws.Remove("lab", false); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Agents["lab"]; ok {
		t.Fatal("agent lab still in config after removal")
	}
	if _, ok := cfg.Agents["base"]; !ok {
		t.Fatal("removing lab removed the unrelated base agent")
	}
}

func TestSaveRefusesStaleMtime(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "system.yaml"), "version: \"1\"\n")
	ws := NewWorkspace(dir, "", "", nil, discard())
	agent := config.Agent{Loop: "x"}
	loop := testLoop("function loop() end\n")
	if _, err := ws.Save("a", agent, loop, "", 0, false); err != nil {
		t.Fatal(err)
	}
	f := ws.Files("a")
	_, err := ws.Save("a", agent, loop, "", f.Mtime+1, false)
	if err != ErrConflict {
		t.Fatalf("stale save error = %v, want ErrConflict", err)
	}
}

// Studio must be able to create the first agent in a config that has no
// agents: section at all.
func TestSingleFileSaveCreatesAgentsSection(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agentflow.yaml")
	writeFile(t, cfgPath, "version: \"1\"\nruntime:\n  persistence: sqlite://./data/empty.db\n")

	ws := NewWorkspace("", cfgPath, filepath.Join(dir, "studio"), nil, discard())
	files, err := ws.Save("first", config.Agent{}, testLoop("function loop()\n  session.send(\"ready\")\nend\n"), "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !fileExists(files.LoopPath) {
		t.Fatalf("loop not written at %s", files.LoopPath)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("persisted empty-start config does not load: %v", err)
	}
	if _, ok := cfg.Agents["first"]; !ok {
		t.Fatalf("first agent missing after save: %+v", cfg.Agents)
	}
}
