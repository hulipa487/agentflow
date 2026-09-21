package config

import (
	"strings"
	"testing"
)

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
