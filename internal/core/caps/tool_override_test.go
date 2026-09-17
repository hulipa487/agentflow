package caps

import (
	"strings"
	"testing"

	"agentflow/internal/config"
)

// toolDefText declares the standard lua:doc tool the override tests style: a
// literal description, two params (one with a literal description), and a
// required list.
const toolDefText = `
tool.def({
  name = "lua:doc",
  description = "literal description",
  params = { type = "object", properties = {
    text = { type = "string", description = "literal param" },
    count = { type = "number" },
  }, required = { "text" } },
  handler = function(args) return { ok = true } end,
})
`

// oneShotLoopBody reports lua:doc's listed text once.
const oneShotLoopBody = `
function loop()
  local msg = session.inbox()
  session.send(describe("lua:doc", "text"))
end
`

// TestDeclaredToolKeepsLiteralTextWithoutOverride: a declared tool with no
// tools.policy.overrides entry is unchanged — its own description and params
// are the fallback.
func TestDeclaredToolKeepsLiteralTextWithoutOverride(t *testing.T) {
	fx := newToolDefFixture(t, nil, nil)
	got := fx.runLoop(t, describeToolLua+oneShotLoopBody+toolDefText)
	if got != "literal description|literal param" {
		t.Fatalf("got %q; want the declared literal text", got)
	}
}

// TestLiteralOverrideReplacesDeclaredDescription: a literal description in
// tools.policy.overrides reaches a Lua-declared tool exactly as it reaches a
// Go one.
func TestLiteralOverrideReplacesDeclaredDescription(t *testing.T) {
	fx := newToolDefFixture(t, map[string]config.ToolSpecOverride{
		"lua:doc": {Description: &config.PromptString{Value: "from config"}},
	}, nil)
	got := fx.runLoop(t, describeToolLua+oneShotLoopBody+toolDefText)
	if got != "from config|literal param" {
		t.Fatalf("got %q; want the literal override applied", got)
	}
}

// TestPromptOverrideResolvesForDeclaredTool: a {prompt: key} description
// resolves against the prompt registry for a declared tool, same as for a Go
// tool.
func TestPromptOverrideResolvesForDeclaredTool(t *testing.T) {
	fx := newToolDefFixture(t, map[string]config.ToolSpecOverride{
		"lua:doc": {Description: &config.PromptString{Value: "doc_prompt", IsRef: true}},
	}, map[string]string{"doc_prompt": "from the prompt registry"})
	got := fx.runLoop(t, describeToolLua+oneShotLoopBody+toolDefText)
	if got != "from the prompt registry|literal param" {
		t.Fatalf("got %q; want the registry text", got)
	}
}

// TestParamOverrideRefinesOneParamOfDeclaredTool: a params.<param>.description
// override refines one declared parameter and leaves the rest of the schema
// (other params, types, required) untouched — and never mutates the loop's own
// table, which is reused on the next turn.
func TestParamOverrideRefinesOneParamOfDeclaredTool(t *testing.T) {
	fx := newToolDefFixture(t, map[string]config.ToolSpecOverride{
		"lua:doc": {Params: map[string]config.ToolParamOverride{
			"text":  {Description: config.PromptString{Value: "what to look up"}},
			"ghost": {Description: config.PromptString{Value: "not declared"}},
		}},
	}, nil)
	got := fx.runLoop(t, `
-- The loop keeps its own reference to the schema table it handed tool.def:
-- that table is reused on every turn, so an override must copy, never mutate.
local docParams = { type = "object", properties = {
  text = { type = "string", description = "literal param" },
  count = { type = "number" },
}, required = { "text" } }
tool.def({
  name = "lua:doc",
  description = "literal description",
  params = docParams,
  handler = function(args) return { ok = true } end,
})
function loop()
  local msg = session.inbox()
  for _, d in ipairs(tools.list()) do
    local f = d["function"]
    if f and f.name == "lua:doc" then
      local props = f.parameters.properties
      if not (props.text.description == "what to look up") then
        session.send("err: param description not overridden: " .. json.encode(props))
        return
      end
      if props.text.type ~= "string" or props.count.type ~= "number" then
        session.send("err: schema clobbered: " .. json.encode(props))
        return
      end
      if props.ghost ~= nil then
        session.send("err: undeclared param was added")
        return
      end
      if f.parameters.required[1] ~= "text" then
        session.send("err: required clobbered")
        return
      end
      -- The loop's own table must be untouched.
      if docParams.properties.text.description ~= "literal param" then
        session.send("err: the declared spec table was mutated in place")
        return
      end
      -- And a second call must not drift.
      for _, d2 in ipairs(tools.list()) do
        local f2 = d2["function"]
        if f2 and f2.name == "lua:doc" and f2.parameters.properties.text.description ~= "what to look up" then
          session.send("err: second call disagreed")
          return
        end
      end
      session.send("ok")
      return
    end
  end
  session.send("err: tool absent")
end
`)
	if got != "ok" {
		t.Fatalf("loop failed: %s", got)
	}
}

// TestPromptEditChangesDeclaredToolTextNextCall: the override is resolved when
// tools.list() is built, against the live registry — so editing a file:-backed
// prompt changes what the model sees on the very next call, with no restart.
func TestPromptEditChangesDeclaredToolTextNextCall(t *testing.T) {
	fx := newToolDefFixture(t, map[string]config.ToolSpecOverride{
		"lua:doc": {Description: &config.PromptString{Value: "doc_prompt", IsRef: true}},
	}, map[string]string{"doc_prompt": "first text"})

	gw, a := fx.start(t, describeToolLua+`
function loop()
  while true do
    local msg = session.inbox()
    session.send(describe("lua:doc", "text"))
  end
end
`+toolDefText)

	if got := deliver(t, a, gw, 0, "m1"); got != "first text|literal param" {
		t.Fatalf("first turn: %q", got)
	}
	// Exactly what the reload watcher does when a file:-backed prompt changes.
	fx.prompts.Set("doc_prompt", "second text")
	if got := deliver(t, a, gw, 1, "m2"); got != "second text|literal param" {
		t.Fatalf("second turn (same session): %q; want the reloaded text", got)
	}
	if strings.Contains(fx.logs.String(), "restarting loop") {
		t.Fatal("a prompt edit must not restart the session")
	}
}

// TestDeclaredShadowOfGoToolTakesTheOverride: a declared name shadows a
// Go-registered tool of the same name (one list entry, Lua handler wins), and
// the override still applies to the winner.
func TestDeclaredShadowOfGoToolTakesTheOverride(t *testing.T) {
	fx := newToolDefFixture(t, map[string]config.ToolSpecOverride{
		"builtin:echo": {Description: &config.PromptString{Value: "overridden echo"}},
	}, nil)
	got := fx.runLoop(t, `
tool.def({
  name = "builtin:echo",
  description = "lua echo",
  handler = function(args) return { ok = true, echoed = "LUA:" .. (args.text or "") } end,
})
function loop()
  local msg = session.inbox()
  local n, desc = 0, nil
  for _, d in ipairs(tools.list()) do
    local f = d["function"]
    if f and f.name == "builtin:echo" then n = n + 1; desc = f.description end
  end
  if n ~= 1 then
    session.send("err: shadowed tool listed " .. n .. " times")
    return
  end
  if desc ~= "overridden echo" then
    session.send("err: override not applied to the winner: " .. tostring(desc))
    return
  end
  local res = tools.run("builtin:echo", { text = "x" })
  if not (res and res.ok and res.echoed == "LUA:x") then
    session.send("err: shadow did not take over run")
    return
  end
  session.send("done")
end
`)
	if got != "done" {
		t.Fatalf("loop failed: %s", got)
	}
}

// TestOverrideNeverAddsAListEntry: an override does not conjure a tool into
// the list. Exposure is unchanged — a name no loop declares (and that is not a
// registered Go tool) produces no entry, and the tools that are declared keep
// theirs.
func TestOverrideNeverAddsAListEntry(t *testing.T) {
	fx := newToolDefFixture(t, map[string]config.ToolSpecOverride{
		"lua:absent": {Description: &config.PromptString{Value: "should never appear"}},
	}, nil)
	got := fx.runLoop(t, describeToolLua+`
function loop()
  local msg = session.inbox()
  for _, d in ipairs(tools.list()) do
    local f = d["function"]
    if f and f.name == "lua:absent" then
      session.send("err: an override conjured a tool into the list")
      return
    end
  end
  session.send(describe("lua:doc", "text"))
end
`+toolDefText)
	if got != "literal description|literal param" {
		t.Fatalf("got %q; want the declared tool unchanged and no extra entry", got)
	}
}

// TestUnclaimedOverrideNameIsReported: an override naming neither a registered
// tool nor a tool any loaded loop declares is a misspelling that would
// otherwise do nothing at all. Boot cannot see Lua tool names, so it is
// reported when a loop declares its tools instead of passing silently.
func TestUnclaimedOverrideNameIsReported(t *testing.T) {
	fx := newToolDefFixture(t, map[string]config.ToolSpecOverride{
		"lua:doc":   {Description: &config.PromptString{Value: "claimed"}},
		"lua:typoo": {Description: &config.PromptString{Value: "never declared"}},
	}, nil)
	if got := fx.runLoop(t, describeToolLua+oneShotLoopBody+toolDefText); got != "claimed|literal param" {
		t.Fatalf("got %q; want the claimed override applied", got)
	}
	if !strings.Contains(fx.logs.String(), "lua:typoo") {
		t.Fatalf("the unclaimed override was not reported:\n%s", fx.logs.String())
	}
	if strings.Contains(fx.logs.String(), `tool=lua:doc`) {
		t.Fatalf("a claimed name must not be reported:\n%s", fx.logs.String())
	}
}
