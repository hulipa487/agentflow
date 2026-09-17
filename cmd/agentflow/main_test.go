package main

import (
	"os"
	"path/filepath"
	"testing"

	"agentflow/internal/config"
)

// TestTriggerTarget: a scheduled trigger's target.profile resolves to a
// configured agent by name, or to the "spawn:<profile>" address the supervisor
// registers for a spawn profile. Anything else is unresolvable (the trigger
// scheduler logs it and skips the trigger — never a boot failure).
func TestTriggerTarget(t *testing.T) {
	cfg := &config.Config{
		Agents:   map[string]config.Agent{"digest": {Loop: "x.lua"}},
		Profiles: config.Profiles{Agent: map[string]config.SpawnProfile{"pm": {Loop: "y.lua"}}},
	}
	cases := []struct {
		profile string
		want    string
		ok      bool
	}{
		{"digest", "digest", true},
		{"pm", "spawn:pm", true},
		{"nope", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := triggerTarget(cfg, c.profile)
		if ok != c.ok || got != c.want {
			t.Fatalf("triggerTarget(%q) = %q, %v; want %q, %v", c.profile, got, ok, c.want, c.ok)
		}
	}
}

// TestResolveInstructions: instructions source from either a file path or a
// prompts: registry key. The registry text becomes the agent's system prompt,
// and the key is reported separately so agent.config() can distinguish a
// prompt-sourced prompt from a file-sourced one. An unreadable file and an
// unknown key are both boot errors, never a silently empty system prompt.
func TestResolveInstructions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sys.md")
	if err := os.WriteFile(path, []byte("from file"), 0o600); err != nil {
		t.Fatal(err)
	}
	prompts := map[string]config.Prompt{"sys": {Inline: "from registry", Content: "from registry"}}

	text, key, err := resolveInstructions(config.InstructionsRef{File: path}, prompts)
	if err != nil || text != "from file" || key != "" {
		t.Fatalf("file source: text=%q key=%q err=%v", text, key, err)
	}
	text, key, err = resolveInstructions(config.InstructionsRef{Prompt: "sys"}, prompts)
	if err != nil || text != "from registry" || key != "sys" {
		t.Fatalf("prompt source: text=%q key=%q err=%v", text, key, err)
	}
	if _, _, err := resolveInstructions(config.InstructionsRef{Prompt: "nope"}, prompts); err == nil {
		t.Fatal("unknown prompt key must fail")
	}
	if _, _, err := resolveInstructions(config.InstructionsRef{File: filepath.Join(dir, "missing.md")}, prompts); err == nil {
		t.Fatal("unreadable instructions file must fail")
	}
	text, key, err = resolveInstructions(config.InstructionsRef{}, prompts)
	if err != nil || text != "" || key != "" {
		t.Fatalf("empty ref: text=%q key=%q err=%v", text, key, err)
	}
}

// TestCheckConfigSource: -config and -configdir are mutually exclusive, but
// the default -config value alone (never explicitly passed) does not collide
// with -configdir.
func TestCheckConfigSource(t *testing.T) {
	if err := checkConfigSource(map[string]bool{"configdir": true}); err != nil {
		t.Fatalf("-configdir alone must be legal: %v", err)
	}
	if err := checkConfigSource(map[string]bool{"config": true}); err != nil {
		t.Fatalf("-config alone must be legal: %v", err)
	}
	if err := checkConfigSource(map[string]bool{}); err != nil {
		t.Fatalf("no config flags must be legal: %v", err)
	}
	if err := checkConfigSource(map[string]bool{"config": true, "configdir": true}); err == nil {
		t.Fatal("-config and -configdir together must fail")
	}
}
