package studio

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/config"
)

func safetyLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func safetyFixture(t *testing.T) (*Safety, string, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "system.yaml"), `
version: "1"
models:
  default: { provider: openai, model: gpt-4o-mini, base_url: http://127.0.0.1:11434/v1 }
safety:
  classifiers:
    moderation:
      model: default
      categories: { safe: allow, harmful: block }
profiles:
  safety:
    base:
      filters: [source-attribution, classifier:moderation]
`)
	writeFile(t, filepath.Join(dir, "profiles", "base.yaml"), "name: base\nloop: ./base.lua\nsafety: base\n")
	cfg, err := config.LoadDir(dir, safetyLog())
	if err != nil {
		t.Fatal(err)
	}
	return NewSafety(cfg, dir, "", safetyLog()), dir, cfg
}

func TestSafetyProfileEditing(t *testing.T) {
	s, dir, _ := safetyFixture(t)

	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if !list.Editable || list.Mode != "configdir" || len(list.Profiles) != 1 || len(list.Classifiers) != 1 {
		t.Fatalf("initial list = %+v", list)
	}
	if !list.Profiles[0].Live || !list.Classifiers[0].Live {
		t.Fatalf("boot-time entries should be live: %+v", list)
	}
	if len(list.Profiles[0].Agents) != 1 || list.Profiles[0].Agents[0] != "base" {
		t.Errorf("profile usage = %+v", list.Profiles[0])
	}
	if len(list.Models) != 1 || list.Models[0] != "default" {
		t.Errorf("eligible models = %v", list.Models)
	}

	list, err = s.SaveProfile(SafetyProfileDraft{
		Name:    "strict",
		Filters: []string{"source-attribution", "classifier:moderation", "affect-guard"},
	}, true, list.MTime)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Profiles) != 2 {
		t.Fatalf("profiles after save = %+v", list.Profiles)
	}
	var strict *SafetyProfileView
	for i := range list.Profiles {
		if list.Profiles[i].Name == "strict" {
			strict = &list.Profiles[i]
		}
	}
	if strict == nil || strict.Live {
		t.Fatalf("new profile = %+v, want present and not live", strict)
	}
	if len(strict.Classifiers) != 1 || strict.Classifiers[0] != "moderation" {
		t.Errorf("classifier refs = %v", strict.Classifiers)
	}
	if _, err := config.LoadDir(dir, safetyLog()); err != nil {
		t.Fatalf("configdir does not load after save: %v", err)
	}

	if errs := s.ValidateProfile(SafetyProfileDraft{Name: "bad name"}, true); len(errs) == 0 {
		t.Error("invalid profile name passed validation")
	}
	if errs := s.ValidateProfile(SafetyProfileDraft{
		Name: "x", Filters: []string{"not-a-filter"},
	}, true); len(errs) == 0 || !strings.Contains(errs[0], "unknown filter") {
		t.Errorf("unknown filter errors = %v", errs)
	}
	if _, err := s.SaveProfile(SafetyProfileDraft{Name: "strict"}, true, 0); err == nil {
		t.Error("duplicate profile name was accepted")
	}
	if _, err := s.SaveProfile(SafetyProfileDraft{Name: "strict"}, false, 1); err != ErrSafetyConflict {
		t.Errorf("stale mtime error = %v, want ErrSafetyConflict", err)
	}

	// base is referenced by profiles/base.yaml, so removing it must be refused.
	if _, err := s.DeleteProfile("base", 0); err == nil || !strings.Contains(err.Error(), "references unknown safety profile") {
		t.Fatalf("delete in-use profile error = %v", err)
	}
}

func TestSafetyClassifierEditing(t *testing.T) {
	s, _, _ := safetyFixture(t)
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}

	threshold := 0.8
	list, err = s.SaveClassifier(ClassifierDraft{
		Name:       "toxicity",
		Model:      "default",
		Mode:       "enforce",
		Directions: []string{"ingress"},
		Sources:    []string{"channel"},
		Threshold:  &threshold,
		Categories: map[string]string{"safe": "allow", "toxicity": "block"},
		Timeout:    "2s",
		OnError:    "block",
		Cache:      ClassifierCache{Size: 100, TTL: "1m"},
		Budget:     ClassifierBudget{TokensPerDay: 1000},
	}, true, list.MTime)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Classifiers) != 2 {
		t.Fatalf("classifiers after save = %+v", list.Classifiers)
	}

	if _, err := s.SaveClassifier(ClassifierDraft{
		Name: "broken", Model: "missing", Categories: map[string]string{"safe": "allow"},
	}, true, list.MTime); err == nil || !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("unknown model error = %v", err)
	}
	if _, err := s.SaveClassifier(ClassifierDraft{
		Name: "noallow", Model: "default", Categories: map[string]string{"bad": "block"},
	}, true, list.MTime); err == nil || !strings.Contains(err.Error(), "allow label") {
		t.Fatalf("missing allow label error = %v", err)
	}

	// The base profile references classifier:moderation; deleting it must be
	// refused until that reference is gone.
	if _, err := s.DeleteClassifier("moderation", 0); err == nil || !strings.Contains(err.Error(), "unknown filter") {
		t.Fatalf("delete in-use classifier error = %v", err)
	}
	if _, err := s.DeleteClassifier("toxicity", 0); err != nil {
		t.Fatalf("delete unused classifier: %v", err)
	}
}

func TestSafetySingleFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agentflow.yaml")
	writeFile(t, path, `
# keep this comment
version: "1"
models:
  default: { provider: openai, model: gpt-4o-mini, base_url: http://127.0.0.1:11434/v1 }
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSafety(cfg, "", path, safetyLog())
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if list.Mode != "file" || !list.Editable || list.Path != path {
		t.Fatalf("list source = %+v", list)
	}
	list, err = s.SaveProfile(SafetyProfileDraft{
		Name: "strict", Filters: []string{"source-attribution", "affect-guard"},
	}, true, list.MTime)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Profiles) != 1 || list.Profiles[0].Name != "strict" {
		t.Fatalf("profiles = %+v", list.Profiles)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "# keep this comment") {
		t.Errorf("comment was lost:\n%s", raw)
	}
	if _, err := config.Load(path); err != nil {
		t.Fatalf("single-file config does not load after save: %v", err)
	}
}
