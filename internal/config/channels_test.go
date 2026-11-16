package config

import (
	"strings"
	"testing"
)

func openAIConfig() *Config {
	return &Config{
		Agents:  map[string]Agent{"coder": {}},
		Gateway: Gateway{Listen: "127.0.0.1:8080"},
		Models: map[string]Model{
			"default": {Provider: "openai", Model: "m", BaseURL: "https://api.example.com/v1"},
		},
	}
}

func TestValidateOpenAIChannel(t *testing.T) {
	base := func() Channel {
		return Channel{
			Name: "api", Type: "openai", Path: "/v1/",
			Agents: []string{"coder"}, Auth: "none",
		}
	}
	cases := []struct {
		name    string
		mutate  func(*Config, *Channel)
		wantErr string
	}{
		{
			name:   "minimal valid",
			mutate: func(*Config, *Channel) {},
		},
		{
			name:    "path must end with slash",
			mutate:  func(_ *Config, ch *Channel) { ch.Path = "/v1" },
			wantErr: "path must end with /",
		},
		{
			name:    "agents required",
			mutate:  func(_ *Config, ch *Channel) { ch.Agents = nil },
			wantErr: "has no agents",
		},
		{
			name:    "agent and agents together",
			mutate:  func(_ *Config, ch *Channel) { ch.Agent = "coder" },
			wantErr: "sets both agent and agents",
		},
		{
			name:    "unknown agent",
			mutate:  func(_ *Config, ch *Channel) { ch.Agents = []string{"ghost"} },
			wantErr: "unknown agent",
		},
		{
			name:    "user_token needs identity",
			mutate:  func(_ *Config, ch *Channel) { ch.Auth = "user_token" },
			wantErr: "runtime.identity.enabled is false",
		},
		{
			name: "user_token with identity",
			mutate: func(c *Config, ch *Channel) {
				ch.Auth = "user_token"
				c.Runtime.Identity.Enabled = true
			},
		},
		{
			name:    "none refused off loopback",
			mutate:  func(c *Config, ch *Channel) { c.Gateway.Listen = "0.0.0.0:8080" },
			wantErr: "not a loopback address",
		},
		{
			name: "none allowed on localhost",
			mutate: func(c *Config, ch *Channel) {
				c.Gateway.Listen = "localhost:8080"
			},
		},
		{
			name:    "key needs keys",
			mutate:  func(_ *Config, ch *Channel) { ch.Auth = "key" },
			wantErr: "has no keys",
		},
		{
			name: "key with a key",
			mutate: func(_ *Config, ch *Channel) {
				ch.Auth = "key"
				ch.Keys = []string{"${COMPAT_API_KEY}"}
			},
		},
		{
			name:    "auth required",
			mutate:  func(_ *Config, ch *Channel) { ch.Auth = "" },
			wantErr: "needs an auth mode",
		},
		{
			name:    "unknown auth",
			mutate:  func(_ *Config, ch *Channel) { ch.Auth = "magic" },
			wantErr: "unsupported auth",
		},
		{
			name: "self-referencing model refused",
			mutate: func(c *Config, _ *Channel) {
				c.Models["default"] = Model{Provider: "openai", BaseURL: "http://127.0.0.1:8080/v1"}
			},
			wantErr: "reaches this instance's own listener",
		},
		{
			name: "localhost alias of the same listener refused",
			mutate: func(c *Config, _ *Channel) {
				c.Models["default"] = Model{Provider: "openai", BaseURL: "http://localhost:8080/v1/"}
			},
			wantErr: "reaches this instance's own listener",
		},
		{
			name: "same listener different path is fine",
			mutate: func(c *Config, _ *Channel) {
				c.Models["default"] = Model{Provider: "openai", BaseURL: "http://127.0.0.1:8080/other/"}
			},
		},
		{
			name: "different port is fine",
			mutate: func(c *Config, _ *Channel) {
				c.Models["default"] = Model{Provider: "openai", BaseURL: "http://127.0.0.1:9999/v1"}
			},
		},
		{
			name:    "journal must be a mode",
			mutate:  func(_ *Config, ch *Channel) { ch.Journal = "verbose" },
			wantErr: "is not a mode",
		},
		{
			name:   "journal metadata accepted",
			mutate: func(_ *Config, ch *Channel) { ch.Journal = "metadata" },
		},
		{
			name:    "reserved engine subtree",
			mutate:  func(_ *Config, ch *Channel) { ch.Path = "/v1/users/" },
			wantErr: "is reserved",
		},
		{
			name: "v1 prefix belongs to the openai type",
			mutate: func(c *Config, ch *Channel) {
				// Replace the openai channel with a webhook on the same path.
				*ch = Channel{Name: "hook", Type: "webhook", Agent: "coder", Path: "/v1/"}
			},
			wantErr: "reserved for the openai channel type",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := openAIConfig()
			ch := base()
			tc.mutate(c, &ch)
			err := ValidateChannels(c, []Channel{ch})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v; want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestOpenAIChannelCollidesWithAnotherChannelsSubtree: the openai driver mounts
// a catch-all under its path, so a webhook mounted over the same subtree would
// be shadowed. The validator names both channels instead.
func TestOpenAIChannelCollidesWithAnotherChannelsSubtree(t *testing.T) {
	c := openAIConfig()
	openai := Channel{Name: "api", Type: "openai", Path: "/v1/", Agents: []string{"coder"}, Auth: "none"}
	hook := Channel{Name: "hook", Type: "webhook", Agent: "coder", Path: "/v1/hook/"}
	err := ValidateChannels(c, []Channel{openai, hook})
	if err == nil || !strings.Contains(err.Error(), "overlapping routes") {
		t.Fatalf("error = %v; want an overlapping-routes refusal", err)
	}
}

// TestJournalModeDefaults: full everywhere except the openai channel, whose
// coding context defaults to metadata.
func TestJournalModeDefaults(t *testing.T) {
	if got := (Channel{Type: "webhook"}).JournalMode(); got != "full" {
		t.Fatalf("webhook journal = %q; want full", got)
	}
	if got := (Channel{Type: "openai"}).JournalMode(); got != "metadata" {
		t.Fatalf("openai journal = %q; want metadata", got)
	}
	if got := (Channel{Type: "openai", Journal: "off"}).JournalMode(); got != "off" {
		t.Fatalf("explicit journal = %q; want off", got)
	}
}

// TestEphemeralCapDefault: unset is 16, an explicit 0 disables the bound.
func TestEphemeralCapDefault(t *testing.T) {
	var r Runtime
	if got := r.EphemeralCap(); got != 16 {
		t.Fatalf("unset = %d; want 16", got)
	}
	zero := 0
	r.Limits.EphemeralPerTenant = &zero
	if got := r.EphemeralCap(); got != 0 {
		t.Fatalf("explicit zero = %d; want 0", got)
	}
}
