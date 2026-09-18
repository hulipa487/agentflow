package triggers

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/gateway"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/session"
	"agentflow/internal/core/supervisor"
)

// TestConfigDirEveryTriggerReachesItsAgent is the end-to-end check for engine
// scheduling: a -configdir deployment whose triggers/tick.yaml declares
// every: "2s" fires with no agent (and no capability) involved, and the
// target agent's loop receives an ordinary Message{type:"cron"} carrying the
// trigger's payload. This is the shape a deployment that deletes its
// user-space scheduler agent depends on.
func TestConfigDirEveryTriggerReachesItsAgent(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"system.yaml": "version: \"1\"\ngateway:\n  listen: \":0\"\n",
		"profiles/worker.yaml": `
name: worker
loop: worker.lua
`,
		"triggers/tick.yaml": `
triggers:
  - name: tick
    every: 2s
    target: { profile: worker }
    payload: { topic: news, n: 1 }
`,
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg, err := config.LoadDir(dir, log)
	if err != nil {
		t.Fatalf("configdir load: %v", err)
	}
	if len(cfg.Triggers) != 1 || cfg.Triggers[0].Every != "2s" {
		t.Fatalf("configdir triggers = %+v", cfg.Triggers)
	}

	// The target agent, wired exactly as main wires it: a loop that reports
	// what the engine handed it.
	defs := map[string]*supervisor.AgentDef{
		"worker": {
			Info:         &session.Info{Name: "worker", HistoryBudget: 100},
			Capabilities: map[string]bool{},
			Handlers:     map[string]session.OpHandler{},
			LoopSrc: `function loop()
  while true do
    local msg = session.inbox()
    log.info("GOT type=" .. tostring(msg.type)
      .. " id=" .. tostring(msg.id)
      .. " from=" .. tostring(msg.from)
      .. " topic=" .. tostring(msg.payload.topic)
      .. " n=" .. tostring(msg.payload.n))
  end
end`,
		},
	}
	sup := supervisor.New(defs, gateway.NewRegistry(log), pool.New(1), nil, log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup.Start(ctx)

	svc := New(
		func(profile string) (string, bool) {
			if _, ok := cfg.Agents[profile]; ok {
				return profile, true
			}
			return "", false
		},
		func(agent, key string, msg session.Message) error { return sup.Deliver(agent, key, msg) },
		cfg.Runtime.TimezoneOffset(),
		log,
	)
	svc.Start(ctx)
	svc.Reload(cfg.Triggers)
	defer svc.Stop()

	// every: 2s means the first fire is ~2s in — no run_on_boot, no boot push.
	waitFor(t, "the cron message in the agent loop", 10*time.Second, func() bool {
		return strings.Contains(logs.String(), "GOT type=cron")
	})
	out := logs.String()
	if !strings.Contains(out, "topic=news") || !strings.Contains(out, "n=1") {
		t.Fatalf("the trigger payload did not reach the loop:\n%s", out)
	}
	if !strings.Contains(out, "id=cron:tick:") {
		t.Fatalf("the message id does not name the trigger:\n%s", out)
	}
	if !strings.Contains(out, "from=system:scheduler") {
		t.Fatalf("the message provenance is not the scheduler:\n%s", out)
	}
	if !strings.Contains(out, "trigger fired") || !strings.Contains(out, "agent=worker") {
		t.Fatalf("the fire was not logged:\n%s", out)
	}
	// The engine, not a loop: no Lua code ever asked for a timer.
	if strings.Contains(out, "unknown op") {
		t.Fatalf("the loop hit an unknown op:\n%s", out)
	}
}

// TestConfigDirCronRunOnBootReachesItsAgent covers the cron path through the
// same wiring: a cron trigger with run_on_boot fires at startup (waiting for
// the next minute boundary would make this a minute-long test), and a spawn
// profile target resolves to its "spawn:<name>" session.
func TestConfigDirCronRunOnBootReachesItsAgent(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"system.yaml": "version: \"1\"\ngateway:\n  listen: \":0\"\nruntime:\n  timezone_offset_hours: 8\n",
		"profiles/bot.yaml": `
name: bot
loop: plugin:per_chat
`,
		"profiles/pm.yaml": `
name: pm
spawn: true
loop: pm.lua
`,
		"triggers/digest.yaml": `
triggers:
  - name: digest
    cron: "0 9 * * 1-5"
    run_on_boot: true
    target: { profile: pm }
    payload: { kind: digest }
`,
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg, err := config.LoadDir(dir, log)
	if err != nil {
		t.Fatalf("configdir load: %v", err)
	}
	if got := cfg.Runtime.TimezoneOffset().Hours(); got != 8 {
		t.Fatalf("timezone offset = %v; want 8", got)
	}

	// The spawn-profile child def main registers under "spawn:pm".
	defs := map[string]*supervisor.AgentDef{
		"spawn:pm": {
			Info:         &session.Info{Name: "spawn:pm", HistoryBudget: 100},
			Capabilities: map[string]bool{},
			Handlers:     map[string]session.OpHandler{},
			LoopSrc: `function loop()
  while true do
    local msg = session.inbox()
    log.info("CHILD type=" .. tostring(msg.type) .. " id=" .. tostring(msg.id)
      .. " kind=" .. tostring(msg.payload.kind))
  end
end`,
		},
	}
	sup := supervisor.New(defs, gateway.NewRegistry(log), pool.New(1), nil, log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup.Start(ctx)

	svc := New(
		func(profile string) (string, bool) {
			if _, ok := cfg.Profiles.Agent[profile]; ok {
				return "spawn:" + profile, true
			}
			return "", false
		},
		func(agent, key string, msg session.Message) error { return sup.Deliver(agent, key, msg) },
		cfg.Runtime.TimezoneOffset(),
		log,
	)
	svc.Start(ctx)
	svc.Reload(cfg.Triggers)
	defer svc.Stop()

	waitFor(t, "the cron boot fire in the spawn-profile session", 5*time.Second, func() bool {
		return strings.Contains(logs.String(), "CHILD type=cron")
	})
	out := logs.String()
	if !strings.Contains(out, "kind=digest") || !strings.Contains(out, "id=cron:digest:") {
		t.Fatalf("the cron payload did not reach the child:\n%s", out)
	}
}
