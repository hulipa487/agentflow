package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func baseLuaFilterTestConfig() *Config {
	return &Config{
		Version: "1",
		Models:  map[string]Model{"m": {Provider: "openai", Model: "gpt-4o-mini"}},
		Safety: SafetyConfig{
			LuaFilters: map[string]LuaFilterProfile{
				"pii_mask": {
					Source:     `function egress(input) return { text = input.text } end`,
					Directions: []string{"egress"},
					Sources:    []string{"channel"},
					Timeout:    "150ms",
					OnError:    "pass",
				},
			},
		},
		Profiles: Profiles{
			Safety: map[string]SafetyProfile{
				"strict": {Filters: []string{"source-attribution", "lua:pii_mask"}},
			},
		},
	}
}

func TestValidateLuaFiltersOKAndFileSource(t *testing.T) {
	c := baseLuaFilterTestConfig()
	if err := validateLuaFilters("test", c); err != nil {
		t.Fatalf("valid inline filter failed: %v", err)
	}
	if err := validateSafetyProfiles("test", c); err != nil {
		t.Fatalf("valid profile referencing lua filter failed: %v", err)
	}

	dir := t.TempDir()
	luaPath := filepath.Join(dir, "filters", "guard.lua")
	if err := os.MkdirAll(filepath.Dir(luaPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(luaPath, []byte(`function ingress(input) return { text = input.message } end`), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Safety.LuaFilters["file_guard"] = LuaFilterProfile{File: "./filters/guard.lua"}
	if err := validateLuaFilters(filepath.Join(dir, "agentflow.yaml"), c); err != nil {
		t.Fatalf("valid file filter failed: %v", err)
	}
	if got := c.Safety.LuaFilters["file_guard"].EffectiveSource(); !strings.Contains(got, "function ingress") {
		t.Fatalf("expected resolved file source, got %q", got)
	}
}

func TestValidateLuaFiltersErrors(t *testing.T) {
	c := baseLuaFilterTestConfig()
	c.Safety.LuaFilters["broken"] = LuaFilterProfile{Source: "function ingress(input) return"}
	if err := validateLuaFilters("test", c); err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("expected syntax error, got %v", err)
	}

	c = baseLuaFilterTestConfig()
	c.Safety.LuaFilters["both"] = LuaFilterProfile{Source: "function ingress(i) end", File: "./x.lua"}
	if err := validateLuaFilters("test", c); err == nil || !strings.Contains(err.Error(), "cannot set both source and file") {
		t.Fatalf("expected both source and file error, got %v", err)
	}

	c = baseLuaFilterTestConfig()
	c.Safety.LuaFilters["pii_mask"] = LuaFilterProfile{
		Source:  `function ingress(i) end`,
		Timeout: "30s",
	}
	if err := validateLuaFilters("test", c); err == nil || !strings.Contains(err.Error(), "at most 10s") {
		t.Fatalf("expected timeout cap error, got %v", err)
	}

	c = baseLuaFilterTestConfig()
	c.Profiles.Safety["strict"] = SafetyProfile{Filters: []string{"lua:missing"}}
	if err := validateSafetyProfiles("test", c); err == nil || !strings.Contains(err.Error(), "unknown filter") {
		t.Fatalf("expected missing lua filter error, got %v", err)
	}
}
