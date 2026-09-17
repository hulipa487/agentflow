package config

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDir materializes a configdir layout for a test.
func writeDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

const dirSystem = `
version: "1"
models:
  default: { provider: openai, model: gpt-4o-mini, base_url: http://127.0.0.1:11434/v1 }
memory:
  backends:
    main_db: { provider: builtin:sqlite, config: { path: ./data/agentflow.db } }
profiles:
  shell:
    box: { provider: ssh, host: 10.0.0.1, user: ops, key_file: ./keys/box.pem }
gateway:
  route: ./routes/route.lua
`

const dirChannels = `
channels:
  - { name: wh, type: webhook, agent: greeter, path: /hook/ }
`

const dirProfileGreeter = `
name: greeter
loop: ./loops/greeter.lua
instructions: ./prompts/greeter.md
model: default
can_contact: [worker]
`

const dirProfileWorker = `
name: worker
spawn: true
loop: ./loops/worker.lua
model: default
can_contact: [greeter]
budget: { tokens_per_day: 5000, window: 168h }
`

const dirTriggersA = `
triggers:
  - name: digest
    cron: "0 9 * * *"
    run_on_boot: true
    target: { profile: worker }
    payload: { topic: news }
`

const dirTriggersB = `
triggers:
  - name: digest
    cron: "30 9 * * *"
    target: { profile: worker }
    payload: { topic: news2 }
  - name: heartbeat
    every: 15m
    target: { profile: worker }
`

// TestLoadDirMerge: the directory layout merges into the same Config shape as
// the single-file path — agents, spawn profiles, channels, triggers — with
// later trigger files winning on duplicate names.
func TestLoadDirMerge(t *testing.T) {
	dir := writeDir(t, map[string]string{
		"system.yaml":             dirSystem,
		"channels.yaml":           dirChannels,
		"profiles/greeter.yaml":   dirProfileGreeter,
		"profiles/worker.yaml":    dirProfileWorker,
		"triggers/10-digest.yaml": dirTriggersA,
		"triggers/20-extra.yaml":  dirTriggersB,
	})
	cfg, err := LoadDir(dir, discardLog())
	if err != nil {
		t.Fatal(err)
	}

	greeter, ok := cfg.Agents["greeter"]
	if !ok {
		t.Fatalf("greeter missing from agents: %v", cfg.Agents)
	}
	if _, isSpawn := cfg.Profiles.Agent["greeter"]; isSpawn {
		t.Fatal("greeter (spawn unset) must land in agents, not profiles.agent")
	}
	if greeter.Model != "default" {
		t.Fatalf("model not merged: %+v", greeter)
	}

	worker, ok := cfg.Profiles.Agent["worker"]
	if !ok {
		t.Fatalf("worker (spawn: true) must land in profiles.agent: %v", cfg.Profiles.Agent)
	}
	if _, isAgent := cfg.Agents["worker"]; isAgent {
		t.Fatal("worker must not also land in agents")
	}
	if worker.Budget.TokensPerDay != 5000 || worker.Budget.Window != "168h" {
		t.Fatalf("spawn budget not converted: %+v", worker.Budget)
	}

	if len(cfg.Gateway.Channels) != 1 || cfg.Gateway.Channels[0].Name != "wh" {
		t.Fatalf("channels not merged: %+v", cfg.Gateway.Channels)
	}

	if len(cfg.Triggers) != 2 {
		t.Fatalf("expected 2 triggers (digest deduped), got %+v", cfg.Triggers)
	}
	var digest, heartbeat *Trigger
	for i := range cfg.Triggers {
		switch cfg.Triggers[i].Name {
		case "digest":
			digest = &cfg.Triggers[i]
		case "heartbeat":
			heartbeat = &cfg.Triggers[i]
		}
	}
	if digest == nil || heartbeat == nil {
		t.Fatalf("trigger merge wrong: %+v", cfg.Triggers)
	}
	// Later file (20-extra.yaml) wins on the duplicate "digest" name.
	if digest.Cron != "30 9 * * *" || digest.RunOnBoot {
		t.Fatalf("later trigger file must win: %+v", digest)
	}
	if digest.Target.Profile != "worker" || digest.Payload["topic"] != "news2" {
		t.Fatalf("trigger target/payload wrong: %+v", digest)
	}
	if heartbeat.Every != "15m" {
		t.Fatalf("heartbeat every wrong: %+v", heartbeat)
	}
}

// TestLoadDirRebasesRelativePaths: <dir> becomes the resolution base for every
// relative path; absolute paths and builtin: refs pass through untouched.
func TestLoadDirRebasesRelativePaths(t *testing.T) {
	absInstr := filepath.Join(os.TempDir(), "abs-prompts", "greeter.md")
	dir := writeDir(t, map[string]string{
		"system.yaml": dirSystem + "plugins: { dir: ./plugins }\n",
		"profiles/greeter.yaml": `
name: greeter
loop: builtin:per_chat
instructions: ` + absInstr + `
`,
		"profiles/builtinbot.yaml": `
name: builtinbot
loop: builtin:per_chat
`,
	})
	cfg, err := LoadDir(dir, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.Agents["greeter"]
	if g.Loop != "builtin:per_chat" {
		t.Fatalf("builtin: ref must not be rebased: %q", g.Loop)
	}
	if g.Instructions != absInstr {
		t.Fatalf("absolute path must not be rebased: %q", g.Instructions)
	}
	if cfg.Gateway.Route != filepath.Join(dir, "routes", "route.lua") {
		t.Fatalf("route not rebased: %q", cfg.Gateway.Route)
	}
	p := cfg.Memory.Backends["main_db"].Config["path"]
	if p != filepath.Join(dir, "data", "agentflow.db") {
		t.Fatalf("sqlite backend path not rebased: %v", p)
	}
	if kf := cfg.Profiles.Shell["box"].KeyFile; kf != filepath.Join(dir, "keys", "box.pem") {
		t.Fatalf("shell key_file not rebased: %q", kf)
	}
	if cfg.Plugins.Dir != filepath.Join(dir, "plugins") {
		t.Fatalf("plugins.dir not rebased: %q", cfg.Plugins.Dir)
	}
	if cfg.PersistencePath() != filepath.Join(dir, "data", "agentflow.db") {
		t.Fatalf("persistence not rebased: %q", cfg.PersistencePath())
	}
}

// TestLoadDirStrictFragment: an unknown field in any fragment is a boot error,
// exactly like the single-file path.
func TestLoadDirStrictFragment(t *testing.T) {
	cases := map[string]map[string]string{
		"system.yaml unknown field": {
			"system.yaml": dirSystem + "nonsense: 1\n",
		},
		"channels.yaml unknown field": {
			"system.yaml":   dirSystem,
			"channels.yaml": "channels:\n  - { name: wh, type: webhook, agent: greeter, path: /h/, lsten: x }\n",
		},
		"profile unknown field": {
			"system.yaml":       dirSystem,
			"profiles/bad.yaml": "name: bad\nloop: builtin:per_chat\nmodle: default\n",
		},
		"trigger unknown field": {
			"system.yaml":       dirSystem,
			"triggers/bad.yaml": "triggers:\n  - { name: t, cron: '* * * * *', targt: { profile: worker } }\n",
		},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writeDir(t, files)
			if _, err := LoadDir(dir, discardLog()); err == nil {
				t.Fatal("strict decode should fail on unknown field")
			} else if !strings.Contains(err.Error(), "field") {
				t.Fatalf("error should name the unknown field: %v", err)
			}
		})
	}
}

// TestLoadDirAgentsInSystemRejected: agents/channels in system.yaml are
// pointed at their proper homes instead of being silently merged.
func TestLoadDirAgentsInSystemRejected(t *testing.T) {
	dir := writeDir(t, map[string]string{
		"system.yaml": dirSystem + "agents:\n  bot: { loop: builtin:per_chat }\n",
	})
	if _, err := LoadDir(dir, discardLog()); err == nil ||
		!strings.Contains(err.Error(), "profiles/*.yaml") {
		t.Fatalf("agents in system.yaml must be rejected: %v", err)
	}
}

// TestLoadDirMissingSystem: system.yaml is required.
func TestLoadDirMissingSystem(t *testing.T) {
	dir := writeDir(t, map[string]string{"channels.yaml": dirChannels})
	if _, err := LoadDir(dir, discardLog()); err == nil {
		t.Fatal("missing system.yaml must fail")
	}
}

// TestLoadDirDuplicateProfileRejected: two files naming the same profile are
// a boot error, not a silent last-wins.
func TestLoadDirDuplicateProfileRejected(t *testing.T) {
	// A flat duplicate (two files naming the same profile) is an error;
	// nested subdirectories are not part of the glob at all.
	dir := writeDir(t, map[string]string{
		"system.yaml":       dirSystem,
		"profiles/one.yaml": "name: greeter\nloop: builtin:per_chat\n",
		"profiles/two.yaml": "name: greeter\nloop: builtin:per_chat\n",
	})
	if _, err := LoadDir(dir, discardLog()); err == nil ||
		!strings.Contains(err.Error(), "duplicate agent") {
		t.Fatalf("duplicate profile name must fail: %v", err)
	}
}

// TestLoadDirSpawnRejectsAgentFields: agent-only fields on a spawn profile are
// a boot error.
func TestLoadDirSpawnRejectsAgentFields(t *testing.T) {
	dir := writeDir(t, map[string]string{
		"system.yaml":     dirSystem,
		"profiles/w.yaml": "name: w\nspawn: true\nloop: builtin:per_chat\npersistent: true\n",
	})
	if _, err := LoadDir(dir, discardLog()); err == nil ||
		!strings.Contains(err.Error(), "persistent") {
		t.Fatalf("persistent on a spawn profile must fail: %v", err)
	}
}

// TestLoadDirValidatesAfterMerge: cross-file references are checked against
// the merged config — here, can_contact pointing at nothing configured.
func TestLoadDirValidatesAfterMerge(t *testing.T) {
	dir := writeDir(t, map[string]string{
		"system.yaml": dirSystem,
		"profiles/lonely.yaml": `
name: lonely
loop: builtin:per_chat
can_contact: [ghost]
`,
	})
	if _, err := LoadDir(dir, discardLog()); err == nil ||
		!strings.Contains(err.Error(), "ghost") {
		t.Fatalf("cross-file reference validation must run: %v", err)
	}
}
