package caps

import (
	"strings"
	"testing"

	"agentflow/internal/config"
)

// declListLua declares the standard lua:doc tool and reports which tool names
// the agent's tools.list() contains, sorted and comma-joined.
const declListLua = `
tool.def({
  name: "lua:doc",
  description: "literal declared desc",
  params: { type: "object", properties: { text: { type: "string", description: "literal param" } } },
  handler: function(_args) { return { ok: true }; },
});
function loop() {
  session.inbox();
  const names = [];
  for (const d of tools.list()) {
    const f = d["function"];
    if (f) names.push(f.name);
  }
  names.sort();
  session.send(names.join(","));
}
`

func TestDeclaredToolVisibleWhenListedInSkills(t *testing.T) {
	fx := newToolDefFixture(t, nil, nil)
	as := fx.agentFor([]string{"builtin:echo", "lua:doc"}, config.ToolsPolicy{})
	got := fx.runLoopAs(t, as, declListLua)
	if got != "builtin:echo,lua:doc" {
		t.Fatalf("got %q; want the declared tool listed alongside the Go one", got)
	}
}

func TestDeclaredToolHiddenWhenNotInSkills(t *testing.T) {
	fx := newToolDefFixture(t, nil, nil)
	as := fx.agentFor([]string{"builtin:echo"}, config.ToolsPolicy{})
	got := fx.runLoopAs(t, as, declListLua)
	if got != "builtin:echo" {
		t.Fatalf("got %q; want only the Go tool", got)
	}
}

func TestDeclaredToolHiddenUnderDefaultNone(t *testing.T) {
	fx := newToolDefFixture(t, nil, nil)
	as := fx.agentFor(nil, config.ToolsPolicy{Default: "none"})
	got := fx.runLoopAs(t, as, declListLua)
	if got != "" {
		t.Fatalf("got %q; want an empty surface under default: none", got)
	}
}

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

func TestHiddenDeclaredToolIsNotListedAtAll(t *testing.T) {
	fx := newToolDefFixture(t, nil, nil)
	as := fx.agentFor([]string{"lua:doc"}, config.ToolsPolicy{})
	got := fx.runLoopAs(t, as, `
tool.def({
  name: "builtin:echo",
  description: "lua echo",
  handler: function(_args) { return { ok: true, echoed: "LUA" }; },
});
tool.def({
  name: "lua:doc",
  description: "lua doc",
  handler: function(_args) { return { ok: true }; },
});
function loop() {
  session.inbox();
  const names = [];
  for (const d of tools.list()) {
    const f = d["function"];
    if (f) names.push(f.name);
  }
  names.sort();
  session.send(names.join(","));
}
`)
	if got != "lua:doc" {
		t.Fatalf("got %q; want only the visible declared tool", got)
	}
}

func TestForbiddenDoesNotGateDeclaredTool(t *testing.T) {
	fx := newToolDefFixture(t, nil, nil)
	as := fx.agentFor([]string{"builtin:echo", "lua:doc"}, config.ToolsPolicy{
		Forbidden: []string{"builtin:echo"},
	})
	got := fx.runLoopAs(t, as, `
tool.def({
  name: "builtin:echo",
  description: "lua echo",
  handler: function(_args) { return { ok: true, echoed: "LUA" }; },
});
tool.def({
  name: "lua:doc",
  description: "lua doc",
  handler: function(_args) { return { ok: true }; },
});
function loop() {
  session.inbox();
  const out = [];
  for (const d of tools.list()) {
    const f = d["function"];
    if (f) out.push(f.name + "=" + String(f.description));
  }
  out.sort();
  session.send(out.join("|"));
}
`)
	if got != "builtin:echo=lua echo|lua:doc=lua doc" {
		t.Fatalf("got %q; want the declared builtin:echo listed despite forbidden", got)
	}
}

func TestVisibleDeclaredToolStillShadowsGoTool(t *testing.T) {
	fx := newToolDefFixture(t, nil, nil)
	as := fx.agentFor([]string{"builtin:echo"}, config.ToolsPolicy{})
	got := fx.runLoopAs(t, as, `
tool.def({
  name: "builtin:echo",
  description: "lua echo",
  handler: function(_args) { return { ok: true, echoed: "LUA" }; },
});
function loop() {
  session.inbox();
  let n = 0;
  let desc = null;
  for (const d of tools.list()) {
    const f = d["function"];
    if (f && f.name === "builtin:echo") { n++; desc = f.description; }
  }
  if (n !== 1) {
    session.send("err: expected exactly one builtin:echo, got " + n);
    return;
  }
  session.send(desc);
}
`)
	if got != "lua echo" {
		t.Fatalf("got %q; want the declared tool to win", got)
	}
}

func TestVisibleDeclaredToolKeepsOverrideAndLivePrompt(t *testing.T) {
	fx := newToolDefFixture(t, map[string]config.ToolSpecOverride{
		"lua:doc": {Description: &config.PromptString{Value: "doc_prompt", IsRef: true}},
	}, map[string]string{"doc_prompt": "first text"})
	as := fx.agentFor([]string{"lua:doc"}, config.ToolsPolicy{})

	gw, a := fx.startAs(t, as, `
tool.def({
  name: "lua:doc",
  description: "literal declared desc",
  handler: function(_args) { return { ok: true }; },
});
function loop() {
  while (true) {
    session.inbox();
    let sent = false;
    for (const d of tools.list()) {
      const f = d["function"];
      if (f && f.name === "lua:doc") { session.send(f.description); sent = true; break; }
    }
    if (!sent) session.send("absent");
  }
}
`)
	if got := deliver(t, a, gw, 0, "m1"); got != "first text" {
		t.Fatalf("first turn: %q; want the overridden description", got)
	}
	fx.prompts.Set("doc_prompt", "second text")
	if got := deliver(t, a, gw, 1, "m2"); got != "second text" {
		t.Fatalf("second turn: %q; want the reloaded text", got)
	}
}

func TestHiddenDeclaredToolStillCountsAsDeclared(t *testing.T) {
	fx := newToolDefFixture(t, map[string]config.ToolSpecOverride{
		"lua:doc":   {Description: &config.PromptString{Value: "claimed"}},
		"lua:typoo": {Description: &config.PromptString{Value: "never declared"}},
	}, nil)
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

func TestHiddenDeclaredToolIsNotDispatchable(t *testing.T) {
	fx := newToolDefFixture(t, nil, nil)
	as := fx.agentFor(nil, config.ToolsPolicy{Default: "none"})
	got := fx.runLoopAs(t, as, `
tool.def({
  name: "lua:secret",
  handler: function(_args) {
    session.send("HANDLER-RAN");
    return { ok: true, leaked: "RAN" };
  },
});
function loop() {
  session.inbox();
  const n = tools.list().length;
  try {
    const res = tools.run("lua:secret", {});
    session.send("DISPATCHED|" + String(res.leaked));
  } catch (err) {
    session.send(n + "|refused|" + (err instanceof Error ? err.message : String(err)));
  }
}
`)
	if strings.Contains(got, "HANDLER-RAN") || strings.HasPrefix(got, "DISPATCHED") {
		t.Fatalf("the hidden handler ran: %q", got)
	}
	if !strings.HasPrefix(got, "0|refused|") {
		t.Fatalf("got %q; want an empty surface and a refused call", got)
	}
	if !strings.Contains(got, "not available") {
		t.Fatalf("got %q; want the same not-available answer a registered tool gives", got)
	}
}

func TestVisibleDeclaredToolIsDispatchable(t *testing.T) {
	fx := newToolDefFixture(t, nil, nil)
	as := fx.agentFor([]string{"lua:ok"}, config.ToolsPolicy{})
	got := fx.runLoopAs(t, as, `
tool.def({
  name: "lua:ok",
  handler: function(_args) { return { ok: true, ran: "YES" }; },
});
function loop() {
  session.inbox();
  session.send(String(tools.run("lua:ok", {}).ran));
}
`)
	if got != "YES" {
		t.Fatalf("got %q; want the visible declared tool to dispatch", got)
	}
}
