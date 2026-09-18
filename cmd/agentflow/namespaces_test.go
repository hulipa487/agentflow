package main

import (
	"strings"
	"testing"

	"agentflow/internal/builtins"
)

// TestNamespaceHints: "builtin:" used to be spelled by four unrelated
// vocabularies — loops, tools, memory providers and the built-in memory
// profile — and nothing in the string said which one a name belonged to. Only
// tools keep it now, so a name from any of the others has to say where it went.
func TestNamespaceHints(t *testing.T) {
	tools := []string{"builtin:web_search", "builtin:fetch", "builtin:legal_read"}
	loops := builtins.Names()

	// A loop: field holding a tool name. This errors today but never says what
	// to write instead.
	got := loopHint("builtin:web_search", tools, loops)
	if !strings.Contains(got, "skills:") || !strings.Contains(got, "loop:") {
		t.Fatalf("loopHint = %q; it must name both fields", got)
	}

	// A loop name in its pre-rename spelling now names nothing at all.
	got = loopHint("builtin:per_chat", tools, loops)
	if !strings.Contains(got, "plugin:per_chat") {
		t.Fatalf("loopHint on the old spelling = %q; it must give the new one", got)
	}

	// A loop name in loop: is valid, so there is nothing to hint.
	if h := loopHint("plugin:per_chat", tools, loops); h != "" {
		t.Fatalf("a valid loop name must produce no hint, got %q", h)
	}
	// So is a path.
	if h := loopHint("./loops/bot.lua", tools, loops); h != "" {
		t.Fatalf("a path must produce no hint, got %q", h)
	}
	// And an unknown name is somebody's own file, not a mistake to report.
	if h := loopHint("my_loop.lua", tools, loops); h != "" {
		t.Fatalf("an unrelated value must produce no hint, got %q", h)
	}

	// A skills: entry holding a loop name. Unlike the above this fails
	// silently — the entry matches no tool, is dropped, and the agent looks
	// toolless — which is why it is worth a warning.
	got = skillHint("plugin:per_chat", loops)
	if !strings.Contains(got, "loop:") || !strings.Contains(got, "skills:") {
		t.Fatalf("skillHint = %q; it must name both fields", got)
	}
	// Same, in the old spelling: it is still a loop name, not a tool.
	got = skillHint("builtin:per_chat", loops)
	if !strings.Contains(got, "plugin:per_chat") {
		t.Fatalf("skillHint on the old spelling = %q; it must give the new one", got)
	}
	if h := skillHint("builtin:web_search", loops); h != "" {
		t.Fatalf("a real tool name must produce no hint, got %q", h)
	}
	// A loop-declared tool (tool.def) exists only once a chunk loads, so an
	// unknown name is not evidence of a mistake and must stay quiet.
	if h := skillHint("my_own_tool", loops); h != "" {
		t.Fatalf("an unknown name may be a declared tool and must produce no hint, got %q", h)
	}
}

// TestBuiltinNames pins the vocabulary the hints match against. If a builtin is
// added without this list moving, a skills: entry naming it stops being caught.
func TestBuiltinNames(t *testing.T) {
	names := builtins.Names()
	if len(names) == 0 {
		t.Fatal("the builtin plugin names must be exported")
	}
	for _, want := range []string{"plugin:per_chat", "plugin:recency", "plugin:semantic", "plugin:ttl"} {
		if !contains(names, want) {
			t.Fatalf("%q must be in %v", want, names)
		}
	}
	// The config spelling carries the prefix. Returning bare names would make
	// every comparison against a configured value silently fail — a hint that
	// never fires looks exactly like a configuration with nothing wrong.
	for _, n := range names {
		if !strings.HasPrefix(n, "plugin:") {
			t.Fatalf("plugin names must carry the config spelling, got %q", n)
		}
	}
}

// TestBuiltinPrefixIsReservedForTools: the four vocabularies no longer collide.
// Anything still spelled "builtin:" is a tool name, which is what makes the
// prefix mean one thing.
func TestBuiltinPrefixIsReservedForTools(t *testing.T) {
	for _, n := range builtins.Names() {
		if strings.HasPrefix(n, "builtin:") {
			t.Fatalf("%q still uses the tool prefix", n)
		}
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
