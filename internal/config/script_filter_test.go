package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func baseScriptFilterTestConfig() *Config {
	return &Config{
		Version: "1",
		Models:  map[string]Model{"m": {Provider: "openai", Model: "gpt-4o-mini"}},
		Safety: SafetyConfig{
			ScriptFilters: map[string]ScriptFilterProfile{
				"pii_mask": {
					Source:     `function egress(input: { text: string }) { return { text: input.text }; }`,
					Directions: []string{"egress"},
					Sources:    []string{"channel"},
					Timeout:    "150ms",
					OnError:    "pass",
				},
			},
		},
		Profiles: Profiles{
			Safety: map[string]SafetyProfile{
				"strict": {Filters: []string{"source-attribution", "script:pii_mask"}},
			},
		},
	}
}

func TestValidateScriptFiltersOKAndFileSource(t *testing.T) {
	c := baseScriptFilterTestConfig()
	if err := validateScriptFilters("test", c); err != nil {
		t.Fatalf("valid inline filter failed: %v", err)
	}
	if err := validateSafetyProfiles("test", c); err != nil {
		t.Fatalf("valid profile referencing script filter failed: %v", err)
	}

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "filters", "guard.ts")
	if err := os.MkdirAll(filepath.Dir(scriptPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, []byte(`export function ingress(input: { message: string }) { return { text: input.message }; }`), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Safety.ScriptFilters["file_guard"] = ScriptFilterProfile{File: "./filters/guard.ts"}
	if err := validateScriptFilters(filepath.Join(dir, "agentflow.yaml"), c); err != nil {
		t.Fatalf("valid file filter failed: %v", err)
	}
	if got := c.Safety.ScriptFilters["file_guard"].EffectiveSource(); !strings.Contains(got, "function ingress") {
		t.Fatalf("expected resolved file source, got %q", got)
	}
}

func TestValidateScriptFiltersErrors(t *testing.T) {
	c := baseScriptFilterTestConfig()
	c.Safety.ScriptFilters["broken"] = ScriptFilterProfile{Source: "function ingress(input) { return"}
	if err := validateScriptFilters("test", c); err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("expected syntax error, got %v", err)
	}

	c = baseScriptFilterTestConfig()
	c.Safety.ScriptFilters["both"] = ScriptFilterProfile{Source: "function ingress(i) {}", File: "./x.ts"}
	if err := validateScriptFilters("test", c); err == nil || !strings.Contains(err.Error(), "cannot set both source and file") {
		t.Fatalf("expected both source and file error, got %v", err)
	}

	c = baseScriptFilterTestConfig()
	c.Safety.ScriptFilters["pii_mask"] = ScriptFilterProfile{
		Source:  `function ingress(_i) {}`,
		Timeout: "30s",
	}
	if err := validateScriptFilters("test", c); err == nil || !strings.Contains(err.Error(), "at most 10s") {
		t.Fatalf("expected timeout cap error, got %v", err)
	}

	c = baseScriptFilterTestConfig()
	c.Profiles.Safety["strict"] = SafetyProfile{Filters: []string{"script:missing"}}
	if err := validateSafetyProfiles("test", c); err == nil || !strings.Contains(err.Error(), "unknown filter") {
		t.Fatalf("expected missing script filter error, got %v", err)
	}
}
