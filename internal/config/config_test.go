package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestToolOverrideSpecFieldsParse: the tools.policy.overrides schema carries
// description and per-param description overrides alongside the policy fields,
// decoded under KnownFields strictness.
func TestToolOverrideSpecFieldsParse(t *testing.T) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(`
version: "1"
agents:
  bot: { loop: builtin:per_chat }
tools:
  policy:
    overrides:
      "builtin:web_search":
        description: "Search the deployment's runbooks."
        params:
          query: { description: "What to look up" }
        needs_confirm: true
        autonomous: false
`))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		t.Fatal(err)
	}
	o, ok := c.Tools.Policy.Overrides["builtin:web_search"]
	if !ok {
		t.Fatal("override not decoded")
	}
	if o.Description == nil || *o.Description != "Search the deployment's runbooks." {
		t.Fatalf("description override not decoded: %+v", o.Description)
	}
	if o.Params["query"].Description != "What to look up" {
		t.Fatalf("param override not decoded: %+v", o.Params)
	}
	if o.NeedsConfirm == nil || !*o.NeedsConfirm {
		t.Fatalf("needs_confirm not decoded: %+v", o.NeedsConfirm)
	}
	if o.Autonomous == nil || *o.Autonomous {
		t.Fatalf("explicit autonomous: false must decode as a non-nil false: %+v", o.Autonomous)
	}
}

// TestToolOverrideUnknownFieldFails: under KnownFields a misspelled override
// key is a boot error, not a silent no-op.
func TestToolOverrideUnknownFieldFails(t *testing.T) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(`
version: "1"
agents:
  bot: { loop: builtin:per_chat }
tools:
  policy:
    overrides:
      "builtin:web_search": { needs_confrm: true }
`))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err == nil {
		t.Fatal("misspelled override key should fail strict decode")
	}
}

// TestValidateWebhookTimeout exercises the webhook channel timeout rule:
// unset (driver default) and a valid duration pass; garbage and non-positive
// durations are boot errors.
func TestValidateWebhookTimeout(t *testing.T) {
	base := func() *Config {
		return &Config{
			Agents: map[string]Agent{"bot": {Loop: "builtin:per_chat"}},
			Gateway: Gateway{
				Channels: []Channel{{Name: "wh", Type: "webhook", Agent: "bot"}},
			},
		}
	}
	c := base()
	if err := validate("cfg.yaml", c); err != nil {
		t.Fatalf("unset timeout should validate: %v", err)
	}
	c = base()
	c.Gateway.Channels[0].Timeout = "120s"
	if err := validate("cfg.yaml", c); err != nil {
		t.Fatalf("120s should validate: %v", err)
	}
	c = base()
	c.Gateway.Channels[0].Timeout = "soon"
	if err := validate("cfg.yaml", c); err == nil || !strings.Contains(err.Error(), "invalid timeout") {
		t.Fatalf("garbage timeout should fail, got %v", err)
	}
	c = base()
	c.Gateway.Channels[0].Timeout = "-5s"
	if err := validate("cfg.yaml", c); err == nil || !strings.Contains(err.Error(), "invalid timeout") {
		t.Fatalf("negative timeout should fail, got %v", err)
	}
}

// TestValidateShellProfile exercises the provider-specific validation rules.
func TestValidateShellProfile(t *testing.T) {
	tests := []struct {
		name    string
		profile ShellProfile
		wantErr string
	}{
		{
			name:    "ssh missing host",
			profile: ShellProfile{Provider: "ssh", User: "root"},
			wantErr: "missing required host",
		},
		{
			name:    "ssh valid",
			profile: ShellProfile{Provider: "ssh", Host: "1.2.3.4:22", User: "root"},
		},
		{
			name:    "docker default (no provider) ok",
			profile: ShellProfile{},
		},
		{
			name:    "docker explicit ok",
			profile: ShellProfile{Provider: "docker", Image: "alpine:3.20"},
		},
		{
			name:    "unknown provider",
			profile: ShellProfile{Provider: "mesos"},
			wantErr: "unknown provider",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateShellProfile("cfg.yaml", "test-owner", "p", tt.profile)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
		})
	}
}

// TestCredentialsMasterKeyEnvDefault verifies the env-var name resolution.
func TestCredentialsMasterKeyEnvDefault(t *testing.T) {
	c := &Config{}
	if got := c.CredentialsMasterKeyEnv(); got != "CREDENTIALS_MASTER_KEY" {
		t.Fatalf("default env = %q, want CREDENTIALS_MASTER_KEY", got)
	}
	c2 := &Config{}
	c2.Runtime.Credentials.MasterKeyEnv = "MY_KEY_ENV"
	if got := c2.CredentialsMasterKeyEnv(); got != "MY_KEY_ENV" {
		t.Fatalf("override env = %q, want MY_KEY_ENV", got)
	}
}

// TestValidateSearch exercises the search-engine validation rules: known
// engines and default resolution. A missing api_key is NOT a boot error —
// engines with unresolvable credentials are skipped with a warning at
// construction (drivers/search.Build), keeping degradation honest and
// uniform across both config paths.
func TestValidateSearch(t *testing.T) {
	tests := []struct {
		name    string
		search  Search
		wantErr string
		wantDef string
	}{
		{name: "none configured ok", search: Search{}},
		{
			name:   "doubao without key boots (skipped at build)",
			search: Search{Engines: map[string]SearchEngine{"doubao": {}}},
		},
		{
			name:   "ollama without key boots (skipped at build)",
			search: Search{Engines: map[string]SearchEngine{"ollama": {}}},
		},
		{
			name:   "youtube without key boots (skipped at build)",
			search: Search{Engines: map[string]SearchEngine{"youtube": {}}},
		},
		{
			name:    "youtube with key defaults to itself",
			search:  Search{Engines: map[string]SearchEngine{"youtube": {APIKey: "k"}}},
			wantDef: "youtube",
		},
		{
			name:    "unknown engine",
			search:  Search{Engines: map[string]SearchEngine{"brave": {APIKey: "k"}}},
			wantErr: "unsupported search engine",
		},
		{
			name:    "multiple engines without default",
			search:  Search{Engines: map[string]SearchEngine{"doubao": {APIKey: "k"}, "ollama": {APIKey: "k2"}}},
			wantErr: "set search.default",
		},
		{
			name:    "default names unconfigured engine",
			search:  Search{Default: "ollama", Engines: map[string]SearchEngine{"doubao": {APIKey: "k"}}},
			wantErr: "not a configured engine",
		},
		{
			name:    "single engine defaults to itself",
			search:  Search{Engines: map[string]SearchEngine{"ollama": {APIKey: "k"}}},
			wantDef: "ollama",
		},
		{
			name:    "explicit default ok",
			search:  Search{Default: "ollama", Engines: map[string]SearchEngine{"doubao": {APIKey: "k"}, "ollama": {APIKey: "k2"}}},
			wantDef: "ollama",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{Search: tt.search, Agents: map[string]Agent{"bot": {Loop: "./loop.lua"}}}
			err := validate("cfg.yaml", c)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			if tt.wantDef != "" && c.Search.Default != tt.wantDef {
				t.Fatalf("default = %q, want %q", c.Search.Default, tt.wantDef)
			}
		})
	}
}

// TestValidateMediaAudit exercises the media backend and audit journal rules.
func TestValidateMediaAudit(t *testing.T) {
	zero := 0
	neg := -1
	tests := []struct {
		name    string
		media   MediaConfig
		audit   AuditConfig
		wantErr string
	}{
		{name: "defaults ok", media: MediaConfig{}, audit: AuditConfig{}},
		{name: "fs explicit ok", media: MediaConfig{Backend: "fs", Dir: "./data/media"}},
		{
			name:    "s3 missing fields",
			media:   MediaConfig{Backend: "s3"},
			wantErr: "requires s3.bucket and s3.region",
		},
		{
			name:  "s3 without keys boots (store skipped at boot)",
			media: MediaConfig{Backend: "s3", S3: MediaS3{Bucket: "b", Region: "r"}},
		},
		{
			name:  "s3 complete ok",
			media: MediaConfig{Backend: "s3", S3: MediaS3{Bucket: "b", Region: "r", AccessKey: "k", SecretKey: "s"}},
		},
		{
			name:    "unknown backend",
			media:   MediaConfig{Backend: "gcs"},
			wantErr: "unsupported media backend",
		},
		{
			name:    "negative retention",
			audit:   AuditConfig{RetentionDays: &neg},
			wantErr: "retention_days must be >= 0",
		},
		{name: "zero retention = forever", audit: AuditConfig{RetentionDays: &zero}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{Media: tt.media, Audit: tt.audit, Agents: map[string]Agent{"bot": {Loop: "./loop.lua"}}}
			err := validate("cfg.yaml", c)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
	}
	// Defaults: enabled, 90-day retention.
	d := AuditConfig{}
	if !d.AuditEnabled() || d.AuditRetention() != 90 {
		t.Fatalf("audit defaults: enabled=%v retention=%d", d.AuditEnabled(), d.AuditRetention())
	}
}

// TestValidateLegalSearch exercises the legal-search engine rules: only hklii
// is supported (no key), and default resolution.
func TestValidateLegalSearch(t *testing.T) {
	tests := []struct {
		name    string
		legal   LegalSearch
		wantErr string
		wantDef string
	}{
		{name: "none configured ok", legal: LegalSearch{}},
		{
			name:    "unknown engine",
			legal:   LegalSearch{Engines: map[string]SearchEngine{"westlaw": {}}},
			wantErr: "unsupported legal_search engine",
		},
		{
			name:    "hklii needs no key, defaults to itself",
			legal:   LegalSearch{Engines: map[string]SearchEngine{"hklii": {}}},
			wantDef: "hklii",
		},
		{
			name:    "default names unconfigured engine",
			legal:   LegalSearch{Default: "lexis", Engines: map[string]SearchEngine{"hklii": {}}},
			wantErr: "not a configured engine",
		},
		{
			name:    "explicit default ok",
			legal:   LegalSearch{Default: "hklii", Engines: map[string]SearchEngine{"hklii": {}}},
			wantDef: "hklii",
		},
		{
			name:    "npc supported, no key",
			legal:   LegalSearch{Engines: map[string]SearchEngine{"npc": {}}},
			wantDef: "npc",
		},
		{
			name:    "two engines need an explicit default",
			legal:   LegalSearch{Engines: map[string]SearchEngine{"hklii": {}, "npc": {}}},
			wantErr: "set legal_search.default",
		},
		{
			name:    "hklii + npc with default",
			legal:   LegalSearch{Default: "npc", Engines: map[string]SearchEngine{"hklii": {}, "npc": {}}},
			wantDef: "npc",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{Legal: tt.legal, Agents: map[string]Agent{"bot": {Loop: "./loop.lua"}}}
			err := validate("cfg.yaml", c)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got: %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			if tt.wantDef != "" && c.Legal.Default != tt.wantDef {
				t.Fatalf("default = %q, want %q", c.Legal.Default, tt.wantDef)
			}
		})
	}
}
