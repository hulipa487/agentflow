package caps

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/session"
	"agentflow/internal/core/tools"
)

// syncBuf is a goroutine-safe log sink (the actor logs on its own goroutine
// while the test reads).
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitFor polls cond until it holds or d elapses.
func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", d, what)
}

// toolDefFixture wires one actor the way main.go does: a Go tool registry with
// tools.policy.overrides applied (registered names baked in, everything else
// deferred to LuaOverrides) plus the shared prompt registry.
type toolDefFixture struct {
	reg      *tools.Registry
	agentSet *tools.AgentSet
	lua      *tools.LuaOverrides
	prompts  *session.PromptRegistry
	logs     *syncBuf
	log      *slog.Logger
}

// newToolDefFixture builds the fixture. overrides may be nil; promptTexts seeds
// the prompt registry.
func newToolDefFixture(t *testing.T, overrides map[string]config.ToolSpecOverride, promptTexts map[string]string) *toolDefFixture {
	t.Helper()
	reg := tools.NewRegistry()
	reg.Register(tools.ToolSpec{
		Name:        "builtin:echo",
		Description: "Echo the text back",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string", "description": "Text to echo"}},
		},
		Autonomous: true,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			return map[string]any{"ok": true, "echoed": args["text"]}, nil
		},
	})
	if err := reg.ApplyOverrides(overrides, promptTexts, nil); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	return &toolDefFixture{
		reg:      reg,
		agentSet: reg.Expose(nil, config.ToolsPolicy{}, false),
		lua:      tools.NewLuaOverrides(overrides, reg.Names()),
		prompts:  session.NewPromptRegistry(promptTexts),
		logs:     logs,
		log:      slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

// start launches one actor with the fixture's wiring and returns its gateway
// and mailbox, so a test can pump several turns and watch tools.list() change.
func (fx *toolDefFixture) start(t *testing.T, loopSrc string) (*schemaGW, *session.Actor) {
	t.Helper()
	return fx.startAs(t, fx.agentSet, loopSrc)
}

// agentFor returns the tool surface for one agent on this fixture's shared
// registry: in main.go the registry and the LuaOverrides are deployment-wide,
// while the AgentSet is per agent.
func (fx *toolDefFixture) agentFor(skills []string, policy config.ToolsPolicy) *tools.AgentSet {
	return fx.reg.Expose(skills, policy, false)
}

// startAs launches an actor with an explicitly chosen tool surface.
func (fx *toolDefFixture) startAs(t *testing.T, agentSet *tools.AgentSet, loopSrc string) (*schemaGW, *session.Actor) {
	t.Helper()
	gw := &schemaGW{}
	a := session.New("main|test",
		session.Identity{SessionID: "main|test", Agent: "main", Capabilities: map[string]bool{"tools": true}},
		&session.Info{Name: "main", HistoryBudget: 100},
		gw, nil, nil, nil, nil,
		ToolHandlers(agentSet, ToolWiring{LuaOverrides: fx.lua, Prompts: fx.prompts, Log: fx.log}),
		pool.New(1), fx.log)
	a.LoopSrc = directive() + loopSrc

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Run(ctx)
	return gw, a
}

// sends returns a copy of the gateway's replies so far.
func (g *schemaGW) replies() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.sends...)
}

// deliver posts one message and waits for the reply with that index.
func deliver(t *testing.T, a *session.Actor, gw *schemaGW, idx int, text string) string {
	t.Helper()
	a.Mailbox <- session.Message{ID: text, Type: "user", From: "u", Text: text, Channel: "webhook", ReplyTo: "1"}
	waitFor(t, "reply "+text, 5*time.Second, func() bool { return len(gw.replies()) > idx })
	return gw.replies()[idx]
}

// runLoop drives one actor with the fixture's wiring and returns the first
// session.send text.
func (fx *toolDefFixture) runLoop(t *testing.T, loopSrc string) string {
	t.Helper()
	return fx.runLoopAs(t, fx.agentSet, loopSrc)
}

// runLoopAs drives one actor with an explicitly chosen tool surface.
func (fx *toolDefFixture) runLoopAs(t *testing.T, agentSet *tools.AgentSet, loopSrc string) string {
	t.Helper()
	gw, a := fx.startAs(t, agentSet, loopSrc)
	return deliver(t, a, gw, 0, "m1")
}

// runToolDefLoop drives one loop with no overrides and no prompts — the
// pre-override behavior, unchanged.
func runToolDefLoop(t *testing.T, loopSrc string) string {
	t.Helper()
	return newToolDefFixture(t, nil, nil).runLoop(t, loopSrc)
}

// describeToolLua is a Lua preamble shared by the override tests: a local
// helper that looks a tool up in tools.list() and returns its description and
// one param description as "desc|paramdesc".
const describeToolLua = `
function describe(name, param) {
  for (const d of tools.list()) {
    const f = d["function"];
    if (f && f.name === name) {
      let pd = "";
      if (param && f.parameters && f.parameters.properties && f.parameters.properties[param]) {
        pd = f.parameters.properties[param].description || "";
      }
      return (f.description || "") + "|" + pd;
    }
  }
  return "absent|";
}
`

func TestToolDefDeclaredToolListedAndRuns(t *testing.T) {
	got := runToolDefLoop(t, `
tool.def({
  name: "lua:shout",
  description: "Shout the text back",
  params: { type: "object", properties: { text: { type: "string" } }, required: ["text"] },
  handler: function(args) {
    return { ok: true, shouted: String(args.text || "").toUpperCase() };
  },
});

function loop() {
  session.inbox();
  let listed = false;
  let go_listed = false;
  for (const d of tools.list()) {
    const f = d["function"];
    if (f && f.name === "lua:shout" && f.description === "Shout the text back" &&
        f.parameters && f.parameters.required) {
      listed = true;
    }
    if (f && f.name === "builtin:echo") {
      go_listed = true;
    }
  }
  if (!listed) {
    session.send("err: lua tool not listed");
    return;
  }
  if (!go_listed) {
    session.send("err: go tool dropped from list");
    return;
  }
  const res = tools.run("lua:shout", { text: "hi" });
  if (!(res && res.ok && res.shouted === "HI")) {
    session.send("err: lua handler result wrong");
    return;
  }
  const gres = tools.run("builtin:echo", { text: "ping" });
  if (!(gres && gres.ok && gres.echoed === "ping")) {
    session.send("err: go tool fallthrough broken");
    return;
  }
  session.send("done");
}
`)
	if got != "done" {
		t.Fatalf("loop failed: %s", got)
	}
}

func TestToolDefShadowsGoToolAndErrorPath(t *testing.T) {
	got := runToolDefLoop(t, `
tool.def({
  name: "builtin:echo",
  description: "Lua echo",
  handler: function(args) {
    return { ok: true, echoed: "LUA:" + (args.text || "") };
  },
});
tool.def({
  name: "lua:boom",
  handler: function(_args) {
    throw new Error("kaboom");
  },
});

function loop() {
  session.inbox();
  let n = 0;
  let desc = null;
  for (const d of tools.list()) {
    const f = d["function"];
    if (f && f.name === "builtin:echo") {
      n++;
      desc = f.description;
    }
  }
  if (n !== 1 || desc !== "Lua echo") {
    session.send("err: shadowed tool listed " + n + " times");
    return;
  }
  const res = tools.run("builtin:echo", { text: "x" });
  if (!(res && res.ok && res.echoed === "LUA:x")) {
    session.send("err: shadow did not take over run");
    return;
  }
  const bad = tools.run("lua:boom", {});
  if (!(bad && bad.ok === false && bad.error && String(bad.error).includes("kaboom"))) {
    session.send("err: handler error not returned as result");
    return;
  }
  session.send("done");
}
`)
	if got != "done" {
		t.Fatalf("loop failed: %s", got)
	}
}
