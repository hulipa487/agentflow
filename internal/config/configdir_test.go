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

// TestLoadDirSpawnProfileExtras: a spawn profile's extras survive the
// agent-file -> SpawnProfile conversion (they used to be accepted by strict
// decode and then silently discarded).
func TestLoadDirSpawnProfileExtras(t *testing.T) {
	dir := writeDir(t, map[string]string{
		"system.yaml": dirSystem,
		"profiles/boss.yaml": `
name: boss
loop: ./loops/boss.lua
extras:
  workflow: boss-flow
`,
		"profiles/pm.yaml": `
name: pm
spawn: true
loop: ./loops/pm.lua
extras:
  workflow: release-train
  goal: { type: autonomous, success_signal: "PR merged", max_turns: 12 }
  api_key: ${PM_API_KEY}
`,
		"profiles/plain.yaml": `
name: plain
spawn: true
loop: ./loops/plain.lua
`,
	})
	cfg, err := LoadDir(dir, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	// Static-agent extras are untouched by the spawn conversion.
	if boss := cfg.Agents["boss"]; boss.Extras["workflow"] != "boss-flow" {
		t.Fatalf("static agent extras changed: %#v", boss.Extras)
	}
	pm, ok := cfg.Profiles.Agent["pm"]
	if !ok {
		t.Fatalf("pm profile missing: %v", cfg.Profiles.Agent)
	}
	if pm.Extras["workflow"] != "release-train" {
		t.Fatalf("spawn extras dropped: %#v", pm.Extras)
	}
	if pm.Extras["api_key"] != "${PM_API_KEY}" {
		t.Fatalf("spawn extras must stay raw: %#v", pm.Extras)
	}
	goal, ok := pm.Extras["goal"].(map[string]any)
	if !ok || goal["success_signal"] != "PR merged" {
		t.Fatalf("goal block wrong: %#v", pm.Extras["goal"])
	}
	// A spawn profile without extras stays nil — no behavior change.
	if cfg.Profiles.Agent["plain"].Extras != nil {
		t.Fatalf("plain profile must have no extras: %#v", cfg.Profiles.Agent["plain"].Extras)
	}
}

// TestLoadTriggersMatchesBoot: the engine's trigger scheduler re-reads the
// merged list through LoadTriggers, so that call must return exactly what the
// boot load produced — same merge order, same env expansion — and must follow
// an edit without a restart.
func TestLoadTriggersMatchesBoot(t *testing.T) {
	t.Setenv("TRIGGER_REGION", "us-east-1")
	dir := writeDir(t, map[string]string{
		"system.yaml":             dirSystem,
		"profiles/greeter.yaml":   dirProfileGreeter,
		"profiles/worker.yaml":    dirProfileWorker,
		"triggers/10-digest.yaml": dirTriggersA,
		"triggers/20-extra.yaml":  dirTriggersB,
	})

	cfg, err := LoadDir(dir, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadTriggers(dir, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(cfg.Triggers) {
		t.Fatalf("reload saw %d triggers; boot saw %d", len(got), len(cfg.Triggers))
	}
	for i := range got {
		if got[i].Name != cfg.Triggers[i].Name || got[i].Cron != cfg.Triggers[i].Cron ||
			got[i].Every != cfg.Triggers[i].Every || got[i].RunOnBoot != cfg.Triggers[i].RunOnBoot ||
			got[i].Target.Profile != cfg.Triggers[i].Target.Profile {
			t.Fatalf("trigger %d differs:\n reload %+v\n boot   %+v", i, got[i], cfg.Triggers[i])
		}
	}
	// Boot order is sorted-glob with the later file winning on a duplicate name.
	if got[0].Name != "digest" || got[0].Cron != "30 9 * * *" || got[1].Name != "heartbeat" {
		t.Fatalf("merge/override order wrong: %+v", got)
	}

	// A non-secret payload reference expands on the reload path too.
	if err := os.WriteFile(filepath.Join(dir, "triggers", "30-region.yaml"), []byte(`
triggers:
  - { name: regional, every: 1h, target: { profile: worker }, payload: { region: "${TRIGGER_REGION}" } }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = LoadTriggers(dir, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[2].Name != "regional" {
		t.Fatalf("new trigger file not picked up: %+v", got)
	}
	if got[2].Payload["region"] != "us-east-1" {
		t.Fatalf("payload not expanded on reload: %v", got[2].Payload)
	}

	// Removing every trigger file leaves an empty list (the running set is then
	// replaced with nothing) — not an error.
	if err := os.Remove(filepath.Join(dir, "triggers", "10-digest.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "triggers", "20-extra.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "triggers", "30-region.yaml")); err != nil {
		t.Fatal(err)
	}
	got, err = LoadTriggers(dir, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("empty triggers dir must yield an empty list: %+v", got)
	}

	// A malformed fragment is an error, so the scheduler keeps the running set.
	if err := os.WriteFile(filepath.Join(dir, "triggers", "40-bad.yaml"), []byte("triggers:\n  - { name: bad, every: 5m, nope: 1 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTriggers(dir, discardLog()); err == nil {
		t.Fatal("a malformed trigger fragment must be an error")
	}
}
