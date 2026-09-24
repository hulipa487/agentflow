package config

import (
	"strings"
	"testing"
)

// TestValidateToolsPolicyDefault: ToolVisibility treats anything that is not
// "none" as allow-all, so a typo like "non" exposed every tool in the registry.
// The permissive default stays — an omitted key means every tool, which is what
// the docs and every existing config assume — so what closes the hole is
// refusing the value that is neither.
func TestValidateToolsPolicyDefault(t *testing.T) {
	base := func(def string) *Config {
		return &Config{
			Agents: map[string]Agent{"bot": {Loop: "plugin:per_chat"}},
			Tools:  Tools{Policy: ToolsPolicy{Default: def}},
		}
	}
	for _, ok := range []string{"", "all", "none", "NONE", "All"} {
		if err := validate("cfg.yaml", base(ok)); err != nil {
			t.Errorf("tools.policy.default %q should validate: %v", ok, err)
		}
	}
	for _, bad := range []string{"non", "deny", "every", "true"} {
		err := validate("cfg.yaml", base(bad))
		if err == nil || !strings.Contains(err.Error(), "is not a policy") {
			t.Errorf("tools.policy.default %q must fail validation, got %v", bad, err)
		}
	}
}

// TestSessionStateIsDeclarable: caps/sessionstate.go tells a loop to declare
// `session.state` in its agent's capabilities, but the name was missing from the
// set the engine allows — so following that instruction failed the boot. The
// capability stays opt-in; it just has to be possible to opt in.
func TestSessionStateIsDeclarable(t *testing.T) {
	c := &Config{
		Agents: map[string]Agent{
			"bot": {Loop: "plugin:per_chat", Capabilities: []string{"llm.chat", "session.state"}},
		},
	}
	if err := validate("cfg.yaml", c); err != nil {
		t.Fatalf("session.state must be declarable: %v", err)
	}

	// An unknown capability is still refused, so the set has not simply opened.
	c.Agents["bot"] = Agent{Loop: "plugin:per_chat", Capabilities: []string{"session.states"}}
	err := validate("cfg.yaml", c)
	if err == nil || !strings.Contains(err.Error(), "not in plugins.allow_capabilities") {
		t.Fatalf("an unknown capability must still fail, got %v", err)
	}
}

// TestValidateSafetyProfiles: an unknown safety name used to resolve to
// safety.None with no warning, so a typo turned the core-owned chain off — the
// opposite of failing closed. Every non-builtin reference now needs a
// profiles.safety entry, and every filter named inside one has to exist.
func TestValidateSafetyProfiles(t *testing.T) {
	base := func(agentSafety string, filters []string) *Config {
		c := &Config{
			Agents: map[string]Agent{"bot": {Loop: "plugin:per_chat", Safety: agentSafety}},
		}
		if filters != nil {
			c.Profiles.Safety = map[string]SafetyProfile{"gentle": {Filters: filters}}
		}
		return c
	}

	// The built-in references need no entry.
	for _, ref := range []string{"", "none", "default"} {
		if err := validate("cfg.yaml", base(ref, nil)); err != nil {
			t.Errorf("safety %q should validate without a profile entry: %v", ref, err)
		}
	}
	// A named profile with real filters is fine.
	if err := validate("cfg.yaml", base("gentle", []string{"source-attribution", "affect-guard"})); err != nil {
		t.Fatalf("a named profile should validate: %v", err)
	}
	// An empty filter list is an explicit empty chain, not a mistake.
	if err := validate("cfg.yaml", base("gentle", []string{})); err != nil {
		t.Fatalf("an empty chain should validate: %v", err)
	}

	// An unknown agent reference is a boot error, not a silent opt-out.
	err := validate("cfg.yaml", base("typo", []string{"affect-guard"}))
	if err == nil || !strings.Contains(err.Error(), "unknown safety profile") {
		t.Fatalf("an unknown safety profile must fail, got %v", err)
	}
	// So is an unknown filter inside a profile: it would be dropped silently.
	err = validate("cfg.yaml", base("gentle", []string{"affect-gaurd"}))
	if err == nil || !strings.Contains(err.Error(), "unknown filter") {
		t.Fatalf("an unknown filter must fail, got %v", err)
	}
}
