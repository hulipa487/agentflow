package tools

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"agentflow/internal/config"
)

// strPtr is a literal description override.
func strPtr(s string) *config.PromptString {
	return &config.PromptString{Value: s}
}

// promptRef is a description override sourced from the prompts registry.
func promptRef(key string) *config.PromptString {
	return &config.PromptString{Value: key, IsRef: true}
}

func boolPtr(b bool) *bool { return &b }

// overrideRegistry returns a registry with one schema-carrying tool.
func overrideRegistry() *Registry {
	r := NewRegistry()
	r.Register(ToolSpec{
		Name:        "builtin:web_search",
		Description: "Search the public web.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Search query"},
				"count": map[string]any{"type": "number"},
			},
			"required": []string{"query"},
		},
		Autonomous: true,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			return map[string]any{"ok": true}, nil
		},
	})
	return r
}

// TestApplyOverridesReplacesDescription: a description override replaces the
// registered description verbatim, visible through Expose/tools.list.
func TestApplyOverridesReplacesDescription(t *testing.T) {
	r := overrideRegistry()
	err := r.ApplyOverrides(map[string]config.ToolSpecOverride{
		"builtin:web_search": {Description: strPtr("Search the deployment's runbooks.")},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	as := r.Expose(nil, config.ToolsPolicy{}, false)
	if got := as.ByName["builtin:web_search"].Description; got != "Search the deployment's runbooks." {
		t.Fatalf("description not overridden: %q", got)
	}
}

// TestApplyOverridesMergesParamDescription: param overrides shallow-merge into
// parameters.properties — the rest of the schema (types, required) is
// untouched. A param the schema does not declare is ignored without error.
func TestApplyOverridesMergesParamDescription(t *testing.T) {
	r := overrideRegistry()
	err := r.ApplyOverrides(map[string]config.ToolSpecOverride{
		"builtin:web_search": {
			Params: map[string]config.ToolParamOverride{
				"query": {Description: config.PromptString{Value: "What to look up, in the user's words"}},
				"bogus": {Description: config.PromptString{Value: "not in the schema"}},
			},
		},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	as := r.Expose(nil, config.ToolsPolicy{}, false)
	params := as.ByName["builtin:web_search"].Parameters
	props := params["properties"].(map[string]any)
	if got := props["query"].(map[string]any)["description"]; got != "What to look up, in the user's words" {
		t.Fatalf("param description not merged: %v", got)
	}
	if got := props["query"].(map[string]any)["type"]; got != "string" {
		t.Fatalf("param type clobbered: %v", got)
	}
	if _, declared := props["count"]; !declared {
		t.Fatal("unrelated param dropped")
	}
	if _, added := props["bogus"]; added {
		t.Fatal("undeclared param must not be added to the schema")
	}
	req, ok := params["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "query" {
		t.Fatalf("required clobbered: %v", params["required"])
	}
}

// TestApplyOverridesIgnoresUnregistered: an override naming no registered Go
// tool is not a boot error — it may target a Lua-declared tool, which exists
// only once a loop chunk loads. It is left for LuaOverrides to resolve, while
// the registered tools still get theirs in the same pass.
func TestApplyOverridesIgnoresUnregistered(t *testing.T) {
	r := overrideRegistry()
	err := r.ApplyOverrides(map[string]config.ToolSpecOverride{
		"builtin:web_search": {Description: strPtr("Search the deployment's runbooks.")},
		"lua:shout":          {Description: strPtr("Shout it")},
	}, nil, nil)
	if err != nil {
		t.Fatalf("an unregistered name must not fail the boot: %v", err)
	}
	// The registered tool was still baked in the same call.
	as := r.Expose(nil, config.ToolsPolicy{}, false)
	if got := as.ByName["builtin:web_search"].Description; got != "Search the deployment's runbooks." {
		t.Fatalf("registered override not applied: %q", got)
	}
	// The unregistered one resolves through the Lua path instead.
	lua := NewLuaOverrides(map[string]config.ToolSpecOverride{
		"lua:shout": {Description: strPtr("Shout it")},
	}, r.Names())
	res := lua.For([]string{"lua:shout"}, nil, nil)
	if got := res["lua:shout"].Description; got == nil || *got != "Shout it" {
		t.Fatalf("lua override not resolved: %+v", res["lua:shout"])
	}
	// A registered name is never reported as unclaimed, even when no loop
	// declares it.
	logs := &strings.Builder{}
	lua2 := NewLuaOverrides(map[string]config.ToolSpecOverride{
		"builtin:web_search": {Description: strPtr("x")},
	}, r.Names())
	lua2.For(nil, nil, slog.New(slog.NewTextHandler(logs, nil)))
	if logs.Len() != 0 {
		t.Fatalf("a registered tool must not be reported unclaimed: %s", logs)
	}
}

// TestApplyOverridesUnknownPromptOnUnregisteredFails: a Lua-targeted override
// referencing a prompt key that does not exist is still a boot error, so a
// misspelled key cannot silently do nothing until a loop runs.
func TestApplyOverridesUnknownPromptOnUnregisteredFails(t *testing.T) {
	r := overrideRegistry()
	err := r.ApplyOverrides(map[string]config.ToolSpecOverride{
		"lua:shout": {
			Description: promptRef("missing"),
			Params:      map[string]config.ToolParamOverride{"text": {Description: config.PromptString{Value: "also_missing", IsRef: true}}},
		},
	}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("unknown prompt key must fail the boot, got %v", err)
	}
}

// TestApplyOverridesPolicyFieldsFlip: policy fields bake into the canonical
// spec; an exposed set then enforces them (needs_confirm gates Invoke).
func TestApplyOverridesPolicyFieldsFlip(t *testing.T) {
	r := overrideRegistry()
	err := r.ApplyOverrides(map[string]config.ToolSpecOverride{
		"builtin:web_search": {NeedsConfirm: boolPtr(true), Autonomous: boolPtr(false)},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Expose with an empty policy: the baked-in fields must hold on their own.
	as := r.Expose(nil, config.ToolsPolicy{}, false)
	spec := as.ByName["builtin:web_search"]
	if !spec.NeedsConfirm || spec.Autonomous {
		t.Fatalf("policy fields not applied: %+v", spec)
	}
	res, err := as.Invoke(context.Background(), "builtin:web_search", map[string]any{"query": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if m := res.(map[string]any); m["needs_confirm"] != true {
		t.Fatalf("expected needs_confirm gate, got %v", m)
	}
}

// TestApplyOverridesPromptDescription: a description override may name a
// prompts: registry key instead of a literal; the registry text replaces the
// registered description, and an unknown key fails the boot.
func TestApplyOverridesPromptDescription(t *testing.T) {
	prompts := map[string]string{"search_desc": "Search the deployment's runbooks."}

	r := overrideRegistry()
	err := r.ApplyOverrides(map[string]config.ToolSpecOverride{
		"builtin:web_search": {
			Description: promptRef("search_desc"),
			Params: map[string]config.ToolParamOverride{
				"query": {Description: config.PromptString{Value: "query_help", IsRef: true}},
			},
		},
	}, map[string]string{"search_desc": "Search the deployment's runbooks.", "query_help": "What to look up"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	as := r.Expose(nil, config.ToolsPolicy{}, false)
	spec := as.ByName["builtin:web_search"]
	if spec.Description != prompts["search_desc"] {
		t.Fatalf("prompt description not resolved: %q", spec.Description)
	}
	props := spec.Parameters["properties"].(map[string]any)
	if got := props["query"].(map[string]any)["description"]; got != "What to look up" {
		t.Fatalf("param prompt description not resolved: %v", got)
	}
}

// TestApplyOverridesUnknownPromptFails: a description referencing a prompt key
// the registry does not define is a boot error naming the tool, never a silent
// empty description.
func TestApplyOverridesUnknownPromptFails(t *testing.T) {
	r := overrideRegistry()
	err := r.ApplyOverrides(map[string]config.ToolSpecOverride{
		"builtin:web_search": {Description: promptRef("missing")},
	}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("unknown prompt key must fail, got %v", err)
	}
}

// TestApplyOverridesEmptyUnchanged: the no-override path is byte-identical to
// today — nothing is mutated, nothing errors.
func TestApplyOverridesEmptyUnchanged(t *testing.T) {
	r := overrideRegistry()
	before := r.Expose(nil, config.ToolsPolicy{}, false).ByName["builtin:web_search"]
	if err := r.ApplyOverrides(nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.ApplyOverrides(map[string]config.ToolSpecOverride{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	after := r.Expose(nil, config.ToolsPolicy{}, false).ByName["builtin:web_search"]
	if before.Description != after.Description || before.Autonomous != after.Autonomous ||
		before.NeedsConfirm != after.NeedsConfirm || before.Permission != after.Permission {
		t.Fatal("empty overrides mutated the spec")
	}
}

// TestApplyOverridesThenJSONNormalized: overrides are baked before any
// emit path, and every emit path runs NormalizeSchema — an overridden schema
// can never surface a mangled "required": {} to a strict provider.
func TestApplyOverridesThenJSONNormalized(t *testing.T) {
	r := overrideRegistry()
	err := r.ApplyOverrides(map[string]config.ToolSpecOverride{
		"builtin:web_search": {
			Description: strPtr("Search the deployment's runbooks."),
			Params:      map[string]config.ToolParamOverride{"query": {Description: config.PromptString{Value: "What to look up"}}},
		},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	j := r.Expose(nil, config.ToolsPolicy{}, false).ByName["builtin:web_search"].JSON()
	fn := j["function"].(map[string]any)
	if fn["description"] != "Search the deployment's runbooks." {
		t.Fatalf("override not visible in JSON: %v", fn["description"])
	}
	params := fn["parameters"].(map[string]any)
	switch req := params["required"].(type) {
	case []string:
		if len(req) == 0 {
			t.Fatal("empty required should have been dropped")
		}
	case map[string]any:
		t.Fatalf("object-form required leaked: %v", req)
	}
	props := params["properties"].(map[string]any)
	if got := props["query"].(map[string]any)["description"]; got != "What to look up" {
		t.Fatalf("param override not visible in JSON: %v", got)
	}
}
