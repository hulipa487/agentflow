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
	a.LoopSrc = directive() + loopSrc
	a.Journal = func(rec session.EgressRecord) { gw.record(rec) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(done)
	}()
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
	// Stop the actor before the caller reads its log buffer: the loop logs
	// while it runs, and a bytes.Buffer is not safe for concurrent reads.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("actor did not stop")
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
function loop() {
  session.inbox();
  const cfg = agent.config();
  if (cfg.name !== "pm" || cfg.model !== "default") {
    session.send("err: name/model wrong");
    return;
  }
  if (cfg.instructions_path !== "/deploy/prompts/pm.md") {
    session.send("err: instructions path wrong: " + String(cfg.instructions_path));
    return;
  }
  if (!(cfg.goal && cfg.goal.type === "autonomous" && cfg.goal.success_signal === "PR merged"
        && cfg.goal.max_turns === 40 && cfg.goal.on_goal_met === "report")) {
    session.send("err: goal wrong: " + json.encode(cfg.goal));
    return;
  }
  if (cfg.extras.workflow !== "release-train") {
    session.send("err: extras wrong");
    return;
  }
  if (!(typeof cfg.extras.api_key === "object" && cfg.extras.api_key && cfg.extras.api_key.env === "PM_API_KEY")) {
    session.send("err: api_key not an opaque env marker: " + json.encode(cfg.extras.api_key));
    return;
  }
  if (!(typeof cfg.extras.slack === "object" && cfg.extras.slack && cfg.extras.slack.cred === "slack_bot")) {
    session.send("err: slack not an opaque cred marker");
    return;
  }
  if (!(typeof cfg.extras.options.key === "object" && cfg.extras.options.key && cfg.extras.options.key.env === "PM_API_KEY")) {
    session.send("err: nested secret not opaque");
    return;
  }
  if (cfg.extras.options.retries !== "3") {
    session.send("err: non-secret extras mangled");
    return;
  }
  session.send("CONFIG:" + json.encode(cfg));
}
`, info, nil, nil, nil)

	if !strings.HasPrefix(got, "CONFIG:") {
		t.Fatalf("loop failed: %s", got)
	}
	if strings.Contains(got, secretValue) {
		t.Fatal("resolved secret leaked into agent.config() output")
	}
}

// TestAgentConfigPrompts: agent.config() surfaces the deployment's resolved
// prompt registry under .prompts — the text, not the {file: ...} reference —
// and an empty table when the config declares no prompts, so a loop can index
// cfg.prompts without a nil check.
func TestAgentConfigPrompts(t *testing.T) {
	info := &session.Info{
		Name:    "bot",
		Prompts: session.NewPromptRegistry(map[string]string{"kb_header": "[Knowledge base]", "distiller": "You are a distiller."}),
	}
	got, _ := runConfigLoop(t, `
function loop() {
  session.inbox();
  const cfg = agent.config();
  if (cfg.prompts.kb_header !== "[Knowledge base]") {
    session.send("err: kb_header: " + String(cfg.prompts.kb_header));
    return;
  }
  if (cfg.prompts.distiller !== "You are a distiller.") {
    session.send("err: distiller: " + String(cfg.prompts.distiller));
    return;
  }
  session.send("ok");
}
`, info, nil, nil, nil)
	if got != "ok" {
		t.Fatalf("loop failed: %s", got)
	}

	got, _ = runConfigLoop(t, `
function loop() {
  session.inbox();
  const cfg = agent.config();
  if (typeof cfg.prompts !== "object" || cfg.prompts === null) {
    session.send("err: prompts is not an object: " + typeof cfg.prompts);
    return;
  }
  if (Object.keys(cfg.prompts).length !== 0) {
    session.send("err: prompts should be empty: " + json.encode(cfg.prompts));
    return;
  }
  session.send("empty");
}
`, &session.Info{Name: "bot"}, nil, nil, nil)
	if got != "empty" {
		t.Fatalf("empty-registry loop failed: %s", got)
	}
}

// TestAgentConfigInstructionsPrompt: when the instructions came from a
// prompts: registry key, agent.config() reports the key and leaves
// instructions_path empty, so a loop can tell a registry-sourced system
// prompt from a file-sourced one and fetch the text from cfg.prompts.
func TestAgentConfigInstructionsPrompt(t *testing.T) {
	info := &session.Info{
		Name:               "bot",
		InstructionsPrompt: "assistant_system",
		Prompts:            session.NewPromptRegistry(map[string]string{"assistant_system": "You are the assistant."}),
	}
	got, _ := runConfigLoop(t, `
function loop() {
  session.inbox();
  const cfg = agent.config();
  if (cfg.instructions_prompt !== "assistant_system") {
    session.send("err: prompt key: " + String(cfg.instructions_prompt));
    return;
  }
  if (cfg.instructions_path !== "") {
    session.send("err: path should be empty for a prompt source: " + String(cfg.instructions_path));
    return;
  }
  if (cfg.prompts[cfg.instructions_prompt] !== "You are the assistant.") {
    session.send("err: registry lookup failed");
    return;
  }
  session.send("ok");
}
`, info, nil, nil, nil)
	if got != "ok" {
		t.Fatalf("loop failed: %s", got)
	}
}

// TestAgentConfigPromptsAreLive: agent.config() reads the shared registry at
// call time rather than a boot snapshot, so a text republished by the reload
// watcher reaches the loop without a session restart.
func TestAgentConfigPromptsAreLive(t *testing.T) {
	reg := session.NewPromptRegistry(map[string]string{"kb_header": "old text"})
	reg.Set("kb_header", "reloaded text")

	got, _ := runConfigLoop(t, `
function loop() {
  session.inbox();
  const cfg = agent.config();
  if (cfg.prompts.kb_header !== "reloaded text") {
    session.send("err: stale prompt: " + String(cfg.prompts.kb_header));
    return;
  }
  session.send("live");
}
`, &session.Info{Name: "bot", Prompts: reg}, nil, nil, nil)
	if got != "live" {
		t.Fatalf("loop failed: %s", got)
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
function loop() {
  session.inbox();
  const r = runtime.triggers();
  if (!(r && r.ok && r.triggers.length === 3)) {
    session.send("err: trigger count wrong: " + json.encode(r));
    return;
  }
  const byName = {};
  for (const tr of r.triggers) byName[tr.name] = tr;
  if (!(byName.digest && byName.digest.kind === "cron" && byName.digest.cron === "30 9 * * *"
        && byName.digest.run_on_boot === true && byName.digest.target.profile === "worker"
        && byName.digest.payload.topic === "news")) {
    session.send("err: digest wrong: " + json.encode(byName.digest));
    return;
  }
  if (!(byName.heartbeat && byName.heartbeat.kind === "every" && byName.heartbeat.every === "15m")) {
    session.send("err: heartbeat wrong");
    return;
  }
  if (!(byName.ticket && byName.ticket.kind === "event" && byName.ticket.event.channel === "webhook"
        && byName.ticket.event.match === "urgent")) {
    session.send("err: ticket wrong");
    return;
  }
  session.send("done");
}
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
function loop() {
  session.inbox();
  const r = credential.get("deploy_token");
  if (!(r && r.ok && r.value === "sk-JOURNAL-SAFE-VALUE")) {
    session.send("err: grant wrong: " + json.encode(r));
    return;
  }
  session.send("fetched");
}
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
function loop() {
  session.inbox();
  try {
    const res = credential.get("other_secret");
    session.send("err: unlisted name did not deny: " + String(res));
    return;
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e);
    if (!msg.includes("allow-list")) {
      session.send("err: unlisted name wrong err: " + msg);
      return;
    }
  }
  session.send("denied");
}
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
function loop() {
  session.inbox();
  try {
    const res = credential.get("deploy_token");
    session.send("err: missing list did not deny: " + String(res));
    return;
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e);
    if (!msg.includes("allow-list")) {
      session.send("err: missing list wrong err: " + msg);
      return;
    }
  }
  session.send("denied");
}
`, &session.Info{Name: "nolist"}, store, nil, &logBuf)
	if got != "denied" {
		t.Fatalf("missing-list loop failed: %s", got)
	}

	// No store at all: clear "not enabled" error, not a crash.
	logBuf.Reset()
	got, _ = runConfigLoop(t, `
function loop() {
  session.inbox();
  try {
    const res = credential.get("deploy_token");
    session.send("err: nil store did not error clearly: " + String(res));
    return;
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e);
    if (!msg.includes("not enabled")) {
      session.send("err: nil store wrong err: " + msg);
      return;
    }
  }
  session.send("disabled");
}
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
function loop() {
  session.inbox();
  const cfg = agent.config();
  if (cfg.name !== "spawn:pm") {
    session.send("err: name " + String(cfg.name));
    return;
  }
  if (cfg.extras.workflow !== "release-train") {
    session.send("err: child extras missing: " + json.encode(cfg.extras));
    return;
  }
  if (!(cfg.goal && cfg.goal.type === "autonomous" && cfg.goal.max_turns === 12)) {
    session.send("err: child goal missing");
    return;
  }
  if (!(typeof cfg.extras.api_key === "object" && cfg.extras.api_key && cfg.extras.api_key.env === "PM_API_KEY")) {
    session.send("err: child secret not opaque: " + json.encode(cfg.extras.api_key));
    return;
  }
  session.send("CHILD:" + json.encode(cfg));
}
`, info, nil, nil, nil)
	if !strings.HasPrefix(got, "CHILD:") {
		t.Fatalf("loop failed: %s", got)
	}
	if strings.Contains(got, secretValue) {
		t.Fatal("resolved secret reached a spawned child's agent.config()")
	}
}
