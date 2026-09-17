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
	a.LoopSrc = loopSrc

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
local function describe(name, param)
  for _, d in ipairs(tools.list()) do
    local f = d["function"]
    if f and f.name == name then
      local pd = ""
      if param and f.parameters and f.parameters.properties and f.parameters.properties[param] then
        pd = f.parameters.properties[param].description or ""
      end
      return (f.description or "") .. "|" .. pd
    end
  end
  return "absent|"
end
`

// TestToolDefDeclaredToolListedAndRuns: a Luau chunk calling tool.def sees its
// tool in tools.list (OpenAI function shape, next to the Go tools) and
// tools.run dispatches to the Lua handler; unregistered names still fall
// through to the Go registry.
func TestToolDefDeclaredToolListedAndRuns(t *testing.T) {
	got := runToolDefLoop(t, `
tool.def({
  name = "lua:shout",
  description = "Shout the text back",
  params = { type = "object", properties = { text = { type = "string" } }, required = { "text" } },
  handler = function(args)
    return { ok = true, shouted = string.upper(args.text or "") }
  end,
})

function loop()
  local msg = session.inbox()
  local listed = false
  local go_listed = false
  for _, d in ipairs(tools.list()) do
    local f = d["function"]
    if f and f.name == "lua:shout" and f.description == "Shout the text back"
       and f.parameters and f.parameters.required then
      listed = true
    end
    if f and f.name == "builtin:echo" then
      go_listed = true
    end
  end
  if not listed then
    session.send("err: lua tool not listed")
    return
  end
  if not go_listed then
    session.send("err: go tool dropped from list")
    return
  end
  local res = tools.run("lua:shout", { text = "hi" })
  if not (res and res.ok and res.shouted == "HI") then
    session.send("err: lua handler result wrong")
    return
  end
  local gres = tools.run("builtin:echo", { text = "ping" })
  if not (gres and gres.ok and gres.echoed == "ping") then
    session.send("err: go tool fallthrough broken")
    return
  end
  session.send("done")
end
`)
	if got != "done" {
		t.Fatalf("loop failed: %s", got)
	}
}

// TestToolDefShadowsGoToolAndErrorPath: a declared name shadows a Go tool of
// the same name (one list entry, Lua handler wins), and a handler error comes
// back as an { ok = false, error = ... } result rather than killing the loop.
func TestToolDefShadowsGoToolAndErrorPath(t *testing.T) {
	got := runToolDefLoop(t, `
tool.def({
  name = "builtin:echo",
  description = "Lua echo",
  handler = function(args)
    return { ok = true, echoed = "LUA:" .. (args.text or "") }
  end,
})
tool.def({
  name = "lua:boom",
  handler = function(args)
    error("kaboom")
  end,
})

function loop()
  local msg = session.inbox()
  local n = 0
  local desc = nil
  for _, d in ipairs(tools.list()) do
    local f = d["function"]
    if f and f.name == "builtin:echo" then
      n = n + 1
      desc = f.description
    end
  end
  if n ~= 1 or desc ~= "Lua echo" then
    session.send("err: shadowed tool listed " .. n .. " times")
    return
  end
  local res = tools.run("builtin:echo", { text = "x" })
  if not (res and res.ok and res.echoed == "LUA:x") then
    session.send("err: shadow did not take over run")
    return
  end
  local bad = tools.run("lua:boom", {})
  if not (bad and bad.ok == false and bad.error and string.find(bad.error, "kaboom")) then
    session.send("err: handler error not returned as result")
    return
  end
  session.send("done")
end
`)
	if got != "done" {
		t.Fatalf("loop failed: %s", got)
	}
}
