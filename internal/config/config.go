// Package config loads agentflow.yaml (phase-2 schema: memory backends, tools,
// MCP, runtime persistence, agent capabilities/skills/memory).
//
// The file is environment-expanded (${VAR}, ${VAR:-default}) before parsing,
// and parsed strictly: an unknown key is a boot error, per docs/yaml-config.md.
package config

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Version string              `yaml:"version"`
	Runtime Runtime               `yaml:"runtime"`
	Models  map[string]Model     `yaml:"models"`
	Memory  Memory               `yaml:"memory"`
	Profiles Profiles             `yaml:"profiles"`
	Gateway Gateway                `yaml:"gateway"`
	MCP     MCP                    `yaml:"mcp"`
	Tools   Tools                  `yaml:"tools"`
	Agents  map[string]Agent      `yaml:"agents"`
	Plugins Plugins                `yaml:"plugins"`
}

// Runtime contains instance-wide tuning and persistence.
type Runtime struct {
	VM struct {
		MemoryLimit       string `yaml:"memory_limit"`
		InstructionBudget string `yaml:"instruction_budget"`
	} `yaml:"vm"`
	Scheduler struct {
		Workers int `yaml:"workers"`
	} `yaml:"scheduler"`
	Reload struct {
		Watch bool `yaml:"watch"`
	} `yaml:"reload"`
	Persistence string `yaml:"persistence"` // e.g. sqlite://./data/agentflow.db
	Admin       AdminConfig `yaml:"admin"`
	Identity    IdentityConfig `yaml:"identity"`
	Credentials CredentialsConfig `yaml:"credentials"`
}

// CredentialsConfig controls the encrypted per-tenant credential store. When
// enabled, the http.request op can resolve an `auth={service=...}` reference
// to a stored API key at request time. The master key is read from the named
// environment variable at boot (never from the config file).
type CredentialsConfig struct {
	Enabled      bool   `yaml:"enabled"`
	Path         string `yaml:"path"` // sqlite path; "" = <runtime persistence dir>/credentials.db
	MasterKeyEnv string `yaml:"master_key_env"` // env var holding the master key; default "CREDENTIALS_MASTER_KEY"
}

// AdminConfig configures the metrics/admin HTTP endpoint.
type AdminConfig struct {
	Listen string `yaml:"listen"` // default 127.0.0.1:9090 (loopback only)
}

// IdentityConfig configures the optional user-identity layer. When disabled
// (the default), channel drivers submit to the router directly and message
// From stays channel-native — zero behavior change. When enabled, every
// inbound is minted a stable user UUID (persisted here) and From is rewritten
// to "user:<uuid>", making loops channel-agnostic and proactive push uniform.
type IdentityConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Persistence string `yaml:"persistence"` // sqlite path; "" = <runtime persistence dir>/identity.db
}

// Model is a named LLM provider configuration.
type Model struct {
	Provider  string `yaml:"provider"`
	Model     string `yaml:"model"`
	APIKey    string `yaml:"api_key"`
	BaseURL   string `yaml:"base_url"`
	Timeout   string `yaml:"timeout"`
	Retry     int    `yaml:"retry"`
	MaxTokens int    `yaml:"max_tokens"`
}

func (m Model) TimeoutD() time.Duration {
	if m.Timeout == "" {
		return 60 * time.Second
	}
	d, err := time.ParseDuration(m.Timeout)
	if err != nil {
		return 60 * time.Second
	}
	return d
}

// Memory holds instance-wide backend definitions.
type Memory struct {
	Backends map[string]Backend `yaml:"backends"`
}

type Backend struct {
	Provider string         `yaml:"provider"`
	Config   map[string]any `yaml:"config"`
}

// Profiles holds reusable named profiles. Memory and shell profiles are shared
// wiring; agent profiles are spawn templates whose grants can only be narrowed.
// Safety profiles select the core-owned safety filter chain.
type Profiles struct {
	Memory  map[string]MemoryProfile `yaml:"memory"`
	Shell  map[string]ShellProfile  `yaml:"shell"`
	Agent  map[string]SpawnProfile  `yaml:"agent"`
	Safety map[string]SafetyProfile  `yaml:"safety"`
}

// SafetyProfile configures the core-owned safety chain. An empty profile
// ("none") explicitly opts out; agents must name a safety profile.
type SafetyProfile struct {
	Filters []string `yaml:"filters"`
}

// MemoryProfile defines logical stores and the filter chain.
type MemoryProfile struct {
	Stores map[string]Store `yaml:"stores"`
	Write  []string         `yaml:"write"`
	Recall string           `yaml:"recall"`
}

type Store struct {
	Backend    string   `yaml:"backend"`
	Table      string   `yaml:"table"`
	Collection string   `yaml:"collection"`
	Retention  string   `yaml:"retention"`
	Window     int      `yaml:"window"`
	Requires   []string `yaml:"requires"`
	Policy     string   `yaml:"policy"`
}

// ShellProfile defines defaults for shell.spawn.
type ShellProfile struct {
	Provider string            `yaml:"provider"` // docker (one-shot) | vultr (persistent) | ssh
	Image    string            `yaml:"image"`    // docker: default alpine:3.20
	WorkDir  string            `yaml:"workdir"`
	Network  string            `yaml:"network"`
	MemLimit string            `yaml:"mem_limit"`
	CPULimit float64           `yaml:"cpu_limit"`
	Env      map[string]string `yaml:"env"`
	Host     string            `yaml:"host"`
	User     string            `yaml:"user"`
	Password string            `yaml:"password"`
	KeyFile  string            `yaml:"key_file"`

	// Vultr provisioning (provider: vultr). The API key is resolved from the
	// VULTR_API_KEY environment variable at boot (see cmd/agentflow), never
	// stored in this struct.
	Region  string   `yaml:"region"`  // e.g. "ewr"
	Plan    string   `yaml:"plan"`    // e.g. "vc2-1c-1gb"
	OsID    int      `yaml:"os_id"`   // integer (Vultr OS id)
	Label   string   `yaml:"label"`   // instance label prefix
	SSHKey  string   `yaml:"ssh_key"` // inline public key (optional)
	SSHKeyID string  `yaml:"sshkey_id"` // pre-registered Vultr ssh-key id (optional)
	Tags    []string `yaml:"tags"`
}

// SpawnProfile is a reusable template for an ephemeral child. It contains
// references rather than raw memory/shell credentials, and its grants are
// always intersected with the spawning actor's effective grants.
type SpawnProfile struct {
	Model        string             `yaml:"model"`
	Loop         string             `yaml:"loop"`
	Instructions string             `yaml:"instructions"`
	Memory       string             `yaml:"memory"`
	Shell        string             `yaml:"shell"`
	Skills       []string           `yaml:"skills"`
	Capabilities []string           `yaml:"capabilities"`
	CanContact   []string           `yaml:"can_contact"`
	Ephemeral    EphemeralLifecycle `yaml:"ephemeral"`
	Budget       BudgetConfig       `yaml:"budget"`
}

type EphemeralLifecycle struct {
	MaxLifetime string `yaml:"max_lifetime"`
	IdleTTL     string `yaml:"idle_ttl"`
	MaxTurns    int    `yaml:"max_turns"`
}

type BudgetConfig struct {
	TokensPerDay int64 `yaml:"tokens_per_day"`
}

// AgentConfig can be a string profile reference or an inline profile.
// YAML: `memory: builtin:conversational` or `memory: { stores: ... }`.
type MemoryAgentConfig struct {
	Profile  string
	IsInline bool
	Inline   MemoryProfile
}

func (m *MemoryAgentConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		return node.Decode(&m.Profile)
	}
	m.IsInline = true
	return node.Decode(&m.Inline)
}

// MCP servers (stdio only for Phase 2).
type MCP struct {
	Servers map[string]MCPServer `yaml:"servers"`
}

type MCPServer struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	URL     string   `yaml:"url"`
	Token   string   `yaml:"token"`
}

// Tools policy and overrides.
type Tools struct {
	Policy ToolsPolicy `yaml:"policy"`
}

type ToolsPolicy struct {
	Default   string                    `yaml:"default"`
	Write     string                    `yaml:"write"`
	Forbidden []string                  `yaml:"forbidden"`
	Overrides map[string]ToolSpecOverride `yaml:"overrides"`
}

// ToolSpecOverride uses pointers so "not set" is distinct from explicit false/0.
type ToolSpecOverride struct {
	NeedsConfirm *bool   `yaml:"needs_confirm"`
	Permission   *string `yaml:"permission"`
	CostLevel    *int    `yaml:"cost_level"`
	UserVisible  *bool   `yaml:"user_visible"`
	Autonomous   *bool   `yaml:"autonomous"`
}

// Plugins controls the global capability ceiling.
type Plugins struct {
	Dir               string   `yaml:"dir"`
	AllowCapabilities []string `yaml:"allow_capabilities"`
}

// Agent is a configured agent.
type Agent struct {
	Loop          string            `yaml:"loop"`
	Model         string            `yaml:"model"`
	Instructions  string            `yaml:"instructions"`
	HistoryBudget int               `yaml:"history_budget"`
	Memory        MemoryAgentConfig `yaml:"memory"`
	Safety        string            `yaml:"safety"`
	Shell         string            `yaml:"shell"`
	Skills        []string          `yaml:"skills"`
	Capabilities  []string          `yaml:"capabilities"`
	CanContact    []string          `yaml:"can_contact"`
	Taps          []string          `yaml:"taps"`
	Persistent    bool              `yaml:"persistent"`
	Singleton     bool              `yaml:"singleton"`
	Lifecycle     map[string]any    `yaml:"lifecycle"`
	Budget        map[string]any    `yaml:"budget"`
	Channels      []string          `yaml:"channels"`
}

type Gateway struct {
	Route    string    `yaml:"route"`
	Channels []Channel `yaml:"channels"`
}

type Channel struct {
	Name       string  `yaml:"name"`
	Type       string  `yaml:"type"` // webhook | telegram
	Mode       string  `yaml:"mode"` // telegram: polling (default) | webhook
	Agent      string  `yaml:"agent"`
	Listen     string  `yaml:"listen"`
	Path       string  `yaml:"path"`
	Token      string  `yaml:"token"`
	AllowUsers []int64 `yaml:"allow_users"`
}

// DefaultCapabilities is what an agent gets if capabilities are omitted.
var DefaultCapabilities = []string{"llm.chat", "memory", "tools", "agent.send", "net.http"}

// DefaultMemoryProfile is the expansion of `memory: builtin:conversational`.
func DefaultMemoryProfile() MemoryProfile {
	return MemoryProfile{
		Stores: map[string]Store{
			"dialogue": {
				Backend:   "main_db",
				Table:     "dialogue",
				Retention: "30d",
				Window:    1000,
				Requires:  []string{"kv"},
			},
			"facts": {
				Backend:   "main_db",
				Table:     "facts",
				Retention: "forever",
				Requires:  []string{"kv", "text_search"},
			},
		},
		Write:  []string{"builtin:routing_table"},
		Recall: "builtin:recency",
	}
}

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

func expandEnv(b []byte) []byte {
	return envRe.ReplaceAllFunc(b, func(m []byte) []byte {
		parts := envRe.FindSubmatch(m)
		if v, ok := os.LookupEnv(string(parts[1])); ok {
			return []byte(v)
		}
		if parts[2] != nil {
			return parts[2]
		}
		return nil
	})
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(expandEnv(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := validate(path, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func validate(path string, c *Config) error {
	if len(c.Agents) == 0 {
		return fmt.Errorf("%s: no agents defined", path)
	}
	if c.Runtime.Scheduler.Workers <= 0 {
		c.Runtime.Scheduler.Workers = 8
	}
	if c.Runtime.Persistence == "" {
		c.Runtime.Persistence = "sqlite://./data/agentflow.db"
	}
	if c.Runtime.Credentials.Enabled && c.CredentialsMasterKeyEnv() == "" {
		return fmt.Errorf("%s: runtime.credentials.enabled requires master_key_env to name the env var holding the master key", path)
	}

	allowedCaps := map[string]bool{}
	for _, cap := range c.Plugins.AllowCapabilities {
		allowedCaps[cap] = true
	}
	// If the global ceiling is empty, be permissive for backward compat.
	if len(allowedCaps) == 0 {
		for _, cap := range DefaultCapabilities {
			allowedCaps[cap] = true
		}
		allowedCaps["scheduler"] = true
		allowedCaps["shell.exec"] = true
		allowedCaps["gateway"] = true
		allowedCaps["vector"] = true
		allowedCaps["store.raw"] = true
		allowedCaps["agent.spawn"] = true
		allowedCaps["agent.request"] = true
		allowedCaps["channel.push"] = true
	}

	for name, a := range c.Agents {
		if a.Loop == "" {
			return fmt.Errorf("%s: agent %q has no loop", path, name)
		}
		if a.Model != "" {
			if _, ok := c.Models[a.Model]; !ok {
				return fmt.Errorf("%s: agent %q references unknown model %q", path, name, a.Model)
			}
		}
		if a.HistoryBudget <= 0 {
			a.HistoryBudget = 6000
		}
		caps := a.Capabilities
		if len(caps) == 0 {
			caps = DefaultCapabilities
		}
		for _, cap := range caps {
			if !allowedCaps[cap] {
				return fmt.Errorf("%s: agent %q capability %q not in plugins.allow_capabilities", path, name, cap)
			}
		}
		if a.Shell != "" {
			if !allowedCaps["shell.exec"] {
				return fmt.Errorf("%s: agent %q uses shell but lacks shell.exec capability", path, name)
			}
			if c.Profiles.Shell != nil {
				if _, ok := c.Profiles.Shell[a.Shell]; !ok {
					return fmt.Errorf("%s: agent %q references unknown shell profile %q", path, name, a.Shell)
				}
				if err := validateShellProfile(path, name, a.Shell, c.Profiles.Shell[a.Shell]); err != nil {
					return err
				}
			} else {
				return fmt.Errorf("%s: agent %q references unknown shell profile %q", path, name, a.Shell)
			}
		}
		if a.Memory.IsInline || a.Memory.Profile != "" {
			if !allowedCaps["memory"] {
				return fmt.Errorf("%s: agent %q uses memory but lacks memory capability", path, name)
			}
			if a.Memory.Profile == "builtin:conversational" {
				continue
			}
			if !a.Memory.IsInline {
				return fmt.Errorf("%s: agent %q memory profile %q is not a builtin preset", path, name, a.Memory.Profile)
			}
			for sname, store := range a.Memory.Inline.Stores {
				if _, ok := c.Memory.Backends[store.Backend]; !ok {
					return fmt.Errorf("%s: agent %q store %q references unknown backend %q", path, name, sname, store.Backend)
				}
			}
		}
		if len(a.Skills) > 0 {
			if !allowedCaps["tools"] {
				return fmt.Errorf("%s: agent %q has skills but lacks tools capability", path, name)
			}
		}
		// can_contact targets must resolve to a configured agent or a spawn
		// profile. Default-deny ACLs are enforced at runtime; this only
		// rejects broken references at boot.
		for _, target := range a.CanContact {
			if _, ok := c.Agents[target]; ok {
				continue
			}
			if c.Profiles.Agent != nil {
				if _, ok := c.Profiles.Agent[target]; ok {
					continue
				}
			}
			return fmt.Errorf("%s: agent %q can_contact target %q is neither a configured agent nor a spawn profile", path, name, target)
		}
	}

	// Validate spawn profiles: capabilities must sit under the global ceiling,
	// and references to models, shell, and memory profiles must resolve.
	for pname, p := range c.Profiles.Agent {
		if p.Loop == "" {
			return fmt.Errorf("%s: spawn profile %q has no loop", path, pname)
		}
		for _, cap := range p.Capabilities {
			if !allowedCaps[cap] {
				return fmt.Errorf("%s: spawn profile %q capability %q not in plugins.allow_capabilities", path, pname, cap)
			}
		}
		if p.Model != "" {
			if _, ok := c.Models[p.Model]; !ok {
				return fmt.Errorf("%s: spawn profile %q references unknown model %q", path, pname, p.Model)
			}
		}
		if p.Shell != "" {
			if c.Profiles.Shell == nil {
				return fmt.Errorf("%s: spawn profile %q references unknown shell profile %q", path, pname, p.Shell)
			}
			if _, ok := c.Profiles.Shell[p.Shell]; !ok {
				return fmt.Errorf("%s: spawn profile %q references unknown shell profile %q", path, pname, p.Shell)
			}
			if err := validateShellProfile(path, "spawn:"+pname, p.Shell, c.Profiles.Shell[p.Shell]); err != nil {
				return err
			}
		}
		for _, target := range p.CanContact {
			if _, ok := c.Agents[target]; ok {
				continue
			}
			if c.Profiles.Agent != nil {
				if _, ok := c.Profiles.Agent[target]; ok {
					continue
				}
			}
			return fmt.Errorf("%s: spawn profile %q can_contact target %q is neither a configured agent nor a spawn profile", path, pname, target)
		}
	}

	for i, ch := range c.Gateway.Channels {
		if ch.Agent == "" {
			return fmt.Errorf("%s: channel #%d (%s) has no agent", path, i, ch.Type)
		}
		if _, ok := c.Agents[ch.Agent]; !ok {
			return fmt.Errorf("%s: channel %q references unknown agent %q", path, ch.Name, ch.Agent)
		}
		switch ch.Type {
		case "webhook":
			if ch.Listen == "" {
				return fmt.Errorf("%s: webhook channel %q has no listen", path, ch.Name)
			}
		case "telegram":
			if ch.Token == "" {
				return fmt.Errorf("%s: telegram channel %q has no token", path, ch.Name)
			}
			if ch.Mode == "" {
				ch.Mode = "polling"
			}
			if ch.Mode != "polling" && ch.Mode != "webhook" {
				return fmt.Errorf("%s: telegram channel %q has unsupported mode %q (use polling or webhook)", path, ch.Name, ch.Mode)
			}
			if ch.Mode == "webhook" && ch.Listen == "" {
				return fmt.Errorf("%s: telegram channel %q webhook mode requires listen", path, ch.Name)
			}
		default:
			return fmt.Errorf("%s: unsupported channel type %q", path, ch.Type)
		}
	}

	for name, m := range c.Models {
		switch m.Provider {
		case "anthropic", "openai", "openai-responses":
		default:
			return fmt.Errorf("%s: model %q has unsupported provider %q", path, name, m.Provider)
		}
	}

	for bname, b := range c.Memory.Backends {
		switch b.Provider {
		case "builtin:sqlite", "builtin:redis", "builtin:mongodb", "builtin:postgres", "builtin:pgvector", "builtin:volatile":
		default:
			return fmt.Errorf("%s: memory backend %q has unsupported provider %q", path, bname, b.Provider)
		}
	}

	for sname, s := range c.MCP.Servers {
		if s.Command == "" && s.URL == "" {
			return fmt.Errorf("%s: mcp server %q has neither command nor url", path, sname)
		}
	}

	return nil
}

// validateShellProfile checks provider-specific requirements for a shell
// profile referenced by an agent or spawn profile. Vultr provisioning needs
// region and plan; the API key is read from the environment at boot, so it is
// not checked here (a missing key fails fast at spawn with a clear error).
func validateShellProfile(path, owner, name string, p ShellProfile) error {
	switch p.Provider {
	case "vultr":
		if p.Region == "" || p.Plan == "" {
			return fmt.Errorf("%s: shell profile %q (used by %q) is provider vultr but missing required region/plan", path, name, owner)
		}
	case "ssh":
		if p.Host == "" {
			return fmt.Errorf("%s: shell profile %q (used by %q) is provider ssh but missing required host", path, name, owner)
		}
	case "docker", "":
		// docker is one-shot; image defaults to alpine:3.20 if unset. No reqs.
	default:
		return fmt.Errorf("%s: shell profile %q (used by %q) has unknown provider %q", path, name, owner, p.Provider)
	}
	return nil
}

// ResolveMemoryProfile returns the concrete memory profile for an agent.
// It expands `builtin:conversational`.
func (c *Config) ResolveMemoryProfile(a Agent) MemoryProfile {
	if a.Memory.Profile == "builtin:conversational" {
		// Ensure the default backend is present if not already defined.
		mp := DefaultMemoryProfile()
		if c.Memory.Backends == nil {
			c.Memory.Backends = map[string]Backend{}
		}
		if _, ok := c.Memory.Backends["main_db"]; !ok {
			c.Memory.Backends["main_db"] = Backend{
				Provider: "builtin:sqlite",
				Config: map[string]any{
					"path": "./data/agentflow.db",
				},
			}
		}
		return mp
	}
	if a.Memory.IsInline {
		return a.Memory.Inline
	}
	return MemoryProfile{}
}

// PersistencePath strips the sqlite:// prefix from Runtime.Persistence.
func (c *Config) PersistencePath() string {
	p := c.Runtime.Persistence
	if p == "" {
		p = "sqlite://./data/agentflow.db"
	}
	const prefix = "sqlite://"
	if len(p) > len(prefix) && p[:len(prefix)] == prefix {
		return p[len(prefix):]
	}
	return p
}

// IdentityPath returns the sqlite path for the identity registry, resolved
// beside the runtime persistence path unless explicitly overridden.
func (c *Config) IdentityPath() string {
	if c.Runtime.Identity.Persistence != "" {
		return c.Runtime.Identity.Persistence
	}
	// Default to a sibling file in the runtime persistence directory.
	p := c.PersistencePath()
	dir := filepath.Dir(p)
	if dir == "" || dir == "." {
		return "identity.db"
	}
	return dir + "/identity.db"
}

// CredentialsPath returns the sqlite path for the credential store, resolved
// beside the runtime persistence path unless explicitly overridden.
func (c *Config) CredentialsPath() string {
	if c.Runtime.Credentials.Path != "" {
		return c.Runtime.Credentials.Path
	}
	p := c.PersistencePath()
	dir := filepath.Dir(p)
	if dir == "" || dir == "." {
		return "credentials.db"
	}
	return dir + "/credentials.db"
}

// CredentialsMasterKeyEnv returns the env var holding the credential master
// key, defaulting to CREDENTIALS_MASTER_KEY.
func (c *Config) CredentialsMasterKeyEnv() string {
	if c.Runtime.Credentials.MasterKeyEnv != "" {
		return c.Runtime.Credentials.MasterKeyEnv
	}
	return "CREDENTIALS_MASTER_KEY"
}
