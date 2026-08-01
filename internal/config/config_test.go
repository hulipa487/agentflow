package config

import "testing"

// TestValidateShellProfile exercises the provider-specific validation rules.
func TestValidateShellProfile(t *testing.T) {
	tests := []struct {
		name    string
		profile ShellProfile
		wantErr string
	}{
		{
			name:    "vultr missing region",
			profile: ShellProfile{Provider: "vultr", Plan: "vc2-1c-1gb"},
			wantErr: "missing required region/plan",
		},
		{
			name:    "vultr missing plan",
			profile: ShellProfile{Provider: "vultr", Region: "ewr"},
			wantErr: "missing required region/plan",
		},
		{
			name:    "vultr valid",
			profile: ShellProfile{Provider: "vultr", Region: "ewr", Plan: "vc2-1c-1gb", OsID: 1743},
		},
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
