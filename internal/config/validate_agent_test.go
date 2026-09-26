package config

import (
	"strings"
	"testing"
)

// ValidateAgent is the runtime agent registry's rule book — the same rules
// boot applies to agents:, held to an agent installed live. The refusals here
// are the ones that would otherwise reach the engine half-built.

func validAgent() Agent {
	return Agent{Loop: "plugin:per_chat"}
}

func validateTestConfig() *Config {
	return &Config{
		Agents: map[string]Agent{
			"existing": {Loop: "plugin:per_chat"},
		},
		Memory: Memory{
			Backends: map[string]Backend{
				"main_db": {},
			},
		},
		Profiles: Profiles{
			Memory: map[string]MemoryProfile{
				"conversational": {Stores: map[string]Store{
					"dialogue": {Backend: "main_db", Table: "dialogue"},
				}},
			},
		},
	}
}

func TestValidateAgentAcceptsAWellFormedAgent(t *testing.T) {
	c := validateTestConfig()
	if err := c.ValidateAgent("newcomer", validAgent(), "test"); err != nil {
		t.Fatalf("well-formed agent refused: %v", err)
	}
}

func TestValidateAgentHoldsCapabilitiesToTheCeiling(t *testing.T) {
	c := validateTestConfig()
	c.Plugins.AllowCapabilities = []string{"memory"}
	a := validAgent()
	a.Capabilities = []string{"shell.exec"}
	err := c.ValidateAgent("newcomer", a, "test")
	if err == nil || !strings.Contains(err.Error(), "not in plugins.allow_capabilities") {
		t.Fatalf("capability over the ceiling = %v", err)
	}

	// An agent that declares nothing falls to the default capability set —
	// which must still sit under the ceiling: a restricted ceiling rejects it.
	a2 := validAgent()
	err = c.ValidateAgent("newcomer", a2, "test")
	if err == nil || !strings.Contains(err.Error(), "not in plugins.allow_capabilities") {
		t.Fatalf("default set over a restricted ceiling = %v; want refusal", err)
	}

	// With no ceiling at all, the default set is allowed (the permissive
	// backward-compat rule), which the well-formed case already covers.
	if err := validateTestConfig().ValidateAgent("newcomer", validAgent(), "test"); err != nil {
		t.Fatalf("default set under an empty ceiling refused: %v", err)
	}
}

func TestValidateAgentRejectsBrokenReferences(t *testing.T) {
	c := validateTestConfig()

	a := validAgent()
	a.Shell = "no-such-profile"
	if err := c.ValidateAgent("newcomer", a, "test"); err == nil || !strings.Contains(err.Error(), "shell profile") {
		t.Fatalf("unknown shell profile = %v", err)
	}

	a = validAgent()
	a.Memory.Profile = "conversational"
	if err := c.ValidateAgent("newcomer", a, "test"); err != nil {
		t.Fatalf("conversational profile refused: %v", err)
	}

	a = validAgent()
	a.Memory.Profile = "no-such-profile"
	if err := c.ValidateAgent("newcomer", a, "test"); err == nil || !strings.Contains(err.Error(), "memory profile") {
		t.Fatalf("unknown memory profile = %v", err)
	}

	a = validAgent()
	a.CanContact = []string{"ghost"}
	if err := c.ValidateAgent("newcomer", a, "test"); err == nil || !strings.Contains(err.Error(), "can_contact") {
		t.Fatalf("unresolvable can_contact = %v", err)
	}

	// An existing agent is a resolvable target — the runtime ACL check is
	// what governs the contact, not this validation.
	a = validAgent()
	a.CanContact = []string{"existing"}
	if err := c.ValidateAgent("newcomer", a, "test"); err != nil {
		t.Fatalf("can_contact to a configured agent refused: %v", err)
	}
}
