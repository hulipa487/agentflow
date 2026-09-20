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
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Version  string            `yaml:"version"`
	Runtime  Runtime           `yaml:"runtime"`
	Models   map[string]Model  `yaml:"models"`
	Memory   Memory            `yaml:"memory"`
	Profiles Profiles          `yaml:"profiles"`
	Gateway  Gateway           `yaml:"gateway"`
	MCP      MCP               `yaml:"mcp"`
	Tools    Tools             `yaml:"tools"`
	Search   Search            `yaml:"search"`
	Legal    LegalSearch       `yaml:"legal_search"`
	Browser  Browser           `yaml:"browser"`
	Net      NetConfig         `yaml:"net"`
	Media    MediaConfig       `yaml:"media"`
	Files    FilesConfig       `yaml:"files"`
	Audit    AuditConfig       `yaml:"audit"`
	Agents   map[string]Agent  `yaml:"agents"`
	Triggers []Trigger         `yaml:"triggers"` // configdir layout; declarative task data for loops
	Prompts  map[string]Prompt `yaml:"prompts"`
	Plugins  Plugins           `yaml:"plugins"`
}

// Prompt is one registry entry. Exactly one of File, Inline, or Text must be
// set; Content is filled at load time with the resolved text.
type Prompt struct {
	File    string `yaml:"file"`
	Inline  string `yaml:"inline"`
	Text    string `yaml:"text"` // alias for inline
	Content string `yaml:"-"`
}

// InstructionsRef is either a file path or a reference to a prompt-registry
// key. YAML: "./prompts/x.md" or {prompt: shared}.
type InstructionsRef struct {
	File   string
	Prompt string
}

// UnmarshalYAML accepts a scalar file path or a mapping with exactly the key
// "prompt".
func (i *InstructionsRef) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		i.File = node.Value
		return nil
	case yaml.MappingNode:
		var m map[string]string
		if err := node.Decode(&m); err != nil {
			return err
		}
		p, ok := m["prompt"]
		if !ok {
			return fmt.Errorf("instructions: expected a file path or {prompt: <key>}")
		}
		if len(m) != 1 {
			return fmt.Errorf("instructions: {prompt: <key>} must contain only the prompt key")
		}
		i.Prompt = p
		return nil
	default:
		return fmt.Errorf("instructions: must be a string or {prompt: <key>}")
	}
}

// MediaConfig selects the blob-store backend for inbound channel media.
// Handles ("media:<sha256>") are backend-agnostic, so loops and the journal
// never see where bytes live. Default: fs rooted beside the runtime db.
type MediaConfig struct {
	Backend string  `yaml:"backend"` // fs (default) | s3
	Dir     string  `yaml:"dir"`     // fs root; default <runtime-persistence-dir>/media
	S3      MediaS3 `yaml:"s3"`
}

// MediaS3 configures the S3 blob-store backend. Keys are env-interpolated
// (${VAR}) like model keys so secrets never sit in the file as literals.
type MediaS3 struct {
	Bucket    string `yaml:"bucket"`
	Region    string `yaml:"region"`
	Endpoint  string `yaml:"endpoint"`   // optional custom S3-compatible host (MinIO, R2, ...); empty = AWS
	Prefix    string `yaml:"prefix"`     // optional object key prefix
	AccessKey string `yaml:"access_key"` // env-interpolated
	SecretKey string `yaml:"secret_key"` // env-interpolated
}

// FilesConfig selects the backend for the user-scoped file store. File bytes
// live in the same content-addressed blob store family as media (handles stay
// "media:<sha256>", backend-agnostic); the snapshot metadata (working tree,
// commits, refs, scratch) lives in the runtime store's files_meta table.
type FilesConfig struct {
	Backend      string  `yaml:"backend"`        // fs (default) | s3
	Dir          string  `yaml:"dir"`            // fs root; default <runtime-persistence-dir>/files
	S3           MediaS3 `yaml:"s3"`             // same fields/resolution as media.s3
	MaxFileBytes int64   `yaml:"max_file_bytes"` // per-file ceiling; 0 = 32 MiB
	ScratchTTL   string  `yaml:"scratch_ttl"`    // e.g. 24h; 0 = no expiry; default 24h
	GCGrace      string  `yaml:"gc_grace"`       // blob GC grace window; 0/empty = disabled; default 24h
}

// FilesMaxBytes returns the per-file ceiling, applying the default.
func (f FilesConfig) FilesMaxBytes() int64 {
	if f.MaxFileBytes > 0 {
		return f.MaxFileBytes
	}
	return 32 << 20
}

// FilesScratchTTL parses scratch_ttl. Empty defaults to 24h; "0" disables
// expiry (records persist until deleted). Malformed values fail at validation.
func (f FilesConfig) FilesScratchTTL() time.Duration {
	if f.ScratchTTL == "" {
		return 24 * time.Hour
	}
	d, _ := time.ParseDuration(f.ScratchTTL) // validated at boot; error impossible here
	return d
}

// FilesGCGrace parses gc_grace, the blob-GC minimum blob age. Empty defaults
// to 24h; "0" disables GC entirely (the boot sweep skips it). Malformed
// values fail at validation.
func (f FilesConfig) FilesGCGrace() time.Duration {
	if f.GCGrace == "" {
		return 24 * time.Hour
	}
	d, _ := time.ParseDuration(f.GCGrace) // validated at boot; error impossible here
	return d
}

// AuditConfig controls the core-owned message journal (every inbound and
// outbound message persisted to the runtime store). It is core-owned so loops
// cannot bypass or forge it — that is what makes it an audit trail.
type AuditConfig struct {
	Enabled       *bool `yaml:"enabled"`        // default true
	RetentionDays *int  `yaml:"retention_days"` // default 90; 0 = keep forever
}

// AuditEnabled reports whether the message journal is on (default true).
func (a AuditConfig) AuditEnabled() bool { return a.Enabled == nil || *a.Enabled }

// AuditRetention returns the journal retention in days (default 90; 0 = keep
// forever).
func (a AuditConfig) AuditRetention() int {
	if a.RetentionDays == nil {
		return 90
	}
	return *a.RetentionDays
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
	Persistence string            `yaml:"persistence"` // e.g. sqlite://./data/agentflow.db
	Admin       AdminConfig       `yaml:"admin"`
	Identity    IdentityConfig    `yaml:"identity"`
	Credentials CredentialsConfig `yaml:"credentials"`
	// TimezoneOffsetHours is the instance-wide UTC offset applied to cron
	// trigger matching (e.g. 8 for UTC+8, -5.5 for UTC-5:30). 0 = UTC, the
	// default. every: triggers are unaffected: an interval has no wall clock.
	TimezoneOffsetHours float64 `yaml:"timezone_offset_hours"`
}

// TimezoneOffset is the cron matching offset as a duration. It is a fixed
// offset (not a named zone): cron schedules then never shift under daylight
// saving, matching how deployments describe "09:00 local" for a single region.
func (r Runtime) TimezoneOffset() time.Duration {
	return time.Duration(r.TimezoneOffsetHours * float64(time.Hour))
}

// CredentialsConfig controls the encrypted per-tenant credential store. When
// enabled, the http.request op can resolve an `auth={service=...}` reference
// to a stored API key at request time. The master key is read from the named
// environment variable at boot (never from the config file).
type CredentialsConfig struct {
	Enabled      bool   `yaml:"enabled"`
	Path         string `yaml:"path"`           // sqlite path; "" = <runtime persistence dir>/credentials.db
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

// Model is a named LLM provider configuration. The omitempty tags keep the
// web console's persist output clean (only set fields are written back).
type Model struct {
	Provider  string `yaml:"provider"`
	Model     string `yaml:"model"`
	APIKey    string `yaml:"api_key,omitempty"`
	BaseURL   string `yaml:"base_url,omitempty"`
	Timeout   string `yaml:"timeout,omitempty"`
	Retry     int    `yaml:"retry,omitempty"`
	MaxTokens int    `yaml:"max_tokens,omitempty"`
	// Thinking is the default thinking level for this model: off | low |
	// medium | high | xhigh | max, one vocabulary mapped per provider at
	// request time (see internal/drivers/llm/thinking.go). Empty sends
	// nothing and the provider uses its own default. Overridable per call
	// via llm.chat opts.thinking.
	Thinking string `yaml:"thinking,omitempty"`
	// ServerTools names provider-native server-side tools to enable on every
	// request for this model (e.g. "web_search", "x_search", "google_search").
	// Unlike client-side function tools (opts.Tools), these are executed by the
	// provider inside the completion; the runtime only injects the native tool
	// entry into the request body. The values are provider-specific strings,
	// keeping the runtime generic.
	ServerTools []string `yaml:"server_tools,omitempty"`
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
	Memory map[string]MemoryProfile `yaml:"memory"`
	Shell  map[string]ShellProfile  `yaml:"shell"`
	Agent  map[string]SpawnProfile  `yaml:"agent"`
	Safety map[string]SafetyProfile `yaml:"safety"`
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
	// EmbedModel names a models: entry used to embed record text on write
	// (memory.write attaches the vector) and queries under
	// recall: plugin:semantic. RerankModel names a provider:"rerank"
	// entry; when set, semantic recall oversamples vector hits by
	// Oversample (default 4) and reranks down to k.
	EmbedModel  string `yaml:"embed_model"`
	RerankModel string `yaml:"rerank_model"`
	Oversample  int    `yaml:"oversample"`
}

type Store struct {
	Backend    string   `yaml:"backend"`
	Table      string   `yaml:"table"`
	Collection string   `yaml:"collection"`
	Retention  string   `yaml:"retention"`
	Window     int      `yaml:"window"`
	Requires   []string `yaml:"requires"`
	Policy     string   `yaml:"policy"`
	// Shared opts the store into cross-agent sharing (a deliberate knowledge
	// base). Private stores (the default) are isolated per agent: the
	// physical table is prefixed with the agent name at bind time. Existing
	// deployments that rely on implicit cross-agent sharing must set this.
	Shared bool `yaml:"shared"`
	// Scope selects the isolation granularity of a private store: "user"
	// (default) scopes rows per user at the key level — channel-originated
	// turns read and write only their own scope, service contexts, and
	// pre-upgrade rows; "agent" keeps one pool for the whole agent across
	// users (pre-isolation behavior). Ignored on shared stores.
	Scope string `yaml:"scope"`
}

// ShellProfile defines defaults for shell.spawn.
type ShellProfile struct {
	Provider string            `yaml:"provider"` // docker (persistent local) | ssh (persistent remote)
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
}

// SpawnProfile is a reusable template for an ephemeral child. It contains
// references rather than raw memory/shell credentials, and its grants are
// always intersected with the spawning actor's effective grants.
type SpawnProfile struct {
	Model        string          `yaml:"model"`
	Loop         string          `yaml:"loop"`
	Instructions InstructionsRef `yaml:"instructions"`
	Memory       string          `yaml:"memory"`
	Shell        string          `yaml:"shell"`
	Skills       []string        `yaml:"skills"`
	Capabilities []string        `yaml:"capabilities"`
	CanContact   []string        `yaml:"can_contact"`
	Credentials  []string        `yaml:"credentials"` // credential.get allow-list for spawned children
	// Extras is the spawn profile's deployment-specific data (a pm options
	// block, a workflow name, a goal{...}), surfaced read-only to a spawned
	// child's loop by agent.config() with secret references rendered opaque.
	Extras map[string]any `yaml:"extras"`
	Budget BudgetConfig   `yaml:"budget"`
}

type BudgetConfig struct {
	TokensPerDay int64 `yaml:"tokens_per_day"`
	// Window is an optional rolling-window duration (e.g. "168h") for spawn
	// profile budgets. When set, usage drains continuously as commits age out
	// instead of resetting daily.
	Window string `yaml:"window"`
}

// AgentConfig can be a string profile reference or an inline profile.
// YAML: `memory: conversational` or `memory: { stores: ... }`.
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
	Default   string                      `yaml:"default"`
	Write     string                      `yaml:"write"`
	Forbidden []string                    `yaml:"forbidden"`
	Overrides map[string]ToolSpecOverride `yaml:"overrides"`
}

// ToolSpecOverride uses pointers so "not set" is distinct from explicit false/0.
type ToolSpecOverride struct {
	// Description replaces the tool's registered description verbatim, either
	// as a literal string or as a reference to a prompts: registry key.
	Description *PromptString `yaml:"description"`
	// Params shallow-merges into the schema's parameters.properties: refine one
	// param's description without re-declaring the whole schema. A param the
	// schema does not declare is skipped with a boot warning, never an error.
	Params map[string]ToolParamOverride `yaml:"params"`

	NeedsConfirm *bool   `yaml:"needs_confirm"`
	Permission   *string `yaml:"permission"`
	CostLevel    *int    `yaml:"cost_level"`
	UserVisible  *bool   `yaml:"user_visible"`
	Autonomous   *bool   `yaml:"autonomous"`
}

// ToolParamOverride overrides the presentation of one parameter. Only the
// description is overridable — types and constraints stay owned by the tool's
// registered Go schema.
type ToolParamOverride struct {
	Description PromptString `yaml:"description"`
}

// PromptString is a string that may instead name a prompts: registry key.
// YAML: "literal text" or {prompt: <key>}. The zero value is "unset" for
// value-typed fields (an empty literal and an absent field are the same, as
// they were for a plain string).
type PromptString struct {
	Value string
	IsRef bool // decoded from the {prompt: <key>} form
}

// UnmarshalYAML accepts a scalar string or a mapping with exactly the key
// "prompt".
func (p *PromptString) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		p.Value = node.Value
		return nil
	case yaml.MappingNode:
		var m map[string]string
		if err := node.Decode(&m); err != nil {
			return err
		}
		key, ok := m["prompt"]
		if !ok || len(m) != 1 {
			return fmt.Errorf("description: expected a string or {prompt: <key>}")
		}
		p.Value = key
		p.IsRef = true
		return nil
	default:
		return fmt.Errorf("description: must be a string or {prompt: <key>}")
	}
}

// Resolve returns the text this value stands for given the resolved prompt
// registry: the literal value, or the registry text when it is a reference.
// ok is false when a reference names a key the registry does not define.
func (p PromptString) Resolve(prompts map[string]string) (string, bool) {
	if !p.IsRef {
		return p.Value, true
	}
	v, ok := prompts[p.Value]
	return v, ok
}

// PromptTexts returns the resolved prompt content keyed by prompt name — the
// map agent.config() surfaces and tool overrides resolve against.
func (c *Config) PromptTexts() map[string]string {
	if len(c.Prompts) == 0 {
		return nil
	}
	out := make(map[string]string, len(c.Prompts))
	for name, p := range c.Prompts {
		out[name] = p.Content
	}
	return out
}

// PromptFiles returns the file backing each `file:`-sourced prompt, keyed by
// prompt name — what the reload watcher polls. inline:/text: entries have no
// file and are absent; the key set of the prompts: block itself is not live,
// so adding or re-pointing a key still needs a restart.
func (c *Config) PromptFiles() map[string]string {
	out := map[string]string{}
	for name, p := range c.Prompts {
		if p.File != "" {
			out[name] = p.File
		}
	}
	return out
}

// Search configures the builtin:web_search tool: a set of named engines and
// the default used when a call omits `engine`. With no engines configured the
// tool reports honest-unavailable rather than failing. Engine names double as
// providers (doubao, ollama, stackoverflow, github, youtube); keys are
// env-interpolated (${VAR}) like model keys so secrets never sit in the file
// as literals.
type Search struct {
	Default string                  `yaml:"default"` // engine used when a call omits `engine`
	Engines map[string]SearchEngine `yaml:"engines"`
}

// SearchEngine is one backend's connection config. BaseURL overrides the
// driver's default endpoint (testing / proxy); Timeout bounds each request.
type SearchEngine struct {
	APIKey  string `yaml:"api_key"`  // doubao, ollama, google_search, x_search: required; stackoverflow, github: optional (lifts the anonymous quota)
	BaseURL string `yaml:"base_url"` // optional endpoint override
	Timeout string `yaml:"timeout"`  // per-request bound; default 30s
	Site    string `yaml:"site"`     // stackoverflow engine: any StackExchange site (default "stackoverflow")
	Model   string `yaml:"model"`    // google_search (default gemini-3.8-flash), x_search (default grok-4.6): grounding model
}

// TimeoutD parses Timeout with a sane default.
func (e SearchEngine) TimeoutD() time.Duration {
	if e.Timeout == "" {
		return 30 * time.Second
	}
	d, err := time.ParseDuration(e.Timeout)
	if err != nil {
		return 30 * time.Second
	}
	return d
}

// LegalSearch configures the builtin:legal_search / builtin:legal_read tools:
// a set of named legal-database engines and the default used when a call omits
// `engine`. Supported: hklii (Hong Kong Legal Information Institute) and npc
// (China National Database of Laws and Regulations); neither needs a key. With
// no engines configured the tools report honest-unavailable rather than
// failing. Engine connection config reuses SearchEngine (base_url / timeout).
type LegalSearch struct {
	Default string                  `yaml:"default"` // engine used when a call omits `engine`
	Engines map[string]SearchEngine `yaml:"engines"`
}

// Browser configures the builtin:browser tool: Cloudflare Browser Run
// (formerly Browser Rendering) Quick Actions, which read a page through a real
// headless browser so JavaScript-rendered content is present. With no account
// configured the tool reports honest-unavailable rather than failing.
//
// AccountID and APIToken are set together or not at all — validation rejects a
// half-configured block, because a missing account id is a mistake the
// operator can fix now. APIToken is not validated for resolvability: it may be
// a lazy secret reference whose value only exists at runtime, and an
// unresolvable credential skips the capability with a warning rather than
// failing the boot, the same rule the search engines and memory backends
// follow. The token needs the "Browser Rendering - Edit" permission.
type Browser struct {
	AccountID string `yaml:"account_id"` // Cloudflare account id; not a secret
	APIToken  string `yaml:"api_token"`  // env-interpolated (${VAR}) or cred:<service>
	BaseURL   string `yaml:"base_url"`   // optional endpoint override (testing / proxy)
	Timeout   string `yaml:"timeout"`    // per-request bound; default 60s
}

// TimeoutD parses Timeout with a sane default.
//
// The default is 60s rather than the search engines' 30s because a page load
// may consume the full 60s gotoOptions.timeout before the action itself runs,
// so a 30s client bound would cancel work Cloudflare is still doing and report
// a timeout where the request would have succeeded.
func (b Browser) TimeoutD() time.Duration {
	if b.Timeout == "" {
		return 60 * time.Second
	}
	d, err := time.ParseDuration(b.Timeout)
	if err != nil {
		return 60 * time.Second
	}
	return d
}

// NetConfig is the engine's outbound-connection safety policy. It governs both
// outbound HTTP paths — the builtin:fetch tool and the Lua http.request op —
// from one setting, so a deployment cannot end up with one guarded and the
// other not.
type NetConfig struct {
	HTTP NetHTTP `yaml:"http"`
}

// NetHTTP is the policy for HTTP(S) connections.
//
// AllowPrivate disables the private-address guard. With it on, these paths may
// reach loopback (including the engine's own admin server), RFC1918, CGNAT,
// link-local — which includes the cloud metadata endpoint at 169.254.169.254 —
// unique-local, and the reserved/special-purpose ranges.
//
// It is false by default because the guard is the only thing between a
// prompt-injected agent and the host's own network. It exists so a deployment
// that legitimately calls an internal service can keep working, and enabling
// it logs a WARN at boot so it cannot be turned on silently.
type NetHTTP struct {
	AllowPrivate bool `yaml:"allow_private"`
}

// Plugins controls the global capability ceiling.
type Plugins struct {
	Dir               string   `yaml:"dir"`
	AllowCapabilities []string `yaml:"allow_capabilities"`

	// EnforceCapabilities makes the declared capability lists binding at
	// runtime, defaulting to true when unset.
	//
	// Before this existed the lists were validated at boot and then ignored:
	// every agent was handed llm.chat, store.*, tools.*, shell.*, http.* and
	// mail.* whatever it declared, so omitting net.http or net.mail changed
	// nothing. Set this false only to keep a configuration running that relied
	// on that — an agent then regains the ops its capabilities do not mention,
	// and the boot warning stops. Prefer declaring the capability.
	EnforceCapabilities *bool `yaml:"enforce_capabilities"`
}

// EnforceCaps reports whether declared capabilities gate ops at runtime
// (default true).
func (p Plugins) EnforceCaps() bool {
	return p.EnforceCapabilities == nil || *p.EnforceCapabilities
}

// Agent is a configured agent.
type Agent struct {
	Loop          string            `yaml:"loop"`
	Model         string            `yaml:"model"`
	Instructions  InstructionsRef   `yaml:"instructions"`
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
	// Credentials is the allow-list of engine-wide credential names this
	// agent may fetch with credential.get(). Empty or missing = no access.
	Credentials []string `yaml:"credentials"`
	// Extras carries deployment-specific profile data (a pm options block, a
	// workflow name, a goal{type, success_signal, max_turns, on_goal_met}...)
	// surfaced read-only to the loop via agent.config(). Secret references
	// render as opaque markers there — resolved values never appear.
	Extras map[string]any `yaml:"extras"`
}

type Gateway struct {
	Route     string    `yaml:"route"`
	Listen    string    `yaml:"listen"`     // the single shared HTTP listener (default :8080)
	PublicURL string    `yaml:"public_url"` // external base, e.g. https://agentflow.example.com
	Channels  []Channel `yaml:"channels"`
}

type Channel struct {
	Name   string `yaml:"name"`
	Type   string `yaml:"type"` // webhook | telegram | ghhook
	Mode   string `yaml:"mode"` // telegram: polling (default) | webhook | auto
	Agent  string `yaml:"agent"`
	Path   string `yaml:"path"` // route mounted on the shared server, e.g. /webhook/<chan>/<uuid>/
	Token  string `yaml:"token"`
	Secret string `yaml:"secret"` // ghhook: webhook secret for HMAC verification (env-interpolated)
	// SecretToken is the telegram webhook secret (env-interpolated or cred:
	// reference, resolved like Token). Telegram presents it as the
	// X-Telegram-Bot-Api-Secret-Token header on every delivery. Empty with
	// webhook/auto mode generates a random per-boot one.
	SecretToken string       `yaml:"secret_token"`
	AllowUsers  []int64      `yaml:"allow_users"`
	Media       ChannelMedia `yaml:"media"` // inbound media policy; absent = media disabled
	// Timeout is the webhook sync reply wait (default "55s"). Multi-agent
	// pipelines that outrun it should raise this or use async mode.
	Timeout string `yaml:"timeout"`
	// Async switches a webhook channel to fire-and-poll: the POST returns
	// 202 + a job id immediately; the reply is collected via
	// GET <path>result/<id> or POSTed to a caller-supplied callback_url.
	Async bool `yaml:"async"`
}

// ChannelMedia gates inbound media on a channel. Media is opt-in: with no
// media block (or an empty allow list) attachments are dropped and only the
// text/caption flows. max_bytes defaults to 8 MiB when allow is set.
type ChannelMedia struct {
	MaxBytes int      `yaml:"max_bytes"`
	Allow    []string `yaml:"allow"` // MIME patterns: image/*, application/pdf, audio/mpeg, ...
}

// DefaultCapabilities is what an agent gets if capabilities are omitted.
var DefaultCapabilities = []string{"llm.chat", "memory", "tools", "agent.send", "net.http"}

// memoryProviderKnown reports whether name is a supported memory backend
// provider, for the hint that catches the retired "builtin:" spelling.
func memoryProviderKnown(name string) bool {
	switch name {
	case "sqlite", "redis", "mongodb", "postgres", "pgvector", "qdrant", "redisvector", "volatile":
		return true
	}
	return false
}

// DefaultMemoryProfile is the expansion of `memory: conversational`.
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
		Write:  []string{"plugin:routing_table"},
		Recall: "plugin:recency",
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
	if err := decodeStrict(b, &c, path); err != nil {
		return nil, err
	}
	if err := validate(path, &c); err != nil {
		return nil, err
	}
	// Prompt file: paths resolve relative to the config file (a bare relative
	// path is joined with the file's directory), then the text is read. Direct
	// instructions: paths keep their existing CWD-relative behavior.
	rebasePromptFiles(filepath.Dir(path), &c)
	if err := resolvePrompts(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// rebasePromptFiles makes base the resolution base for relative prompt file:
// paths, leaving absolute paths untouched.
func rebasePromptFiles(base string, c *Config) {
	for name, p := range c.Prompts {
		if p.File != "" && !filepath.IsAbs(p.File) {
			p.File = filepath.Join(base, p.File)
		}
		c.Prompts[name] = p
	}
}

// resolvePrompts fills each registry entry's Content with its resolved text:
// the literal inline/text value, or the contents of the file: path (already
// rebased against the config base). An unreadable file is a boot error, so a
// missing prompt never degrades into a silently empty system prompt.
func resolvePrompts(c *Config) error {
	for name, p := range c.Prompts {
		switch {
		case p.File != "":
			b, err := os.ReadFile(p.File)
			if err != nil {
				return fmt.Errorf("prompt %q: read %s: %w", name, p.File, err)
			}
			p.Content = string(b)
		case p.Inline != "":
			p.Content = p.Inline
		case p.Text != "":
			p.Content = p.Text
		}
		c.Prompts[name] = p
	}
	return nil
}

// decodeStrict decodes one YAML document with KnownFields validation after
// environment expansion. Used by the single-file path.
func decodeStrict(data []byte, v any, label string) error {
	dec := yaml.NewDecoder(bytes.NewReader(expandEnv(data)))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("parse %s: %w", label, err)
	}
	return nil
}

// decodeStrictRaw decodes one YAML document with KnownFields validation but
// NO environment expansion: secret fields keep their raw reference (${VAR} /
// cred:<service>) for lazy resolution, and the configdir loader expands the
// non-secret fields structurally after the merge.
func decodeStrictRaw(data []byte, v any, label string) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("parse %s: %w", label, err)
	}
	return nil
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
	if off := c.Runtime.TimezoneOffsetHours; off < -12 || off > 14 {
		return fmt.Errorf("%s: runtime.timezone_offset_hours %g is outside the real-world range -12..14", path, off)
	}
	if c.Runtime.Credentials.Enabled && c.CredentialsMasterKeyEnv() == "" {
		return fmt.Errorf("%s: runtime.credentials.enabled requires master_key_env to name the env var holding the master key", path)
	}

	// Prompt registry: each entry names exactly one source, and every reference
	// to a prompt key (instructions, tool description overrides) must resolve.
	for name, p := range c.Prompts {
		n := 0
		if p.File != "" {
			n++
		}
		if p.Inline != "" {
			n++
		}
		if p.Text != "" {
			n++
		}
		if n == 0 {
			return fmt.Errorf("%s: prompt %q has no file, inline, or text", path, name)
		}
		if n > 1 {
			return fmt.Errorf("%s: prompt %q sets more than one of file/inline/text; pick one", path, name)
		}
	}
	if err := validatePromptRefs(path, c); err != nil {
		return err
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
		allowedCaps["net.mail"] = true
		allowedCaps["files"] = true
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
			if a.Memory.Profile == "conversational" {
				continue
			}
			// Resolve the store set: a named profiles.memory reference or an
			// inline profile. Either way, every store's backend must exist.
			stores := a.Memory.Inline.Stores
			if !a.Memory.IsInline {
				mp, ok := c.Profiles.Memory[a.Memory.Profile]
				if !ok {
					return fmt.Errorf("%s: agent %q references unknown memory profile %q", path, name, a.Memory.Profile)
				}
				stores = mp.Stores
			}
			for sname, store := range stores {
				if _, ok := c.Memory.Backends[store.Backend]; !ok {
					return fmt.Errorf("%s: agent %q store %q references unknown backend %q", path, name, sname, store.Backend)
				}
				switch store.Scope {
				case "", "user", "agent":
				default:
					return fmt.Errorf("%s: agent %q store %q: unsupported scope %q (want user or agent)", path, name, sname, store.Scope)
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

	// The shared HTTP listener: one port for every HTTP channel now. Default
	// to :8080 when unset; operators typically bind loopback behind a proxy.
	if c.Gateway.Listen == "" {
		c.Gateway.Listen = ":8080"
	}
	if c.Gateway.PublicURL != "" {
		if !strings.HasPrefix(c.Gateway.PublicURL, "http://") && !strings.HasPrefix(c.Gateway.PublicURL, "https://") {
			return fmt.Errorf("%s: gateway.public_url must start with http:// or https:// (got %q)", path, c.Gateway.PublicURL)
		}
	}

	seenPaths := map[string]string{} // path -> channel name, for collision detection
	for i, ch := range c.Gateway.Channels {
		if ch.Agent == "" {
			return fmt.Errorf("%s: channel #%d (%s) has no agent", path, i, ch.Type)
		}
		if _, ok := c.Agents[ch.Agent]; !ok {
			return fmt.Errorf("%s: channel %q references unknown agent %q", path, ch.Name, ch.Agent)
		}
		switch ch.Type {
		case "webhook":
			// path defaults are applied by the driver; here we only require that
			// any explicit path ends in "/" so the subtree match covers the UUID.
			if ch.Path != "" && !strings.HasSuffix(ch.Path, "/") {
				return fmt.Errorf("%s: webhook channel %q path must end with / (got %q)", path, ch.Name, ch.Path)
			}
			if ch.Timeout != "" {
				d, err := time.ParseDuration(ch.Timeout)
				if err != nil || d <= 0 {
					return fmt.Errorf("%s: webhook channel %q has invalid timeout %q (use e.g. \"120s\")", path, ch.Name, ch.Timeout)
				}
			}
		case "ghhook":
			if ch.Path != "" && !strings.HasSuffix(ch.Path, "/") {
				return fmt.Errorf("%s: ghhook channel %q path must end with / (got %q)", path, ch.Name, ch.Path)
			}
		case "telegram":
			// Token presence is not validated here: the token may be a lazy
			// secret reference (${VAR} / cred:<service>) resolved at channel
			// construction, and an unresolvable one skips the channel with a
			// warning rather than failing the boot.
			if ch.Mode == "" {
				ch.Mode = "polling"
			}
			if ch.Mode != "polling" && ch.Mode != "webhook" && ch.Mode != "auto" {
				return fmt.Errorf("%s: telegram channel %q has unsupported mode %q (use polling, webhook, or auto)", path, ch.Name, ch.Mode)
			}
			if ch.Mode == "webhook" && c.Gateway.PublicURL == "" {
				return fmt.Errorf("%s: telegram channel %q webhook mode requires gateway.public_url", path, ch.Name)
			}
			if ch.Path != "" && !strings.HasSuffix(ch.Path, "/") {
				return fmt.Errorf("%s: telegram channel %q path must end with / (got %q)", path, ch.Name, ch.Path)
			}
		default:
			return fmt.Errorf("%s: unsupported channel type %q (channel %q)", path, ch.Type, ch.Name)
		}
		// media policy sanity: max_bytes must be positive when set; an allow
		// list with no patterns is the same as media disabled (allowed, but
		// flag obvious mistakes like a negative ceiling).
		if ch.Media.MaxBytes < 0 {
			return fmt.Errorf("%s: channel %q media.max_bytes must be >= 0", path, ch.Name)
		}
		// path collision check — the shared mux panics on dupes, but a config
		// error is friendlier and names both colliding channels.
		key := ch.Path
		if key != "" {
			if prev, dup := seenPaths[key]; dup {
				return fmt.Errorf("%s: channels %q and %q mount the same path %q", path, prev, ch.Name, key)
			}
			seenPaths[key] = ch.Name
		}
	}

	for name, m := range c.Models {
		switch m.Provider {
		case "anthropic", "openai", "openai-responses", "gemini", "rerank":
		default:
			return fmt.Errorf("%s: model %q has unsupported provider %q", path, name, m.Provider)
		}
		// Thinking is the one enum the llm layer also validates per call (hot
		// runtime edits bypass this boot check); both keep the same vocabulary.
		switch strings.ToLower(strings.TrimSpace(m.Thinking)) {
		case "", "off", "low", "medium", "high", "xhigh", "max":
		default:
			return fmt.Errorf("%s: model %q has unsupported thinking level %q (want off|low|medium|high|xhigh|max)", path, name, m.Thinking)
		}
	}

	// Search engines are optional; none configured leaves web_search in
	// honest-unavailable mode. Engine names are the provider selectors. A key
	// is NOT required at validation: it may be a lazy secret reference and an
	// unresolvable credential skips the engine at construction with a warning
	// (uniform honest-degradation), never a boot failure.
	for ename := range c.Search.Engines {
		switch ename {
		case "doubao", "ollama", "youtube", "google_search", "x_search", "stackoverflow", "github":
		default:
			return fmt.Errorf("%s: unsupported search engine %q (want doubao, ollama, google_search, x_search, stackoverflow, github, or youtube)", path, ename)
		}
	}
	if n := len(c.Search.Engines); n > 0 {
		if c.Search.Default == "" {
			if n > 1 {
				return fmt.Errorf("%s: search has %d engines; set search.default to the one used when a call omits `engine`", path, n)
			}
			for ename := range c.Search.Engines {
				c.Search.Default = ename
			}
		} else if _, ok := c.Search.Engines[c.Search.Default]; !ok {
			return fmt.Errorf("%s: search.default %q is not a configured engine", path, c.Search.Default)
		}
	}

	// Legal-search engines are optional; none configured leaves legal_search /
	// legal_read in honest-unavailable mode. hklii and npc need no key.
	for ename := range c.Legal.Engines {
		switch ename {
		case "hklii", "npc":
		default:
			return fmt.Errorf("%s: unsupported legal_search engine %q (want hklii or npc)", path, ename)
		}
	}
	if n := len(c.Legal.Engines); n > 0 {
		if c.Legal.Default == "" {
			if n > 1 {
				return fmt.Errorf("%s: legal_search has %d engines; set legal_search.default to the one used when a call omits `engine`", path, n)
			}
			for ename := range c.Legal.Engines {
				c.Legal.Default = ename
			}
		} else if _, ok := c.Legal.Engines[c.Legal.Default]; !ok {
			return fmt.Errorf("%s: legal_search.default %q is not a configured engine", path, c.Legal.Default)
		}
	}

	// The browser tool is optional; no account configured leaves builtin:browser
	// in honest-unavailable mode. account_id and api_token are set together or
	// not at all: account_id is not a secret, so one present without the other
	// is a mistake worth failing the boot over. The token's *resolvability* is
	// deliberately not checked — it may be a lazy secret reference, and an
	// unresolvable credential skips the capability at construction with a
	// warning rather than failing the boot, as everywhere else.
	if c.Browser.AccountID != "" && c.Browser.APIToken == "" {
		return fmt.Errorf("%s: browser.account_id is set but browser.api_token is empty", path)
	}
	if c.Browser.AccountID == "" && c.Browser.APIToken != "" {
		return fmt.Errorf("%s: browser.api_token is set but browser.account_id is empty", path)
	}

	for bname, b := range c.Memory.Backends {
		switch b.Provider {
		case "sqlite", "redis", "mongodb", "postgres", "pgvector", "qdrant", "redisvector", "volatile":
		default:
			// The prefix used to be required. Saying so beats "unsupported
			// provider", which reads as if the backend were unknown when it is
			// only misspelled.
			if name, ok := strings.CutPrefix(b.Provider, "builtin:"); ok && memoryProviderKnown(name) {
				return fmt.Errorf("%s: memory backend %q has provider %q — providers are unprefixed now, write %q",
					path, bname, b.Provider, name)
			}
			return fmt.Errorf("%s: memory backend %q has unsupported provider %q", path, bname, b.Provider)
		}
	}

	// "conversational" is the one built-in memory profile, and it is resolved
	// before profiles.memory is consulted — so a user profile of that name
	// would never run. Refusing the name is better than silently ignoring it.
	if _, clash := c.Profiles.Memory["conversational"]; clash {
		return fmt.Errorf("%s: profiles.memory defines %q, which is reserved for the built-in profile", path, "conversational")
	}

	// Media blob store: fs (default) or s3. S3 requires bucket and region;
	// credentials may be lazy references and an unresolvable one skips the
	// store at boot with a warning, never a boot failure.
	switch c.Media.Backend {
	case "", "fs":
	case "s3":
		if c.Media.S3.Bucket == "" || c.Media.S3.Region == "" {
			return fmt.Errorf("%s: media backend s3 requires s3.bucket and s3.region", path)
		}
	default:
		return fmt.Errorf("%s: unsupported media backend %q (want fs or s3)", path, c.Media.Backend)
	}

	// File store: same backend shape as media. S3 requires bucket and region;
	// credentials may be lazy references and an unresolvable one skips the
	// file store at boot with a warning, never a boot failure (agents without
	// the files capability never touch it).
	switch c.Files.Backend {
	case "", "fs":
	case "s3":
		if c.Files.S3.Bucket == "" || c.Files.S3.Region == "" {
			return fmt.Errorf("%s: files backend s3 requires s3.bucket and s3.region", path)
		}
	default:
		return fmt.Errorf("%s: unsupported files backend %q (want fs or s3)", path, c.Files.Backend)
	}
	if c.Files.MaxFileBytes < 0 {
		return fmt.Errorf("%s: files.max_file_bytes must be >= 0", path)
	}
	if c.Files.ScratchTTL != "" {
		if _, err := time.ParseDuration(c.Files.ScratchTTL); err != nil {
			return fmt.Errorf("%s: files.scratch_ttl: %v", path, err)
		}
	}
	if c.Files.GCGrace != "" {
		if _, err := time.ParseDuration(c.Files.GCGrace); err != nil {
			return fmt.Errorf("%s: files.gc_grace: %v", path, err)
		}
	}

	if c.Audit.RetentionDays != nil && *c.Audit.RetentionDays < 0 {
		return fmt.Errorf("%s: audit.retention_days must be >= 0 (0 = keep forever)", path)
	}

	for sname, s := range c.MCP.Servers {
		if s.Command == "" && s.URL == "" {
			return fmt.Errorf("%s: mcp server %q has neither command nor url", path, sname)
		}
	}

	return nil
}

// validatePromptRefs checks every reference into the prompts registry: agent
// and spawn-profile instructions, and tool description overrides. A reference
// to a key that is not defined is a boot error, never a silent empty prompt.
func validatePromptRefs(path string, c *Config) error {
	has := func(key string) bool {
		_, ok := c.Prompts[key]
		return ok
	}
	for name, a := range c.Agents {
		if a.Instructions.Prompt != "" && !has(a.Instructions.Prompt) {
			return fmt.Errorf("%s: agent %q instructions references unknown prompt %q", path, name, a.Instructions.Prompt)
		}
	}
	for pname, p := range c.Profiles.Agent {
		if p.Instructions.Prompt != "" && !has(p.Instructions.Prompt) {
			return fmt.Errorf("%s: spawn profile %q instructions references unknown prompt %q", path, pname, p.Instructions.Prompt)
		}
	}
	for tname, o := range c.Tools.Policy.Overrides {
		if o.Description != nil && o.Description.IsRef && !has(o.Description.Value) {
			return fmt.Errorf("%s: tool override %q description references unknown prompt %q", path, tname, o.Description.Value)
		}
		for pname, po := range o.Params {
			if po.Description.IsRef && !has(po.Description.Value) {
				return fmt.Errorf("%s: tool override %q param %q description references unknown prompt %q", path, tname, pname, po.Description.Value)
			}
		}
	}
	return nil
}

// validateShellProfile checks provider-specific requirements for a shell
// profile referenced by an agent or spawn profile. SSH requires a host; docker
// needs nothing (image defaults to alpine:3.20).
func validateShellProfile(path, owner, name string, p ShellProfile) error {
	switch p.Provider {
	case "ssh":
		if p.Host == "" {
			return fmt.Errorf("%s: shell profile %q (used by %q) is provider ssh but missing required host", path, name, owner)
		}
	case "docker", "":
		// docker is persistent; image defaults to alpine:3.20 if unset. No reqs.
	default:
		return fmt.Errorf("%s: shell profile %q (used by %q) has unknown provider %q", path, name, owner, p.Provider)
	}
	return nil
}

// ResolveMemoryProfile returns the concrete memory profile for an agent.
// It expands `conversational`, then named profiles.memory entries,
// then inline profiles.
func (c *Config) ResolveMemoryProfile(a Agent) MemoryProfile {
	if a.Memory.Profile == "conversational" {
		// Ensure the default backend is present if not already defined.
		mp := DefaultMemoryProfile()
		if c.Memory.Backends == nil {
			c.Memory.Backends = map[string]Backend{}
		}
		if _, ok := c.Memory.Backends["main_db"]; !ok {
			c.Memory.Backends["main_db"] = Backend{
				Provider: "sqlite",
				Config: map[string]any{
					"path": "./data/agentflow.db",
				},
			}
		}
		return mp
	}
	if a.Memory.Profile != "" {
		if mp, ok := c.Profiles.Memory[a.Memory.Profile]; ok {
			return mp
		}
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
