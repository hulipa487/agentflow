package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The user API's frontend-facing settings: an origin list that must hold bare
// origins, and an identity provider that must name its issuer.
func TestValidateUsersCORSAndSession(t *testing.T) {
	tests := []struct {
		name    string
		users   UsersConfig
		wantErr string
	}{
		{name: "no origins is fine (CORS off)"},
		{name: "bare origins", users: UsersConfig{CORSOrigins: []string{"https://app.example.com", "http://localhost:3000"}}},
		{
			name:    "missing scheme",
			users:   UsersConfig{CORSOrigins: []string{"app.example.com"}},
			wantErr: "must start with http:// or https://",
		},
		{
			name:    "trailing slash",
			users:   UsersConfig{CORSOrigins: []string{"https://app.example.com/"}},
			wantErr: "bare origin",
		},
		{
			name:    "a path is not an origin",
			users:   UsersConfig{CORSOrigins: []string{"https://app.example.com/app"}},
			wantErr: "bare origin",
		},
		{name: "identity provider ok", users: UsersConfig{OIDC: &OIDCConfig{Issuer: "https://auth.example.com"}}},
		{
			name:    "identity provider without an issuer",
			users:   UsersConfig{OIDC: &OIDCConfig{Audience: "agentflow"}},
			wantErr: "oidc.issuer is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{Agents: map[string]Agent{"bot": {Loop: "./loop.lua"}}}
			c.Runtime.Users = tt.users
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

	// Defaults: just-in-time provisioning on, and no origin allowed.
	u := UsersConfig{}
	if !u.JITEnabled() {
		t.Error("verified first logins should provision a profile by default")
	}
	no := false
	if (UsersConfig{JITProvisioning: &no}).JITEnabled() {
		t.Error("jit_provisioning: false must be honoured")
	}
	if u.OriginAllowed("https://anything.example") {
		t.Error("no origin may be allowed by default")
	}
	if !(UsersConfig{CORSOrigins: []string{"https://app.example.com"}}).OriginAllowed("https://app.example.com") {
		t.Error("a listed origin must be allowed")
	}
	// An empty Origin header is never allowed, listed origins or not.
	if (UsersConfig{CORSOrigins: []string{"https://app.example.com"}}).OriginAllowed("") {
		t.Error("a request with no Origin must not be treated as allowed")
	}
}

// The token ledger's retention follows the audit rules: non-negative, with 0
// meaning keep forever, and the event log off unless asked for.
func TestValidateUsage(t *testing.T) {
	neg, zero, ten := -1, 0, 10
	tests := []struct {
		name    string
		usage   UsageConfig
		wantErr string
	}{
		{name: "defaults ok"},
		{name: "negative retention", usage: UsageConfig{RetentionDays: &neg}, wantErr: "usage.retention_days"},
		{name: "keep forever", usage: UsageConfig{RetentionDays: &zero}},
		{name: "explicit window", usage: UsageConfig{RetentionDays: &ten}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{Usage: tt.usage, Agents: map[string]Agent{"bot": {Loop: "./loop.lua"}}}
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

	// Defaults: detail log off, 30-day retention.
	u := UsageConfig{}
	if u.UsageEvents() {
		t.Error("the per-call detail log must default to off")
	}
	if u.UsageRetention() != 30 {
		t.Errorf("default retention = %d, want 30", u.UsageRetention())
	}
	on := true
	if !(UsageConfig{Events: &on}).UsageEvents() {
		t.Error("events should be on when asked for")
	}
	if got := (UsageConfig{RetentionDays: &zero}).UsageRetention(); got != 0 {
		t.Errorf("zero retention should read as zero (keep forever), got %d", got)
	}
}

// Local blobs sit beside a SQLite persistence file, and have nowhere to sit
// when persistence is a server DSN — the trap being that filepath.Dir of a DSN
// is a nonsense path rather than an error.
func TestDataDir(t *testing.T) {
	cases := map[string]string{
		"sqlite://./data/agentflow.db": "data",   // beside the database file
		"./var/af/runtime.db":          "var/af", // bare path, no scheme
		"postgres://u:p@h:5432/flow":   "./data", // no local directory to sit beside
		"postgresql://u@h/flow":        "./data",
		"":                             "data", // default persistence is sqlite://./data/agentflow.db
	}
	for persistence, want := range cases {
		c := &Config{}
		c.Runtime.Persistence = persistence
		got := filepath.ToSlash(c.DataDir())
		if !strings.HasSuffix(got, want) {
			t.Errorf("DataDir(%q) = %q, want a path ending in %q", persistence, got, want)
		}
	}
}

// The per-store targets follow the runtime store: one file per store beside a
// SQLite database, or the same server for a fleet — the reason a deployment
// that points persistence at Postgres gets shared identities without saying so
// twice. An explicit target always wins.
func TestStoreTargets(t *testing.T) {
	cases := []struct {
		name        string
		persistence string
		identity    string
		creds       string
		wantID      string
		wantCreds   string
	}{
		{
			name:      "defaults sit beside the default database",
			wantID:    "data/identity.db",
			wantCreds: "data/credentials.db",
		},
		{
			name:        "beside an explicit sqlite database",
			persistence: "sqlite://./var/af/runtime.db",
			wantID:      "var/af/identity.db",
			wantCreds:   "var/af/credentials.db",
		},
		{
			name:        "a fleet shares the server",
			persistence: "postgres://u:p@h:5432/flow",
			wantID:      "postgres://u:p@h:5432/flow",
			wantCreds:   "postgres://u:p@h:5432/flow",
		},
		{
			name:        "an explicit target wins",
			persistence: "postgres://u:p@h/flow",
			identity:    "./local/identity.db",
			creds:       "/srv/creds.db",
			wantID:      "./local/identity.db",
			wantCreds:   "/srv/creds.db",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{}
			c.Runtime.Persistence = tt.persistence
			c.Runtime.Identity.Persistence = tt.identity
			c.Runtime.Credentials.Path = tt.creds
			if got := filepath.ToSlash(c.IdentityStore()); !strings.HasSuffix(got, tt.wantID) {
				t.Errorf("IdentityStore() = %q, want a target ending in %q", got, tt.wantID)
			}
			if got := filepath.ToSlash(c.CredentialsStore()); !strings.HasSuffix(got, tt.wantCreds) {
				t.Errorf("CredentialsStore() = %q, want a target ending in %q", got, tt.wantCreds)
			}
		})
	}
}

// The epoch identifies a configuration across instances: stable for the same
// bytes, different the moment anything about the deployment changes.
func TestComputeEpoch(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.yaml")
	b := filepath.Join(dir, "b.yaml")
	write := func(p, s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(a, "version: '1'\n")
	write(b, "version: '1'\n")

	one := ComputeEpoch(a)
	if len(one) != 12 {
		t.Fatalf("epoch should be a short hex digest, got %q", one)
	}
	if ComputeEpoch(a) != one {
		t.Fatal("the same bytes must give the same epoch")
	}
	if ComputeEpoch(a) == ComputeEpoch(b) {
		t.Fatal("identical content under a different name is still a different deployment")
	}
	if ComputeEpoch(a, b) == ComputeEpoch(b, a) {
		t.Fatal("fragment order participates in the epoch")
	}
	if ComputeEpoch(filepath.Join(dir, "missing.yaml")) == "" {
		t.Fatal("an absent fragment contributes nothing but must not fail")
	}
	write(a, "version: '2'\n")
	if ComputeEpoch(a) == one {
		t.Fatal("a changed fragment must move the epoch")
	}
}

// A secret is a value, not a shape: rotating it must leave the epoch alone, or
// every instance would look divergent every time a key is rolled.
func TestEpochIgnoresRotatedSecrets(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cfg.yaml")
	if err := os.WriteFile(cfgPath, []byte(`version: "1"
models:
  m:
    provider: openai
    model: x
    api_key: ${EPOCH_TEST_KEY}
agents:
  bot:
    loop: ./loop.lua
`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EPOCH_TEST_KEY", "first")
	c1, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Setenv("EPOCH_TEST_KEY", "second")
	c2, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c1.Epoch != c2.Epoch {
		t.Fatalf("rotating a secret moved the epoch: %q vs %q", c1.Epoch, c2.Epoch)
	}
	if c1.Epoch != ComputeEpoch(cfgPath) {
		t.Fatal("Load must set the epoch from the file it read")
	}
	// The instances really did load different secrets — the epoch is stable
	// because it hashes what was written, not what was resolved.
	if c1.Models["m"].APIKey == c2.Models["m"].APIKey {
		t.Fatal("test premise: the two loads should have resolved different keys")
	}
}
