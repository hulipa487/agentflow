package caps

import (
	"strings"
	"testing"

	"agentflow/internal/config"
)

// declListLua declares the standard lua:doc tool and reports which tool names
// the agent's tools.list() contains, sorted and comma-joined. A name the agent
// cannot see is simply absent.
const declListLua = `
tool.def({
  name = "lua:doc",
  description = "literal declared desc",
  params = { type = "object", properties = { text = { type = "string", description = "literal param" } } },
  handler = function(args) return { ok = true } end,
})
function loop()
  local msg = session.inbox()
  local names = {}
  for _, d in ipairs(tools.list()) do
    local f = d["function"]
    if f then names[#names + 1] = f.name end
  end
  table.sort(names)
  session.send(table.concat(names, ","))
end
`

// TestDeclaredToolVisibleWhenListedInSkills: skills: governs a declared tool
// exactly as it governs a Go tool.
func TestDeclaredToolVisibleWhenListedInSkills(t *testing.T) {
	fx := newToolDefFixture(t, nil, nil)
	as := fx.agentFor([]string{"builtin:echo", "lua:doc"}, config.ToolsPolicy{})
	got := fx.runLoopAs(t, as, declListLua)
	if got != "builtin:echo,lua:doc" {
		t.Fatalf("got %q; want the declared tool listed alongside the Go one", got)
	}
}

// TestDeclaredToolHiddenWhenNotInSkills: a non-empty skills: list that omits
// the declared tool hides it — the per-agent surface that a shared loop
// directory depends on.
func TestDeclaredToolHiddenWhenNotInSkills(t *testing.T) {
	fx := newToolDefFixture(t, nil, nil)
	as := fx.agentFor([]string{"builtin:echo"}, config.ToolsPolicy{})
	got := fx.runLoopAs(t, as, declListLua)
	if got != "builtin:echo" {
		t.Fatalf("got %q; want only the Go tool", got)
	}
}

// TestDeclaredToolHiddenUnderDefaultNone: with no skills and
// tools.policy.default: none nothing is visible, declared tools included.
func TestDeclaredToolHiddenUnderDefaultNone(t *testing.T) {
	fx := newToolDefFixture(t, nil, nil)
	as := fx.agentFor(nil, config.ToolsPolicy{Default: "none"})
	got := fx.runLoopAs(t, as, declListLua)
	if got != "" {
		t.Fatalf("got %q; want an empty surface under default: none", got)
	}
}

// TestDeclaredToolVisibleUnderDefaultAll: no skills and default all (or unset)
// exposes everything, declared tools included — the pre-existing behavior for
// a deployment that never set a default.
func TestDeclaredToolVisibleUnderDefaultAll(t *testing.T) {
	for _, def := range []string{"", "all"} {
		fx := newToolDefFixture(t, nil, nil)
		as := fx.agentFor(nil, config.ToolsPolicy{Default: def})
		got := fx.runLoopAs(t, as, declListLua)
		if got != "builtin:echo,lua:doc" {
			t.Fatalf("default %q: got %q; want both visible", def, got)
		}
	}
}

// TestSharedLoopDifferentSkillsDifferentSurfaces: two agents running the same
// loop chunk over one registry get different tool surfaces. This is the whole
// point of the change — before it, both saw every declared tool.
func TestSharedLoopDifferentSkillsDifferentSurfaces(t *testing.T) {
	fx := newToolDefFixture(t, nil, nil)
	docAgent := fx.agentFor([]string{"lua:doc"}, config.ToolsPolicy{})
	echoAgent := fx.agentFor([]string{"builtin:echo"}, config.ToolsPolicy{})

	if got := fx.runLoopAs(t, docAgent, declListLua); got != "lua:doc" {
		t.Fatalf("doc agent got %q; want only lua:doc", got)
	}
	if got := fx.runLoopAs(t, echoAgent, declListLua); got != "builtin:echo" {
		t.Fatalf("echo agent got %q; want only builtin:echo", got)
	}
}

// TestHiddenDeclaredToolIsNotListedAtAll: a declared tool the agent cannot see
// leaves no trace — it is absent, and it does not drag a half-applied shadow
// onto the Go tool it shares a name with. The filter runs before shadowing.
//
// Note this pins the reachable contract only: because visibility keys on the
// tool name, a Go tool and a declared tool of the same name always share it
// (skills lists the name or it does not), so "visible Go tool whose declared
// namesake is hidden" cannot arise. The order still matters as an invariant —
// filtering after shadowing would drop the Go tool on behalf of a declared one
// that is then discarded.
func TestHiddenDeclaredToolIsNotListedAtAll(t *testing.T) {
	fx := newToolDefFixture(t, nil, nil)
	// skills names lua:doc only, so the declared builtin:echo is hidden.
	as := fx.agentFor([]string{"lua:doc"}, config.ToolsPolicy{})
	got := fx.runLoopAs(t, as, `
tool.def({
  name = "builtin:echo",
  description = "lua echo",
  handler = function(args) return { ok = true, echoed = "LUA" } end,
})
tool.def({
  name = "lua:doc",
  description = "lua doc",
  handler = function(args) return { ok = true } end,
})
function loop()
  local msg = session.inbox()
  local names = {}
  for _, d in ipairs(tools.list()) do
    local f = d["function"]
    if f then names[#names + 1] = f.name end
  end
  table.sort(names)
  session.send(table.concat(names, ","))
end
`)
	if got != "lua:doc" {
		t.Fatalf("got %q; want only the visible declared tool", got)
	}
}

// TestForbiddenDoesNotGateDeclaredTool: tools.policy.forbidden hides the
// registered tool of that name but leaves a declared tool of the same name
// visible — the documented "declared tools are the loop's own code" contract,
// unchanged by this pass. It is also the one reachable case where the Go tool
// and its declared namesake differ, and it runs the other way: the declared
// winner is listed, the forbidden Go tool is gone.
func TestForbiddenDoesNotGateDeclaredTool(t *testing.T) {
	fx := newToolDefFixture(t, nil, nil)
	as := fx.agentFor([]string{"builtin:echo", "lua:doc"}, config.ToolsPolicy{
		Forbidden: []string{"builtin:echo"},
	})
	got := fx.runLoopAs(t, as, `
tool.def({
  name = "builtin:echo",
  description = "lua echo",
  handler = function(args) return { ok = true, echoed = "LUA" } end,
})
tool.def({
  name = "lua:doc",
  description = "lua doc",
  handler = function(args) return { ok = true } end,
})
function loop()
  local msg = session.inbox()
  local out = {}
  for _, d in ipairs(tools.list()) do
    local f = d["function"]
    if f then out[#out + 1] = f.name .. "=" .. tostring(f.description) end
  end
  table.sort(out)
  session.send(table.concat(out, "|"))
end
`)
	if got != "builtin:echo=lua echo|lua:doc=lua doc" {
		t.Fatalf("got %q; want the declared builtin:echo listed despite forbidden", got)
	}
}

// TestVisibleDeclaredToolStillShadowsGoTool: a declared tool the agent can see
// wins over the Go tool it shares a name with, as before.
func TestVisibleDeclaredToolStillShadowsGoTool(t *testing.T) {
	fx := newToolDefFixture(t, nil, nil)
	as := fx.agentFor([]string{"builtin:echo"}, config.ToolsPolicy{})
	got := fx.runLoopAs(t, as, `
tool.def({
  name = "builtin:echo",
  description = "lua echo",
  handler = function(args) return { ok = true, echoed = "LUA" } end,
})
function loop()
  local msg = session.inbox()
  local n, desc = 0, nil
  for _, d in ipairs(tools.list()) do
    local f = d["function"]
    if f and f.name == "builtin:echo" then n = n + 1; desc = f.description end
  end
  if n ~= 1 then
    session.send("err: expected exactly one builtin:echo, got " .. n)
    return
  end
  session.send(desc)
end
`)
	if got != "lua echo" {
		t.Fatalf("got %q; want the declared tool to win", got)
	}
}

// TestVisibleDeclaredToolKeepsOverrideAndLivePrompt: an override still applies
// to a declared tool that is visible, and a file:-backed prompt edit still
// lands on the next turn — the skills filter must not bypass the resolution
// path.
func TestVisibleDeclaredToolKeepsOverrideAndLivePrompt(t *testing.T) {
	fx := newToolDefFixture(t, map[string]config.ToolSpecOverride{
		"lua:doc": {Description: &config.PromptString{Value: "doc_prompt", IsRef: true}},
	}, map[string]string{"doc_prompt": "first text"})
	as := fx.agentFor([]string{"lua:doc"}, config.ToolsPolicy{})

	gw, a := fx.startAs(t, as, `
tool.def({
  name = "lua:doc",
  description = "literal declared desc",
  handler = function(args) return { ok = true } end,
})
function loop()
  while true do
    local msg = session.inbox()
    for _, d in ipairs(tools.list()) do
      local f = d["function"]
      if f and f.name == "lua:doc" then session.send(f.description) return end
    end
    session.send("absent")
  end
end
`)
	if got := deliver(t, a, gw, 0, "m1"); got != "first text" {
		t.Fatalf("first turn: %q; want the overridden description", got)
	}
	fx.prompts.Set("doc_prompt", "second text")
	if got := deliver(t, a, gw, 1, "m2"); got != "second text" {
		t.Fatalf("second turn: %q; want the reloaded text", got)
	}
}

// TestHiddenDeclaredToolStillCountsAsDeclared: a declared tool the agent
// cannot see still reaches the overrides resolver, so an override aimed at it
// is not misreported as naming a tool no loop declares. Visibility is about
// the model's surface, not about what the chunk declared.
func TestHiddenDeclaredToolStillCountsAsDeclared(t *testing.T) {
	fx := newToolDefFixture(t, map[string]config.ToolSpecOverride{
		"lua:doc":   {Description: &config.PromptString{Value: "claimed"}},
		"lua:typoo": {Description: &config.PromptString{Value: "never declared"}},
	}, nil)
	// skills names the Go tool only: lua:doc is declared but hidden.
	as := fx.agentFor([]string{"builtin:echo"}, config.ToolsPolicy{})
	if got := fx.runLoopAs(t, as, declListLua); got != "builtin:echo" {
		t.Fatalf("setup: got %q; want lua:doc hidden", got)
	}
	if strings.Contains(fx.logs.String(), "lua:doc") {
		t.Fatalf("a hidden but declared tool was reported unclaimed:\n%s", fx.logs.String())
	}
	if !strings.Contains(fx.logs.String(), "lua:typoo") {
		t.Fatalf("a genuinely unclaimed override was not reported:\n%s", fx.logs.String())
	}
}
