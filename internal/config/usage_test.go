package config

import (
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
