package caps

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/session"
	"agentflow/internal/core/tools"
)

// runToolDefLoop drives one actor with the given loop source and returns the
// first session.send text. The registry carries one Go tool (builtin:echo) so
// fallthrough from a Lua-declared tools.run back to the Go path is exercised.
func runToolDefLoop(t *testing.T, loopSrc string) string {
	t.Helper()

	reg := tools.NewRegistry()
	reg.Register(tools.ToolSpec{
		Name:        "builtin:echo",
		Description: "Echo the text back",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
		},
		Autonomous: true,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			return map[string]any{"ok": true, "echoed": args["text"]}, nil
		},
	})
	agentSet := reg.Expose(nil, config.ToolsPolicy{}, false)

	gw := &schemaGW{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := session.New("main|test",
		session.Identity{SessionID: "main|test", Agent: "main", Capabilities: map[string]bool{"tools": true}},
		&session.Info{Name: "main", HistoryBudget: 100},
		gw, nil, nil, nil, nil, ToolHandlers(agentSet), pool.New(1), log)
	a.LoopSrc = loopSrc

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
	return gw.sends[0]
}

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
