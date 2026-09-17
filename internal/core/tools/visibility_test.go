package tools

import (
	"testing"

	"agentflow/internal/config"
)

// TestToolVisibilityRule: the skills/default decision is the single rule the
// Go registry and the Lua-declared path both use, so it is pinned here once.
func TestToolVisibilityRule(t *testing.T) {
	tests := []struct {
		name    string
		skills  []string
		def     string
		tool    string
		visible bool
	}{
		{name: "listed in skills", skills: []string{"a", "b"}, tool: "a", visible: true},
		{name: "not listed in skills", skills: []string{"a", "b"}, tool: "c"},
		{name: "skills win over default none", skills: []string{"a"}, def: "none", tool: "a", visible: true},
		{name: "skills win over default all", skills: []string{"a"}, def: "all", tool: "z"},
		{name: "no skills, default none", def: "none", tool: "a"},
		{name: "no skills, default all", def: "all", tool: "a", visible: true},
		{name: "no skills, default unset", def: "", tool: "a", visible: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := NewToolVisibility(tt.skills, config.ToolsPolicy{Default: tt.def})
			if got := v.Allows(tt.tool); got != tt.visible {
				t.Fatalf("Allows(%q) = %v; want %v", tt.tool, got, tt.visible)
			}
		})
	}
}

// TestAgentSetCarriesItsVisibility: Expose re-exports the rule it filtered by,
// which is what lets the prelude filter declared tools identically. A set
// built without one exposes nothing rather than defaulting to everything.
func TestAgentSetCarriesItsVisibility(t *testing.T) {
	r := overrideRegistry()
	as := r.Expose([]string{"builtin:web_search"}, config.ToolsPolicy{}, false)
	if !as.Visible.Allows("builtin:web_search") {
		t.Fatal("a listed tool must be visible on the set that exposed it")
	}
	if as.Visible.Allows("lua:anything") {
		t.Fatal("an unlisted name must not be visible")
	}

	// The zero value is closed, not open: a hand-built set must not leak.
	var zero AgentSet
	if zero.Visible.Allows("anything") {
		t.Fatal("the zero AgentSet must expose nothing")
	}
}

// TestExposeEmptySurface: default none with no skills is an empty surface, and
// the rule it carries says so — the Lua path reads the same answer.
func TestExposeEmptySurface(t *testing.T) {
	r := overrideRegistry()
	as := r.Expose(nil, config.ToolsPolicy{Default: "none"}, false)
	if len(as.Tools) != 0 || len(as.ByName) != 0 {
		t.Fatalf("default none with no skills must expose nothing: %+v", as.Tools)
	}
	if as.Visible.Allows("builtin:web_search") {
		t.Fatal("the carried rule must agree with the empty surface")
	}
}
