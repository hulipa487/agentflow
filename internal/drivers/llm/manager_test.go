package llm

import (
	"io"
	"log/slog"
	"testing"

	"agentflow/internal/config"
)

func TestManagerHotSwap(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "gpt-4o-mini"},
	}, log)

	if _, err := m.resolve("default"); err != nil {
		t.Fatalf("boot model missing: %v", err)
	}
	if _, err := m.resolve("nope"); err == nil {
		t.Fatal("expected unknown model error")
	}

	m.Upsert("fast", config.Model{Provider: "anthropic", Model: "claude-haiku"})
	if cfg, err := m.Get("fast"); err != nil || cfg.Provider != "anthropic" {
		t.Fatalf("upsert not visible: %v %v", cfg, err)
	}

	m.Upsert("default", config.Model{Provider: "openai", Model: "gpt-5"})
	if cfg, _ := m.Get("default"); cfg.Model != "gpt-5" {
		t.Fatalf("upsert did not replace: %+v", cfg)
	}

	m.Remove("fast")
	if _, err := m.resolve("fast"); err == nil {
		t.Fatal("remove did not take effect")
	}

	m.SetAll(map[string]config.Model{"only": {Provider: "gemini", Model: "gemini-3-flash"}})
	if _, err := m.resolve("default"); err == nil {
		t.Fatal("SetAll did not replace the set")
	}
	if _, err := m.resolve("only"); err != nil {
		t.Fatalf("SetAll lost new entry: %v", err)
	}

	// List returns a copy: mutating it must not affect the manager.
	l := m.List()
	l["evil"] = config.Model{}
	if _, err := m.resolve("evil"); err == nil {
		t.Fatal("List leaked the live map")
	}
}

// A config with no models: key yields a nil map at boot; runtime Upsert must
// still work (regression: assignment into nil map panicked).
func TestManagerNilBootModels(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewManager(nil, log)
	m.Upsert("default", config.Model{Provider: "openai", Model: "gpt-5"})
	if cfg, err := m.Get("default"); err != nil || cfg.Model != "gpt-5" {
		t.Fatalf("upsert into nil-boot manager failed: %v %v", cfg, err)
	}
	m.SetAll(nil) // revert against a config with no models either
	m.Upsert("x", config.Model{Provider: "gemini", Model: "g"})
	if _, err := m.Get("x"); err != nil {
		t.Fatalf("upsert after nil SetAll failed: %v", err)
	}
	if got := m.List(); got == nil {
		t.Fatal("List returned nil map")
	}
}
