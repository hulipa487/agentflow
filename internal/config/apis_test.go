package config

import (
	"strings"
	"testing"
)

func TestValidateAPIs(t *testing.T) {
	c := &Config{
		Agents: map[string]Agent{"bot": {Loop: "plugin:per_chat"}},
		APIs: map[string]APIConfig{
			"billing": {
				Runtime: "script",
				Entry:   "./apis/billing/index.ts",
				Timeout: "5s",
				Capabilities: APICapabilities{
					HTTP: &APIHTTPCapability{AllowedHosts: []string{"api.stripe.com"}, Credentials: []string{"stripe"}},
				},
				Actions: map[string]APIActionConfig{
					"charge": {Description: "Charge customer", ExposeAsTool: true},
				},
			},
			"gh_pkg": {
				Dir: "./apis/gh",
			},
		},
	}
	if err := validate("test", c); err != nil {
		t.Fatalf("valid apis config refused: %v", err)
	}

	// Dir + inline runtime cannot be mixed.
	badMix := &Config{
		Agents: map[string]Agent{"bot": {Loop: "plugin:per_chat"}},
		APIs: map[string]APIConfig{
			"broken": {Dir: "./apis/x", Runtime: "wasm"},
		},
	}
	if err := validate("test", badMix); err == nil || !strings.Contains(err.Error(), "dir cannot be combined") {
		t.Fatalf("expected dir/inline conflict error, got %v", err)
	}

	// Inline entry without actions is rejected.
	badInline := &Config{
		Agents: map[string]Agent{"bot": {Loop: "plugin:per_chat"}},
		APIs: map[string]APIConfig{
			"broken": {Runtime: "wasm", Entry: "./x.wasm"},
		},
	}
	if err := validate("test", badInline); err == nil || !strings.Contains(err.Error(), "actions must declare at least one") {
		t.Fatalf("expected missing actions error, got %v", err)
	}

	// Unsupported runtime is rejected.
	badRT := &Config{
		Agents: map[string]Agent{"bot": {Loop: "plugin:per_chat"}},
		APIs: map[string]APIConfig{
			"broken": {Runtime: "python", Entry: "./x.py", Actions: map[string]APIActionConfig{"run": {}}},
		},
	}
	if err := validate("test", badRT); err == nil || !strings.Contains(err.Error(), "must be script or wasm") {
		t.Fatalf("expected runtime error, got %v", err)
	}
}
