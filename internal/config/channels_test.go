package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

// TestRegionalReplyWarnings: in a regional cell a channel that answers on the
// request it received can lose the answer when another instance owns the
// conversation. The config still loads; the operator is told.
func TestRegionalReplyWarnings(t *testing.T) {
	cfg := &Config{}
	cfg.Gateway.Channels = []Channel{
		{Name: "api", Type: "openai", Agents: []string{"coder"}, Auth: "key", Keys: []string{"k"}},
		{Name: "sync", Type: "webhook", Agent: "coder", Path: "/h/"},
		{Name: "async", Type: "webhook", Agent: "coder", Path: "/a/", Async: true, DeliverURL: "https://x.example/cb"},
		{Name: "tg", Type: "telegram", Agent: "coder"},
	}
	if w := cfg.RegionalReplyWarnings(); len(w) != 0 {
		t.Fatalf("a single topology has nothing to warn about: %v", w)
	}
	cfg.Runtime.Topology = "regional"
	w := cfg.RegionalReplyWarnings()
	if len(w) != 2 {
		t.Fatalf("warnings = %d (%v); want one for the openai channel and one for the synchronous webhook", len(w), w)
	}
	if !strings.Contains(w[0], `"api"`) || !strings.Contains(w[1], `"sync"`) {
		t.Fatalf("warnings name the wrong channels: %v", w)
	}
}

func TestValidateQQChannel(t *testing.T) {
	base := func() Channel {
		return Channel{Name: "qq", Type: "qq", Agent: "coder", AppID: "1024", Secret: "${QQ_BOT_SECRET}"}
	}
	cases := []struct {
		name    string
		mutate  func(*Channel)
		wantErr string
	}{
		{name: "websocket by default", mutate: func(*Channel) {}},
		{name: "webhook mode", mutate: func(ch *Channel) { ch.Mode = "webhook" }},
		{name: "scenes", mutate: func(ch *Channel) { ch.Scenes = []string{"c2c", "DM"} }},
		{name: "app id required", mutate: func(ch *Channel) { ch.AppID = " " }, wantErr: "has no app_id"},
		{name: "secret required", mutate: func(ch *Channel) { ch.Secret = "" }, wantErr: "has no secret"},
		{name: "unknown mode", mutate: func(ch *Channel) { ch.Mode = "polling" }, wantErr: "use websocket or webhook"},
		{name: "path only in webhook mode", mutate: func(ch *Channel) { ch.Path = "/webhook/qq/" }, wantErr: "only webhook mode uses"},
		{name: "webhook path slash", mutate: func(ch *Channel) { ch.Mode = "webhook"; ch.Path = "/qq" }, wantErr: "must end with /"},
		{name: "unknown scene", mutate: func(ch *Channel) { ch.Scenes = []string{"forum"} }, wantErr: "unknown scene"},
		{name: "api base", mutate: func(ch *Channel) { ch.APIBase = "ftp://x" }, wantErr: "api_base"},
		{name: "allow_users", mutate: func(ch *Channel) { ch.AllowUsers = []int64{1} }, wantErr: "only telegram uses"},
		{name: "markdown", mutate: func(ch *Channel) { ch.Markdown = true }},
		{name: "markdown off-type", mutate: func(ch *Channel) { ch.Type = "webhook"; ch.Markdown = true }, wantErr: "only qq, feishu and wecom channel types use"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := base()
			tc.mutate(&ch)
			err := ValidateChannels(openAIConfig(), []Channel{ch})
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

// Two QQ bots in webhook mode get their own default routes, so neither
// shadows the other.
func TestQQWebhookDefaultPathsPerChannel(t *testing.T) {
	a := Channel{Name: "qq-a", Type: "qq", Agent: "coder", AppID: "1", Secret: "s", Mode: "webhook"}
	b := Channel{Name: "qq-b", Type: "qq", Agent: "coder", AppID: "2", Secret: "s", Mode: "webhook"}
	if a.QQPath() != "/webhook/qq/qq-a/" {
		t.Fatalf("default path = %q", a.QQPath())
	}
	if err := ValidateChannels(openAIConfig(), []Channel{a, b}); err != nil {
		t.Fatalf("two qq webhooks collided: %v", err)
	}
	if (Channel{Type: "qq"}).QQPath() != "" {
		t.Fatal("websocket mode mounts no route")
	}
}

// The channel round-trips through the Studio's YAML writer with its fields.
func TestQQChannelMarshalsItsFields(t *testing.T) {
	ch := Channel{Name: "qq", Type: "qq", Agent: "coder", AppID: "1024", Secret: "${S}", Scenes: []string{"c2c"}, APIBase: "https://proxy.example.com", Markdown: true}
	out, err := yaml.Marshal(ch)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"app_id: \"1024\"", "scenes:", "- c2c", "api_base: https://proxy.example.com", "secret: ${S}", "markdown: true"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("yaml lacks %q:\n%s", want, out)
		}
	}
}

func TestValidateFeishuChannel(t *testing.T) {
	base := func() Channel {
		return Channel{Name: "feishu", Type: "feishu", Agent: "coder", AppID: "cli_123", Secret: "${FEISHU_SECRET}"}
	}
	cases := []struct {
		name    string
		mutate  func(*Channel)
		wantErr string
	}{
		{name: "websocket by default", mutate: func(*Channel) {}},
		{name: "webhook with token", mutate: func(ch *Channel) { ch.Mode = "webhook"; ch.Token = "${VERIF}" }},
		{name: "webhook with encrypt_key", mutate: func(ch *Channel) { ch.Mode = "webhook"; ch.EncryptKey = "${ENC}" }},
		{name: "scenes", mutate: func(ch *Channel) { ch.Scenes = []string{"p2p", "GROUP_ALL"} }},
		{name: "markdown", mutate: func(ch *Channel) { ch.Markdown = true }},
		{name: "app id required", mutate: func(ch *Channel) { ch.AppID = " " }, wantErr: "has no app_id"},
		{name: "secret required", mutate: func(ch *Channel) { ch.Secret = "" }, wantErr: "has no secret"},
		{name: "unknown mode", mutate: func(ch *Channel) { ch.Mode = "polling" }, wantErr: "use websocket or webhook"},
		{name: "path only in webhook mode", mutate: func(ch *Channel) { ch.Path = "/webhook/feishu/" }, wantErr: "only webhook mode uses"},
		{name: "webhook requires auth", mutate: func(ch *Channel) { ch.Mode = "webhook" }, wantErr: "requires token"},
		{name: "webhook path slash", mutate: func(ch *Channel) { ch.Mode = "webhook"; ch.Token = "t"; ch.Path = "/feishu" }, wantErr: "must end with /"},
		{name: "unknown scene", mutate: func(ch *Channel) { ch.Scenes = []string{"guild"} }, wantErr: "unknown scene"},
		{name: "api base", mutate: func(ch *Channel) { ch.APIBase = "ftp://x" }, wantErr: "api_base"},
		{name: "allow_users", mutate: func(ch *Channel) { ch.AllowUsers = []int64{1} }, wantErr: "only telegram uses"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := base()
			tc.mutate(&ch)
			err := ValidateChannels(openAIConfig(), []Channel{ch})
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
	a := Channel{Name: "fs-a", Type: "feishu", Agent: "coder", AppID: "1", Secret: "s", Mode: "webhook", Token: "t"}
	b := Channel{Name: "fs-b", Type: "feishu", Agent: "coder", AppID: "2", Secret: "s", Mode: "webhook", Token: "t"}
	if a.FeishuPath() != "/webhook/feishu/fs-a/" {
		t.Fatalf("default path = %q", a.FeishuPath())
	}
	if err := ValidateChannels(openAIConfig(), []Channel{a, b}); err != nil {
		t.Fatalf("two feishu webhooks collided: %v", err)
	}
	if (Channel{Type: "feishu"}).FeishuPath() != "" {
		t.Fatal("websocket mode mounts no route")
	}
}

func TestValidateWeComChannel(t *testing.T) {
	validAES := "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG" // 43 base64 chars -> 32 bytes
	base := func() Channel {
		return Channel{
			Name:       "wecom",
			Type:       "wecom",
			Agent:      "coder",
			CorpID:     "ww123456",
			AgentID:    1000002,
			Secret:     "${WECOM_SECRET}",
			Token:      "${WECOM_TOKEN}",
			EncryptKey: validAES,
		}
	}
	cases := []struct {
		name    string
		mutate  func(*Channel)
		wantErr string
	}{
		{name: "valid default webhook", mutate: func(*Channel) {}},
		{name: "valid env aes ref", mutate: func(ch *Channel) { ch.EncryptKey = "${WECOM_AES}" }},
		{name: "valid markdown", mutate: func(ch *Channel) { ch.Markdown = true }},
		{name: "corp_id required", mutate: func(ch *Channel) { ch.CorpID = "" }, wantErr: "has no corp_id"},
		{name: "agent_id required", mutate: func(ch *Channel) { ch.AgentID = 0 }, wantErr: "agent_id > 0"},
		{name: "secret required", mutate: func(ch *Channel) { ch.Secret = "" }, wantErr: "has no secret"},
		{name: "token required", mutate: func(ch *Channel) { ch.Token = "" }, wantErr: "has no token"},
		{name: "encrypt_key required", mutate: func(ch *Channel) { ch.EncryptKey = "" }, wantErr: "has no encrypt_key"},
		{name: "bad literal encrypt_key", mutate: func(ch *Channel) { ch.EncryptKey = "too-short" }, wantErr: "43-character base64"},
		{name: "unsupported mode", mutate: func(ch *Channel) { ch.Mode = "websocket" }, wantErr: "only webhook"},
		{name: "path slash", mutate: func(ch *Channel) { ch.Path = "/wecom" }, wantErr: "must end with /"},
		{name: "api base", mutate: func(ch *Channel) { ch.APIBase = "ftp://x" }, wantErr: "api_base"},
		{name: "scenes forbidden", mutate: func(ch *Channel) { ch.Scenes = []string{"group"} }, wantErr: "does not use"},
		{name: "allow_users forbidden", mutate: func(ch *Channel) { ch.AllowUsers = []int64{1} }, wantErr: "only telegram uses"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := base()
			tc.mutate(&ch)
			err := ValidateChannels(openAIConfig(), []Channel{ch})
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
	a := Channel{Name: "wc-a", Type: "wecom", Agent: "coder", CorpID: "ww1", AgentID: 1, Secret: "s", Token: "t", EncryptKey: "${K}"}
	b := Channel{Name: "wc-b", Type: "wecom", Agent: "coder", CorpID: "ww2", AgentID: 2, Secret: "s", Token: "t", EncryptKey: "${K}"}
	if a.WeComPath() != "/webhook/wecom/wc-a/" {
		t.Fatalf("default path = %q", a.WeComPath())
	}
	if err := ValidateChannels(openAIConfig(), []Channel{a, b}); err != nil {
		t.Fatalf("two wecom webhooks collided: %v", err)
	}
	raw, err := yaml.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"corp_id: ww1", "agent_id: 1", "encrypt_key: ${K}"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("yaml lacks %q:\n%s", want, raw)
		}
	}
}
