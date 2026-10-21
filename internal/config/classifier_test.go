package config

import (
	"strings"
	"testing"
)

func classifierConfig() *Config {
	return &Config{
		Models: map[string]Model{
			"moderation": {Provider: "openai", Model: "gpt-4.1-mini"},
		},
		Safety: SafetyConfig{Classifiers: map[string]ClassifierProfile{
			"moderation": {
				Model:      "moderation",
				Categories: map[string]string{"safe": "allow", "violence": "block"},
			},
		}},
		Profiles: Profiles{
			Safety: map[string]SafetyProfile{
				"strict": {Filters: []string{"classifier:moderation"}},
			},
		},
	}
}

func TestValidateClassifiersOK(t *testing.T) {
	c := classifierConfig()
	if err := validateClassifiers("test", c); err != nil {
		t.Fatal(err)
	}
	if err := validateSafetyProfiles("test", c); err != nil {
		t.Fatal(err)
	}
}

func TestValidateClassifierRejectsUserKeyedModel(t *testing.T) {
	c := classifierConfig()
	c.Models["moderation"] = Model{Provider: "openai", Model: "gpt", KeySource: "user"}
	err := validateClassifiers("test", c)
	if err == nil || !strings.Contains(err.Error(), "key_source: user") {
		t.Fatalf("got %v; want a user-keyed model refusal", err)
	}
}

func TestValidateClassifierRejectsUnknownModel(t *testing.T) {
	c := classifierConfig()
	delete(c.Models, "moderation")
	if err := validateClassifiers("test", c); err == nil || !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("got %v", err)
	}
}

func TestValidateClassifierRequiresAllowLabel(t *testing.T) {
	c := classifierConfig()
	p := c.Safety.Classifiers["moderation"]
	p.Categories = map[string]string{"violence": "block", "harassment": "review"}
	c.Safety.Classifiers["moderation"] = p
	if err := validateClassifiers("test", c); err == nil || !strings.Contains(err.Error(), "allow label") {
		t.Fatalf("got %v", err)
	}
}

func TestValidateClassifierRejectsBadAction(t *testing.T) {
	c := classifierConfig()
	p := c.Safety.Classifiers["moderation"]
	p.Categories = map[string]string{"safe": "allow", "violence": "ignore"}
	c.Safety.Classifiers["moderation"] = p
	if err := validateClassifiers("test", c); err == nil || !strings.Contains(err.Error(), "not allow, review, or block") {
		t.Fatalf("got %v", err)
	}
}

func TestValidateSafetyProfileRejectsUnknownClassifier(t *testing.T) {
	c := classifierConfig()
	c.Profiles.Safety["strict"] = SafetyProfile{Filters: []string{"classifier:missing"}}
	if err := validateSafetyProfiles("test", c); err == nil || !strings.Contains(err.Error(), "unknown filter") {
		t.Fatalf("got %v", err)
	}
}

func TestValidateSpawnProfileSafetyReference(t *testing.T) {
	c := classifierConfig()
	c.Profiles.Agent = map[string]SpawnProfile{
		"child": {Safety: "strict"},
	}
	if err := validateSafetyProfiles("test", c); err != nil {
		t.Fatal(err)
	}
	c.Profiles.Agent["child"] = SpawnProfile{Safety: "missing"}
	if err := validateSafetyProfiles("test", c); err == nil || !strings.Contains(err.Error(), "spawn profile") {
		t.Fatalf("got %v", err)
	}
}
