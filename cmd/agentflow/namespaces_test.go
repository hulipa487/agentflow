package main

import (
	"strings"
	"testing"

	"agentflow/internal/builtins"
)

// TestNamespaceHints: "builtin:" is spelled by three unrelated vocabularies —
// loops, tools and memory providers — and nothing in the string says which one
// a name belongs to. These hints are what turn the resulting confusion into an
// instruction.
func TestNamespaceHints(t *testing.T) {
	tools := []string{"builtin:web_search", "builtin:fetch", "builtin:legal_read"}
	loops := builtins.Names()

	// A loop: field holding a tool name. This errors today ("unknown builtin")
	// but never says what to write instead.
	got := loopHint("builtin:web_search", tools)
	if !strings.Contains(got, "skills:") || !strings.Contains(got, "loop:") {
		t.Fatalf("loopHint = %q; it must name both fields", got)
	}
	// A loop name in loop: is valid, so there is nothing to hint.
	if h := loopHint("builtin:per_chat", tools); h != "" {
		t.Fatalf("a valid loop name must produce no hint, got %q", h)
	}
	// So is a path.
	if h := loopHint("./loops/bot.lua", tools); h != "" {
		t.Fatalf("a path must produce no hint, got %q", h)
	}

	// A skills: entry holding a loop name. Unlike the above this fails
	// silently — the entry matches no tool, is dropped, and the agent looks
	// toolless — which is why it is worth a warning.
	got = skillHint("builtin:per_chat", loops)
	if !strings.Contains(got, "loop:") || !strings.Contains(got, "skills:") {
		t.Fatalf("skillHint = %q; it must name both fields", got)
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

// TestBuiltinNames pins the loop vocabulary the hints match against. If a
// builtin is added without this list moving, a skills: entry naming it stops
// being caught.
func TestBuiltinNames(t *testing.T) {
	names := builtins.Names()
	if len(names) == 0 {
		t.Fatal("the builtin loop names must be exported")
	}
	for _, want := range []string{"builtin:per_chat", "builtin:recency", "builtin:semantic", "builtin:ttl"} {
		if !contains(names, want) {
			t.Fatalf("%q must be in %v", want, names)
		}
	}
	// The config spelling carries the prefix. Returning bare names would make
	// every comparison against a configured value silently fail — a hint that
	// never fires looks exactly like a configuration with nothing wrong.
	for _, n := range names {
		if !strings.HasPrefix(n, "builtin:") {
			t.Fatalf("builtin names must carry the config spelling, got %q", n)
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
