package main

import (
	"testing"

	"agentflow/internal/config"
)

// TestTriggerTarget: a scheduled trigger's target.profile resolves to a
// configured agent by name, or to the "spawn:<profile>" address the supervisor
// registers for a spawn profile. Anything else is unresolvable (the trigger
// scheduler logs it and skips the trigger — never a boot failure).
func TestTriggerTarget(t *testing.T) {
	cfg := &config.Config{
		Agents:   map[string]config.Agent{"digest": {Loop: "x.lua"}},
		Profiles: config.Profiles{Agent: map[string]config.SpawnProfile{"pm": {Loop: "y.lua"}}},
	}
	cases := []struct {
		profile string
		want    string
		ok      bool
	}{
		{"digest", "digest", true},
		{"pm", "spawn:pm", true},
		{"nope", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := triggerTarget(cfg, c.profile)
		if ok != c.ok || got != c.want {
			t.Fatalf("triggerTarget(%q) = %q, %v; want %q, %v", c.profile, got, ok, c.want, c.ok)
		}
	}
}

// TestCheckConfigSource: -config and -configdir are mutually exclusive, but
// the default -config value alone (never explicitly passed) does not collide
// with -configdir.
func TestCheckConfigSource(t *testing.T) {
	if err := checkConfigSource(map[string]bool{"configdir": true}); err != nil {
		t.Fatalf("-configdir alone must be legal: %v", err)
	}
	if err := checkConfigSource(map[string]bool{"config": true}); err != nil {
		t.Fatalf("-config alone must be legal: %v", err)
	}
	if err := checkConfigSource(map[string]bool{}); err != nil {
		t.Fatalf("no config flags must be legal: %v", err)
	}
	if err := checkConfigSource(map[string]bool{"config": true, "configdir": true}); err == nil {
		t.Fatal("-config and -configdir together must fail")
	}
}
