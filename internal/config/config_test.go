package config

import (
	"strings"
	"testing"
	"time"

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
  bot: { loop: plugin:per_chat }
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
	if o.Description == nil || o.Description.Value != "Search the deployment's runbooks." {
		t.Fatalf("description override not decoded: %+v", o.Description)
	}
	if o.Params["query"].Description.Value != "What to look up" {
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
  bot: { loop: plugin:per_chat }
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
			Agents: map[string]Agent{"bot": {Loop: "plugin:per_chat"}},
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

// TestValidateMemoryScope exercises the store scope vocabulary: user (default)
// or agent on private stores; anything else fails at boot.
func TestValidateMemoryScope(t *testing.T) {
	mk := func(scope string) *Config {
		return &Config{
			Agents: map[string]Agent{"bot": {Loop: "./loop.lua", Memory: MemoryAgentConfig{IsInline: true, Inline: MemoryProfile{Stores: map[string]Store{"d": {Backend: "b", Table: "d", Scope: scope}}}}}},
			Memory: Memory{Backends: map[string]Backend{"b": {Provider: "sqlite"}}},
		}
	}
	if err := validate("cfg.yaml", mk("")); err != nil {
		t.Fatalf("empty scope must pass: %v", err)
	}
	if err := validate("cfg.yaml", mk("user")); err != nil {
		t.Fatalf("user scope must pass: %v", err)
	}
	if err := validate("cfg.yaml", mk("agent")); err != nil {
		t.Fatalf("agent scope must pass: %v", err)
	}
	if err := validate("cfg.yaml", mk("fleet")); err == nil {
		t.Fatal("unknown scope must fail at boot")
	}
}

// TestValidateFiles exercises the file-store backend and scratch rules.
func TestValidateFiles(t *testing.T) {
	neg := int64(-1)
	tests := []struct {
		name    string
		files   FilesConfig
		wantErr string
	}{
		{name: "defaults ok", files: FilesConfig{}},
		{name: "fs explicit ok", files: FilesConfig{Backend: "fs", Dir: "./data/files"}},
		{
			name:    "s3 missing fields",
			files:   FilesConfig{Backend: "s3"},
			wantErr: "requires s3.bucket and s3.region",
		},
		{
			name:  "s3 without keys boots (store skipped at boot)",
			files: FilesConfig{Backend: "s3", S3: MediaS3{Bucket: "b", Region: "r"}},
		},
		{
			name:    "unknown backend",
			files:   FilesConfig{Backend: "gcs"},
			wantErr: "unsupported files backend",
		},
		{
			name:    "negative max bytes",
			files:   FilesConfig{MaxFileBytes: neg},
			wantErr: "files.max_file_bytes must be >= 0",
		},
		{
			name:    "malformed scratch ttl",
			files:   FilesConfig{ScratchTTL: "tomorrow"},
			wantErr: "files.scratch_ttl",
		},
		{name: "scratch ttl ok", files: FilesConfig{ScratchTTL: "24h"}},
		{
			name:    "malformed gc grace",
			files:   FilesConfig{GCGrace: "eventually"},
			wantErr: "files.gc_grace",
		},
		{name: "gc grace ok", files: FilesConfig{GCGrace: "24h"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{Files: tt.files, Agents: map[string]Agent{"bot": {Loop: "./loop.lua"}}}
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
	// Defaults: 32 MiB ceiling, 24h scratch TTL, 24h GC grace.
	f := FilesConfig{}
	if f.FilesMaxBytes() != 32<<20 || f.FilesScratchTTL() != 24*time.Hour || f.FilesGCGrace() != 24*time.Hour {
		t.Fatalf("files defaults: max=%d ttl=%v gc=%v", f.FilesMaxBytes(), f.FilesScratchTTL(), f.FilesGCGrace())
	}
}

// TestValidateUsers exercises the users-API rules: the profile store lives in
// the identity layer, registration mode is a closed vocabulary, and the link
// TTL must parse.
func TestValidateUsers(t *testing.T) {
	no := false
	tests := []struct {
		name    string
		users   UsersConfig
		ident   bool
		wantErr string
	}{
		{name: "defaults ok"},
		{
			name:    "enabled requires the identity layer",
			users:   UsersConfig{Enabled: true},
			wantErr: "requires runtime.identity.enabled",
		},
		{name: "enabled with identity ok", users: UsersConfig{Enabled: true}, ident: true},
		{name: "invite mode ok", users: UsersConfig{Enabled: true, Registration: "invite"}, ident: true},
		{
			name:    "unknown registration mode",
			users:   UsersConfig{Registration: "closed"},
			wantErr: "registration must be open or invite",
		},
		{
			name:    "malformed link ttl",
			users:   UsersConfig{LinkTTL: "soon"},
			wantErr: "runtime.users.link_ttl",
		},
		{name: "link ttl ok", users: UsersConfig{LinkTTL: "15m"}},
		{name: "auto-claim opt-out ok", users: UsersConfig{RequireRegistration: &no}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{Agents: map[string]Agent{"bot": {Loop: "./loop.lua"}}}
			c.Runtime.Users = tt.users
			c.Runtime.Identity.Enabled = tt.ident
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
	// Defaults: registration required, open mode, 10m challenge lifetime.
	u := UsersConfig{}
	if !u.RegistrationRequired() || u.RegistrationMode() != "open" || u.LinkChallengeTTL() != 10*time.Minute {
		t.Fatalf("users defaults: required=%v mode=%q ttl=%v",
			u.RegistrationRequired(), u.RegistrationMode(), u.LinkChallengeTTL())
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

// TestValidatePrompts: each prompt entry names exactly one source, and every
// reference into the registry (agent/spawn instructions, tool description
// overrides) must resolve — a missing key is a boot error, never a silently
// empty prompt.
func TestValidatePrompts(t *testing.T) {
	base := func() *Config {
		return &Config{
			Agents:  map[string]Agent{"bot": {Loop: "./loop.lua"}},
			Prompts: map[string]Prompt{"sys": {Inline: "you are helpful"}},
		}
	}
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "inline ok", mutate: func(c *Config) {}},
		{name: "text alias ok", mutate: func(c *Config) { c.Prompts["t"] = Prompt{Text: "alias"} }},
		{name: "file ok", mutate: func(c *Config) { c.Prompts["f"] = Prompt{File: "./x.md"} }},
		{
			name:    "no source",
			mutate:  func(c *Config) { c.Prompts["empty"] = Prompt{} },
			wantErr: "has no file, inline, or text",
		},
		{
			name:    "two sources",
			mutate:  func(c *Config) { c.Prompts["both"] = Prompt{Inline: "a", File: "./x.md"} },
			wantErr: "more than one",
		},
		{
			name: "instructions prompt ok",
			mutate: func(c *Config) {
				c.Agents["bot"] = Agent{Loop: "./loop.lua", Instructions: InstructionsRef{Prompt: "sys"}}
			},
		},
		{
			name: "instructions unknown prompt",
			mutate: func(c *Config) {
				c.Agents["bot"] = Agent{Loop: "./loop.lua", Instructions: InstructionsRef{Prompt: "nope"}}
			},
			wantErr: "unknown prompt",
		},
		{
			name: "spawn instructions unknown prompt",
			mutate: func(c *Config) {
				c.Profiles.Agent = map[string]SpawnProfile{
					"w": {Loop: "./w.lua", Instructions: InstructionsRef{Prompt: "nope"}},
				}
			},
			wantErr: "unknown prompt",
		},
		{
			name: "tool description prompt ok",
			mutate: func(c *Config) {
				c.Tools.Policy.Overrides = map[string]ToolSpecOverride{
					"builtin:web_search": {Description: &PromptString{Value: "sys", IsRef: true}},
				}
			},
		},
		{
			name: "tool description unknown prompt",
			mutate: func(c *Config) {
				c.Tools.Policy.Overrides = map[string]ToolSpecOverride{
					"builtin:web_search": {Description: &PromptString{Value: "nope", IsRef: true}},
				}
			},
			wantErr: "unknown prompt",
		},
		{
			name: "tool param description unknown prompt",
			mutate: func(c *Config) {
				c.Tools.Policy.Overrides = map[string]ToolSpecOverride{
					"builtin:web_search": {Params: map[string]ToolParamOverride{
						"query": {Description: PromptString{Value: "nope", IsRef: true}},
					}},
				}
			},
			wantErr: "unknown prompt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			tt.mutate(c)
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
}

// TestInstructionsRefDecode: instructions accepts a scalar file path (the
// existing shape) or {prompt: <key>}; tool descriptions accept a literal or a
// prompt reference. Anything else is a decode error under KnownFields.
func TestInstructionsRefDecode(t *testing.T) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(`
version: "1"
agents:
  promptbot:
    loop: plugin:per_chat
    instructions: {prompt: assistant_system}
  filebot:
    loop: plugin:per_chat
    instructions: ./prompts/filebot.md
prompts:
  assistant_system: { inline: "You are the assistant." }
  from_file: { file: ./prompts/sys.md }
tools:
  policy:
    overrides:
      "builtin:web_search":
        description: {prompt: assistant_system}
        params:
          query: { description: "literal query help" }
`))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		t.Fatal(err)
	}
	if got := c.Agents["promptbot"].Instructions; got.Prompt != "assistant_system" || got.File != "" {
		t.Fatalf("prompt ref not decoded: %+v", got)
	}
	if got := c.Agents["filebot"].Instructions; got.File != "./prompts/filebot.md" || got.Prompt != "" {
		t.Fatalf("file path not decoded: %+v", got)
	}
	o := c.Tools.Policy.Overrides["builtin:web_search"]
	if o.Description == nil || !o.Description.IsRef || o.Description.Value != "assistant_system" {
		t.Fatalf("description prompt ref not decoded: %+v", o.Description)
	}
	if q := o.Params["query"].Description; q.IsRef || q.Value != "literal query help" {
		t.Fatalf("literal param description not decoded: %+v", q)
	}

	// A malformed instructions reference is a decode error, not a silent path.
	bad := yaml.NewDecoder(strings.NewReader(`
agents:
  bot: { loop: plugin:per_chat, instructions: {nope: 1} }
`))
	bad.KnownFields(true)
	if err := bad.Decode(&Config{}); err == nil {
		t.Fatal("instructions with an unknown key must fail to decode")
	}
}

// TestValidateMemoryBackendProviders: every provider the engine registers is
// accepted, and an unknown name is still a boot error. The two lists — this
// switch and the RegisterProvider calls in main.go — have to stay in step, or
// a config naming a real provider fails validation before it can be opened.
func TestValidateMemoryBackendProviders(t *testing.T) {
	supported := []string{
		"sqlite", "redis", "mongodb", "postgres",
		"pgvector", "qdrant", "redisvector", "volatile",
	}
	for _, provider := range supported {
		t.Run(provider, func(t *testing.T) {
			c := &Config{
				Agents: map[string]Agent{"bot": {Loop: "./loop.lua"}},
				Memory: Memory{Backends: map[string]Backend{"b": {Provider: provider}}},
			}
			if err := validate("cfg.yaml", c); err != nil {
				t.Fatalf("provider %q must validate: %v", provider, err)
			}
		})
	}

	c := &Config{
		Agents: map[string]Agent{"bot": {Loop: "./loop.lua"}},
		Memory: Memory{Backends: map[string]Backend{"b": {Provider: "builtin:pinecone"}}},
	}
	if err := validate("cfg.yaml", c); err == nil || !strings.Contains(err.Error(), "unsupported provider") {
		t.Fatalf("an unknown provider must fail the boot, got %v", err)
	}

	// The retired "builtin:" spelling is recognized by name, so the message
	// says what to write instead of implying the backend does not exist.
	c = &Config{
		Agents: map[string]Agent{"bot": {Loop: "./loop.lua"}},
		Memory: Memory{Backends: map[string]Backend{"b": {Provider: "builtin:sqlite"}}},
	}
	if err := validate("cfg.yaml", c); err == nil || !strings.Contains(err.Error(), "providers are unprefixed") {
		t.Fatalf("the pre-rename spelling must be called out, got %v", err)
	}
}

// TestValidateModelThinking: the thinking level is a closed vocabulary, and a
// config typo is a boot error — not a silently-dropped value at first call.
func TestValidateModelThinking(t *testing.T) {
	for _, level := range []string{"", "off", "low", "medium", "high", "xhigh", "max", "HIGH"} {
		c := &Config{
			Agents: map[string]Agent{"bot": {Loop: "./loop.lua"}},
			Models: map[string]Model{"default": {Provider: "openai", Model: "m", Thinking: level}},
		}
		if err := validate("cfg.yaml", c); err != nil {
			t.Fatalf("thinking %q must validate: %v", level, err)
		}
	}

	c := &Config{
		Agents: map[string]Agent{"bot": {Loop: "./loop.lua"}},
		Models: map[string]Model{"default": {Provider: "openai", Model: "m", Thinking: "banana"}},
	}
	if err := validate("cfg.yaml", c); err == nil || !strings.Contains(err.Error(), "unsupported thinking level") {
		t.Fatalf("an unknown level must fail the boot, got %v", err)
	}
}

// TestValidateReservedMemoryProfileName: "conversational" is the one built-in
// memory profile and is resolved before profiles.memory is consulted, so a
// user profile of that name would never run. Refusing the name beats silently
// ignoring it.
func TestValidateReservedMemoryProfileName(t *testing.T) {
	c := &Config{
		Agents:   map[string]Agent{"bot": {Loop: "./loop.lua"}},
		Profiles: Profiles{Memory: map[string]MemoryProfile{"conversational": {}}},
	}
	if err := validate("cfg.yaml", c); err == nil || !strings.Contains(err.Error(), "reserved for the built-in profile") {
		t.Fatalf("the built-in profile name must be reserved, got %v", err)
	}

	// Any other name is fine.
	c = &Config{
		Agents:   map[string]Agent{"bot": {Loop: "./loop.lua"}},
		Profiles: Profiles{Memory: map[string]MemoryProfile{"chatty": {}}},
	}
	if err := validate("cfg.yaml", c); err != nil {
		t.Fatalf("an unrelated profile name must validate: %v", err)
	}
}

// TestValidateBrowser: account_id and api_token are set together or not at
// all. A half-configured block is a mistake the operator can fix now, so it
// fails the boot. Whether the token *resolves* is deliberately not checked
// here — it may be a lazy secret reference, and an unresolvable credential
// degrades at construction with a warning instead of failing the boot, the
// same rule the search engines and memory backends follow.
func TestValidateBrowser(t *testing.T) {
	cfgWith := func(b Browser) *Config {
		return &Config{
			Agents:  map[string]Agent{"bot": {Loop: "./loop.lua"}},
			Browser: b,
		}
	}

	valid := map[string]Browser{
		"unset":            {},
		"fully set":        {AccountID: "acct", APIToken: "tok"},
		"lazy token":       {AccountID: "acct", APIToken: "${CF_TOKEN}"},
		"credential token": {AccountID: "acct", APIToken: "cred:cloudflare"},
		"with base_url":    {AccountID: "acct", APIToken: "tok", BaseURL: "http://localhost:1234"},
		"with a timeout":   {AccountID: "acct", APIToken: "tok", Timeout: "45s"},
	}
	for name, b := range valid {
		t.Run(name, func(t *testing.T) {
			if err := validate("cfg.yaml", cfgWith(b)); err != nil {
				t.Fatalf("must validate: %v", err)
			}
		})
	}

	if err := validate("cfg.yaml", cfgWith(Browser{AccountID: "acct"})); err == nil ||
		!strings.Contains(err.Error(), "api_token is empty") {
		t.Fatalf("an account without a token must fail the boot, got %v", err)
	}
	if err := validate("cfg.yaml", cfgWith(Browser{APIToken: "tok"})); err == nil ||
		!strings.Contains(err.Error(), "account_id is empty") {
		t.Fatalf("a token without an account must fail the boot, got %v", err)
	}
}

// TestBrowserTimeoutD: the default is 60s rather than the search engines' 30s,
// because a page load may consume the full 60s gotoOptions timeout before the
// action itself runs.
func TestBrowserTimeoutD(t *testing.T) {
	if got := (Browser{}).TimeoutD(); got != 60*time.Second {
		t.Fatalf("default = %v; want 60s", got)
	}
	if got := (Browser{Timeout: "45s"}).TimeoutD(); got != 45*time.Second {
		t.Fatalf("explicit = %v; want 45s", got)
	}
	if got := (Browser{Timeout: "nonsense"}).TimeoutD(); got != 60*time.Second {
		t.Fatalf("unparseable = %v; want the 60s default", got)
	}
}

// TestLoadNetBlock: the outbound-HTTP policy parses under KnownFields
// strictness and round-trips, and the guard is on unless asked otherwise.
//
// Validation has nothing to add here — a bool cannot be malformed — so the
// boot warning the NetHTTP doc promises is emitted where the policy is built,
// in main.go, rather than at load.
func TestLoadNetBlock(t *testing.T) {
	const base = `
version: "1"
models:
  default:
    provider: openai
    model: m
    api_key: k
gateway:
  listen: ":0"
`
	const profile = `
name: bot
loop: plugin:per_chat
`

	on := writeDir(t, map[string]string{
		"system.yaml":       base + "net:\n  http:\n    allow_private: true\n",
		"profiles/bot.yaml": profile,
	})
	cfg, err := LoadDir(on, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Net.HTTP.AllowPrivate {
		t.Fatal("net.http.allow_private must round-trip")
	}

	// Unset means guard. This is the default that matters: the zero value has
	// to be the safe one.
	off := writeDir(t, map[string]string{
		"system.yaml":       base,
		"profiles/bot.yaml": profile,
	})
	cfg, err = LoadDir(off, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Net.HTTP.AllowPrivate {
		t.Fatal("the address guard must be on by default")
	}

	// An unknown key is still a boot error, like every other block.
	bad := writeDir(t, map[string]string{
		"system.yaml":       base + "net:\n  http:\n    allow_private: true\n    allow_everything: true\n",
		"profiles/bot.yaml": profile,
	})
	if _, err := LoadDir(bad, discardLog()); err == nil {
		t.Fatal("an unknown key under net.http must fail the boot")
	}
}

// TestValidateTimezoneOffset: the cron matching offset is a fixed UTC offset
// in the real-world range, and TimezoneOffset converts it to a duration.
func TestValidateTimezoneOffset(t *testing.T) {
	tests := []struct {
		name    string
		hours   float64
		wantErr string
	}{
		{name: "unset is UTC", hours: 0},
		{name: "UTC+8", hours: 8},
		{name: "half-hour offset", hours: -5.5},
		{name: "far east", hours: 14},
		{name: "far west", hours: -12},
		{name: "past the date line", hours: 14.5, wantErr: "outside the real-world range"},
		{name: "nonsense", hours: -30, wantErr: "outside the real-world range"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{
				Runtime: Runtime{TimezoneOffsetHours: tt.hours},
				Agents:  map[string]Agent{"bot": {Loop: "./loop.lua"}},
			}
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

	if got := (&Config{Runtime: Runtime{TimezoneOffsetHours: 5.5}}).Runtime.TimezoneOffset(); got != 5*time.Hour+30*time.Minute {
		t.Fatalf("TimezoneOffset = %v", got)
	}
	if got := (&Config{}).Runtime.TimezoneOffset(); got != 0 {
		t.Fatalf("unset offset = %v; want 0", got)
	}
}
