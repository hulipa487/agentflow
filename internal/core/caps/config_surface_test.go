package caps

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/credentials"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/session"
)

// journalGW captures egress (schemaGW behavior) plus a journal feed.
type journalGW struct {
	schemaGW
	journalMu sync.Mutex
	journal   []session.EgressRecord
}

func (g *journalGW) record(rec session.EgressRecord) {
	g.journalMu.Lock()
	defer g.journalMu.Unlock()
	g.journal = append(g.journal, rec)
}

func (g *journalGW) journalText() string {
	g.journalMu.Lock()
	defer g.journalMu.Unlock()
	var b strings.Builder
	for _, r := range g.journal {
		b.WriteString(r.Text)
	}
	return b.String()
}

// runConfigLoop drives one actor with the given loop source, the given agent
// Info (extras/credentials), and the runtime config handlers. It returns the
// first session.send text, the captured log output, and the journal.
func runConfigLoop(t *testing.T, loopSrc string, info *session.Info, store *credentials.Store, triggers []config.Trigger, logBuf *bytes.Buffer) (string, *journalGW) {
	t.Helper()

	var log *slog.Logger
	if logBuf != nil {
		log = slog.New(slog.NewTextHandler(logBuf, nil))
	} else {
		log = discardLogger()
	}

	gw := &journalGW{}
	handlers := map[string]session.OpHandler{}
	for k, h := range RuntimeHandlers(triggers) {
		handlers[k] = h
	}
	handlers["credential.get"] = CredentialHandler(store, info.Credentials, log)

	a := session.New("main|test",
		session.Identity{SessionID: "main|test", Agent: "main", Capabilities: map[string]bool{}},
		info, gw, nil, nil, nil, nil, handlers, pool.New(1), log)
	a.LoopSrc = loopSrc
	a.Journal = func(rec session.EgressRecord) { gw.record(rec) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	a.Mailbox <- session.Message{ID: "m1", Type: "user", From: "u", Text: "go", Channel: "webhook", ReplyTo: "1"}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		gw.mu.Lock()
		n := len(gw.sends)
		gw.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if len(gw.sends) == 0 {
		t.Fatal("no reply from loop")
	}
	return gw.sends[0], gw
}

// TestAgentConfigShapeAndSecretOpacity: agent.config() surfaces the profile
// (name, model, instructions path, goal, extras) with secret references
// rendered as opaque markers — the resolved value appears nowhere.
func TestAgentConfigShapeAndSecretOpacity(t *testing.T) {
	const secretValue = "sk-SUPER-SECRET-VALUE"
	t.Setenv("PM_API_KEY", secretValue) // set: proves opacity is not "just unresolved"

	info := &session.Info{
		Name:             "pm",
		Model:            "default",
		InstructionsPath: "/deploy/prompts/pm.md",
		Extras: map[string]any{
			"goal": map[string]any{
				"type":           "autonomous",
				"success_signal": "PR merged",
				"max_turns":      40,
				"on_goal_met":    "report",
			},
			"workflow": "release-train",
			"api_key":  "${PM_API_KEY}",
			"slack":    "cred:slack_bot",
			"options":  map[string]any{"retries": "3", "key": "${PM_API_KEY}"},
		},
	}
	got, _ := runConfigLoop(t, `
function loop()
  local msg = session.inbox()
  local cfg = agent.config()
  if cfg.name ~= "pm" or cfg.model ~= "default" then
    session.send("err: name/model wrong")
    return
  end
  if cfg.instructions_path ~= "/deploy/prompts/pm.md" then
    session.send("err: instructions path wrong: " .. tostring(cfg.instructions_path))
    return
  end
  if not (cfg.goal and cfg.goal.type == "autonomous" and cfg.goal.success_signal == "PR merged"
          and cfg.goal.max_turns == 40 and cfg.goal.on_goal_met == "report") then
    session.send("err: goal wrong: " .. json.encode(cfg.goal))
    return
  end
  if cfg.extras.workflow ~= "release-train" then
    session.send("err: extras wrong")
    return
  end
  if not (type(cfg.extras.api_key) == "table" and cfg.extras.api_key.env == "PM_API_KEY") then
    session.send("err: api_key not an opaque env marker: " .. json.encode(cfg.extras.api_key))
    return
  end
  if not (type(cfg.extras.slack) == "table" and cfg.extras.slack.cred == "slack_bot") then
    session.send("err: slack not an opaque cred marker")
    return
  end
  if not (type(cfg.extras.options.key) == "table" and cfg.extras.options.key.env == "PM_API_KEY") then
    session.send("err: nested secret not opaque")
    return
  end
  if cfg.extras.options.retries ~= "3" then
    session.send("err: non-secret extras mangled")
    return
  end
  session.send("CONFIG:" .. json.encode(cfg))
end
`, info, nil, nil, nil)

	if !strings.HasPrefix(got, "CONFIG:") {
		t.Fatalf("loop failed: %s", got)
	}
	if strings.Contains(got, secretValue) {
		t.Fatal("resolved secret leaked into agent.config() output")
	}
}

// TestRuntimeTriggersMerge: runtime.triggers() returns the merged list with
// kind, schedule fields, target, and payload.
func TestRuntimeTriggersMerge(t *testing.T) {
	triggers := []config.Trigger{
		{Name: "digest", Cron: "30 9 * * *", RunOnBoot: true,
			Target: config.TriggerTarget{Profile: "worker"}, Payload: map[string]any{"topic": "news"}},
		{Name: "heartbeat", Every: "15m", Target: config.TriggerTarget{Profile: "worker"}},
		{Name: "ticket", Event: &config.TriggerEvent{Channel: "webhook", Match: "urgent"},
			Target: config.TriggerTarget{Profile: "worker"}},
	}
	got, _ := runConfigLoop(t, `
function loop()
  local msg = session.inbox()
  local r = runtime.triggers()
  if not (r and r.ok and #r.triggers == 3) then
    session.send("err: trigger count wrong: " .. json.encode(r))
    return
  end
  local byName = {}
  for _, tr in ipairs(r.triggers) do byName[tr.name] = tr end
  if not (byName.digest and byName.digest.kind == "cron" and byName.digest.cron == "30 9 * * *"
          and byName.digest.run_on_boot == true and byName.digest.target.profile == "worker"
          and byName.digest.payload.topic == "news") then
    session.send("err: digest wrong: " .. json.encode(byName.digest))
    return
  end
  if not (byName.heartbeat and byName.heartbeat.kind == "every" and byName.heartbeat.every == "15m") then
    session.send("err: heartbeat wrong")
    return
  end
  if not (byName.ticket and byName.ticket.kind == "event" and byName.ticket.event.channel == "webhook"
          and byName.ticket.event.match == "urgent") then
    session.send("err: ticket wrong")
    return
  end
  session.send("done")
end
`, &session.Info{Name: "bot"}, nil, triggers, nil)
	if got != "done" {
		t.Fatalf("loop failed: %s", got)
	}
}

// TestCredentialGetGrantDeny: a granted agent gets the value; an agent with
// no allow-list (and one with an unlisted name) gets a clear error; access
// log lines name agent+credential+outcome; and the journal record the loop's
// egress produced carries no secret material.
func TestCredentialGetGrantDeny(t *testing.T) {
	ctx := context.Background()
	store, err := credentials.Open(t.TempDir()+"/cred.db", "master", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Put(ctx, "", "deploy_token", "api", "sk-JOURNAL-SAFE-VALUE", "", ""); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer

	// Granted: the value comes back.
	got, gw := runConfigLoop(t, `
function loop()
  local msg = session.inbox()
  local r = credential.get("deploy_token")
  if not (r and r.ok and r.value == "sk-JOURNAL-SAFE-VALUE") then
    session.send("err: grant wrong: " .. json.encode(r))
    return
  end
  -- Egress that must stay free of the secret (the runtime never journals the
  -- op response; the loop also must not echo it for this contract to hold).
  session.send("fetched")
end
`, &session.Info{Name: "deployer", Credentials: []string{"deploy_token"}}, store, nil, &logBuf)
	if got != "fetched" {
		t.Fatalf("grant loop failed: %s", got)
	}
	if strings.Contains(gw.journalText(), "sk-JOURNAL-SAFE-VALUE") {
		t.Fatal("secret material persisted to the message journal")
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "credential access granted") || !strings.Contains(logs, "deploy_token") {
		t.Fatalf("grant not logged: %s", logs)
	}

	// Unlisted name on an agent with a list.
	logBuf.Reset()
	got, _ = runConfigLoop(t, `
function loop()
  local msg = session.inbox()
  local ok, res = pcall(credential.get, "other_secret")
  if ok or not string.find(tostring(res), "allow-list", 1, true) then
    session.send("err: unlisted name did not deny: " .. tostring(res))
    return
  end
  session.send("denied")
end
`, &session.Info{Name: "deployer", Credentials: []string{"deploy_token"}}, store, nil, &logBuf)
	if got != "denied" {
		t.Fatalf("unlisted loop failed: %s", got)
	}
	if !strings.Contains(logBuf.String(), "credential access denied") {
		t.Fatalf("denial not logged: %s", logBuf.String())
	}

	// Missing allow-list entirely.
	logBuf.Reset()
	got, _ = runConfigLoop(t, `
function loop()
  local msg = session.inbox()
  local ok, res = pcall(credential.get, "deploy_token")
  if ok or not string.find(tostring(res), "allow-list", 1, true) then
    session.send("err: missing list did not deny: " .. tostring(res))
    return
  end
  session.send("denied")
end
`, &session.Info{Name: "nolist"}, store, nil, &logBuf)
	if got != "denied" {
		t.Fatalf("missing-list loop failed: %s", got)
	}

	// No store at all: clear "not enabled" error, not a crash.
	logBuf.Reset()
	got, _ = runConfigLoop(t, `
function loop()
  local msg = session.inbox()
  local ok, res = pcall(credential.get, "deploy_token")
  if ok or not string.find(tostring(res), "not enabled", 1, true) then
    session.send("err: nil store did not error clearly: " .. tostring(res))
    return
  end
  session.send("disabled")
end
`, &session.Info{Name: "anyone", Credentials: []string{"deploy_token"}}, nil, nil, &logBuf)
	if got != "disabled" {
		t.Fatalf("nil-store loop failed: %s", got)
	}
}

// TestSpawnedChildConfigExtras: the Info shape main.go now puts on a spawn
// template (spawn:<profile> name, instructions path, extras, credential
// allow-list) is what a spawned child's agent.config() renders — extras
// visible, secret references opaque, and the resolved value absent.
func TestSpawnedChildConfigExtras(t *testing.T) {
	const secretValue = "sk-CHILD-MUST-NOT-SEE"
	t.Setenv("PM_API_KEY", secretValue) // set: proves opacity is not "unresolved"

	info := &session.Info{
		Name:             "spawn:pm",
		Model:            "default",
		InstructionsPath: "/deploy/prompts/pm.md",
		Credentials:      []string{"deploy_token"},
		Extras: map[string]any{
			"workflow": "release-train",
			"goal":     map[string]any{"type": "autonomous", "max_turns": 12},
			"api_key":  "${PM_API_KEY}",
		},
	}
	got, _ := runConfigLoop(t, `
function loop()
  local msg = session.inbox()
  local cfg = agent.config()
  if cfg.name ~= "spawn:pm" then
    session.send("err: name " .. tostring(cfg.name))
    return
  end
  if cfg.extras.workflow ~= "release-train" then
    session.send("err: child extras missing: " .. json.encode(cfg.extras))
    return
  end
  if not (cfg.goal and cfg.goal.type == "autonomous" and cfg.goal.max_turns == 12) then
    session.send("err: child goal missing")
    return
  end
  if not (type(cfg.extras.api_key) == "table" and cfg.extras.api_key.env == "PM_API_KEY") then
    session.send("err: child secret not opaque: " .. json.encode(cfg.extras.api_key))
    return
  end
  session.send("CHILD:" .. json.encode(cfg))
end
`, info, nil, nil, nil)
	if !strings.HasPrefix(got, "CHILD:") {
		t.Fatalf("loop failed: %s", got)
	}
	if strings.Contains(got, secretValue) {
		t.Fatal("resolved secret reached a spawned child's agent.config()")
	}
}
