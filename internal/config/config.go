// Package config loads agentflow.yaml (phase-2 schema: memory backends, tools,
// MCP, runtime persistence, agent capabilities/skills/memory).
//
// The file is environment-expanded (${VAR}, ${VAR:-default}) before parsing,
// and parsed strictly: an unknown key is a boot error. The config reference is
// the Configuration page in the AgentFlow Studio documentation.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"agentflow/internal/core/memory/locality"
	"agentflow/internal/core/safety"
	"agentflow/internal/core/storedb"
)

type Config struct {
	Version string `yaml:"version"`
	// Epoch identifies the configuration this process loaded: a hash of the
	// fragments as written, before ${VAR} expansion, so rotating a secret does
	// not move it while any change of shape does. Two instances can compare it
	// to tell whether they are running the same deployment.
	Epoch    string           `yaml:"-"`
	Runtime  Runtime          `yaml:"runtime"`
	Models   map[string]Model `yaml:"models"`
	Memory   Memory           `yaml:"memory"`
	Profiles Profiles         `yaml:"profiles"`
	Safety   SafetyConfig     `yaml:"safety"`
	Gateway  Gateway          `yaml:"gateway"`
	MCP      MCP              `yaml:"mcp"`
	Tools    Tools            `yaml:"tools"`
	Search   Search           `yaml:"search"`
	Legal    LegalSearch      `yaml:"legal_search"`
	Browser  Browser          `yaml:"browser"`
	Net      NetConfig        `yaml:"net"`
	Media    MediaConfig         `yaml:"media"`
	Files    FilesConfig         `yaml:"files"`
	Audit    AuditConfig         `yaml:"audit"`
	Usage    UsageConfig         `yaml:"usage"`
	Observability ObservabilityConfig `yaml:"observability"`
	Agents   map[string]Agent    `yaml:"agents"`
	// Environments are the declared toolchains shell jobs run in: a name, a
	// pinned image or a local root filesystem, and variables. A shell profile
	// picks one; a project may pick another.
	Environments map[string]Environment `yaml:"environments"`
	Triggers     []Trigger              `yaml:"triggers"` // configdir layout; declarative task data for loops
	Prompts      map[string]Prompt      `yaml:"prompts"`
	Plugins      Plugins                `yaml:"plugins"`
	// Packages names deployable Lua source packages, each a directory with an
	// optional manifest.yaml and *.lua files. A loop imports one with
	// "-- af-import: <name>" (or is set to "package:<name>" directly). Package
	// code is version-gated and concatenated before the importing chunk.
	Packages map[string]string `yaml:"packages"`
	// APIs declares external API extension packages (TypeScript/Goja or Wasm/wazero).
	APIs map[string]APIConfig `yaml:"apis,omitempty"`
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

// MediaConfig selects the centralized blob-store backend for media (and, by
// default, for files when files.* sets no separate store). Handles
// ("media:<sha256>") are backend-agnostic, so loops and the journal never see
// where bytes live. Default: fs rooted beside the runtime db.
type MediaConfig struct {
	Backend    string       `yaml:"backend"` // fs (default) | s3
	Dir        string       `yaml:"dir"`     // fs root; default <runtime-persistence-dir>/media
	S3         MediaS3      `yaml:"s3"`
	TempURL    MediaTempURL `yaml:"temp_url"`    // optional HMAC-signed HTTP endpoint for serving fs blobs to URL-only channels (e.g. QQ)
	Retention  string       `yaml:"retention"`   // media grant lifetime; default 168h (7d); "0" = forever
	DerivedTTL string       `yaml:"derived_ttl"` // cached transcoded variant lifetime; default 24h
	GCGrace    string       `yaml:"gc_grace"`    // blob GC grace window; default 24h; "0" disables GC
}

// MediaTempURL configures the optional HMAC-signed temporary HTTP endpoint
// that lets URL-only channels (such as QQ) fetch media stored on local disk
// without S3. Requires gateway.public_url to be set.
type MediaTempURL struct {
	Enabled bool   `yaml:"enabled"` // opt-in; disabled by default
	Path    string `yaml:"path"`    // HTTP route prefix; default /media/t/
	Secret  string `yaml:"secret"`  // HMAC signing key (secret registry); empty in single-node mints an ephemeral per-boot key
	TTL     string `yaml:"ttl"`     // signed URL lifetime; default 10m
}

// MediaTempURLPath returns the mounted route prefix for temporary media URLs.
func (m MediaTempURL) MediaTempURLPath() string {
	if m.Path != "" {
		return m.Path
	}
	return "/media/t/"
}

// MediaTempURLTTL returns the signed URL lifetime (default 10m).
func (m MediaTempURL) MediaTempURLTTL() time.Duration {
	if m.TTL == "" {
		return 10 * time.Minute
	}
	d, _ := time.ParseDuration(m.TTL)
	return d
}

// MediaRetention returns the default media grant retention (default 168h / 7d;
// "0" disables expiry).
func (m MediaConfig) MediaRetention() time.Duration {
	if m.Retention == "" {
		return 7 * 24 * time.Hour
	}
	d, _ := time.ParseDuration(m.Retention)
	return d
}

// MediaDerivedTTL returns the TTL for cached transcoded variants (default 24h).
func (m MediaConfig) MediaDerivedTTL() time.Duration {
	if m.DerivedTTL == "" {
		return 24 * time.Hour
	}
	d, _ := time.ParseDuration(m.DerivedTTL)
	return d
}

// MediaGCGrace returns the blob GC grace window for media (default 24h; "0"
// disables GC).
func (m MediaConfig) MediaGCGrace() time.Duration {
	if m.GCGrace == "" {
		return 24 * time.Hour
	}
	d, _ := time.ParseDuration(m.GCGrace)
	return d
}

// MediaS3 configures the S3 blob-store backend. Keys are env-interpolated
// (${VAR}) like model keys so secrets never sit in the file as literals.
type MediaS3 struct {
	Bucket         string `yaml:"bucket"`
	Region         string `yaml:"region"`
	Endpoint       string `yaml:"endpoint"`        // optional custom S3-compatible host (MinIO, R2, ...); empty = AWS
	PublicEndpoint string `yaml:"public_endpoint"` // optional public base URL for presigned GETs when endpoint is internal
	Prefix         string `yaml:"prefix"`          // optional object key prefix
	AccessKey      string `yaml:"access_key"`      // env-interpolated
	SecretKey      string `yaml:"secret_key"`      // env-interpolated
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

// HasExplicitStore reports whether files configures its own separate blob store
// rather than sharing the unified media blob store.
func (f FilesConfig) HasExplicitStore() bool {
	return f.Backend != "" || f.Dir != "" || f.S3.Bucket != "" || f.S3.Endpoint != ""
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

// UsageConfig controls the per-user token ledger: the daily rollup every quota
// check and accounting view reads, plus an optional per-call detail log.
type UsageConfig struct {
	// Events writes one row per metered call in addition to the daily rollup.
	// Off by default: the rollup answers quota and dashboard questions, and the
	// event log is the high-volume table.
	Events *bool `yaml:"events"`
	// RetentionDays prunes ledger rows older than this many days (default 30;
	// 0 = keep forever).
	RetentionDays *int `yaml:"retention_days"`
	// DefaultTokensPerDay is the deployment-wide per-user daily token limit
	// (0 = unlimited). A profile's own tokens_per_day overrides it.
	DefaultTokensPerDay int64 `yaml:"default_tokens_per_day"`
}

// UsageEvents reports whether the per-call detail log is enabled (default off).
func (u UsageConfig) UsageEvents() bool { return u.Events != nil && *u.Events }

// UsageRetention returns ledger retention in days (default 30; 0 = forever).
func (u UsageConfig) UsageRetention() int {
	if u.RetentionDays == nil {
		return 30
	}
	return *u.RetentionDays
}

// ObservabilityConfig controls external telemetry export (OTLP/HTTP and
// Langfuse) derived from runtime execution trace events.
type ObservabilityConfig struct {
	Tracing TracingConfig `yaml:"tracing"`
}

// TracingConfig exports turn/op/reply/safety spans asynchronously over
// OTLP/HTTP JSON (including Langfuse's /api/public/otel/v1/traces endpoint).
type TracingConfig struct {
	Enabled         bool              `yaml:"enabled"`
	Provider        string            `yaml:"provider"`         // otlp (default) | langfuse
	Endpoint        string            `yaml:"endpoint"`         // collector base URL or .../v1/traces
	ServiceName     string            `yaml:"service_name"`     // default "agentflow"
	Headers         map[string]string `yaml:"headers"`          // optional HTTP headers (e.g. Authorization)
	PublicKey       string            `yaml:"public_key"`       // Langfuse public key
	SecretKey       string            `yaml:"secret_key"`       // Langfuse secret key
	SampleRate      *float64          `yaml:"sample_rate"`      // [0, 1]; default 1.0 when enabled
	IncludePayloads bool              `yaml:"include_payloads"` // opt-in text capture
	BatchSize       int               `yaml:"batch_size"`       // default 64
	FlushInterval   string            `yaml:"flush_interval"`   // default "5s"
	Timeout         string            `yaml:"timeout"`          // default "10s"
}

// Sample returns the configured sampling fraction in [0, 1] (default 1.0).
func (t TracingConfig) Sample() float64 {
	if t.SampleRate == nil {
		return 1.0
	}
	return *t.SampleRate
}

// Runtime contains instance-wide tuning and persistence.
type Runtime struct {
	// Pointers distinguish "unset" from an explicit zero. vm.memory_limit caps
	// each Luau state and scheduler.workers is the op-pool size (default 8).
	VM struct {
		MemoryLimit *string `yaml:"memory_limit"`
	} `yaml:"vm"`
	Scheduler struct {
		Workers *int `yaml:"workers"`
	} `yaml:"scheduler"`
	// Sandbox is how shell jobs run: the gVisor runtime, the queue and the
	// limits. See SandboxConfig.
	Sandbox SandboxConfig `yaml:"sandbox"`
	// Limits are the per-tenant resource bounds (E18). Zero (the default)
	// disables each bound — they are opt-in per deployment — but a hostile
	// tenant (D14) makes the op-pool fairness unconditional: that one is not
	// configurable, it is how the pool works now.
	Limits struct {
		// SessionsPerTenant caps live sessions per tenant, enforced at the
		// supervisor's spawn boundary (routed first contact and agent.spawn).
		SessionsPerTenant int `yaml:"sessions_per_tenant"`
		// EgressPerMinute caps http.request and mail.send ops per tenant per
		// minute. A fixed window resets each minute.
		EgressPerMinute int `yaml:"egress_per_minute"`
		// EphemeralPerTenant caps concurrent one-turn sessions per tenant. It is
		// separate from sessions_per_tenant — stateless traffic must not consume
		// the conversation slots — and unset means 16, the default the OpenAI
		// channel documents. An explicit 0 disables the bound.
		EphemeralPerTenant *int `yaml:"ephemeral_per_tenant"`
	} `yaml:"limits"`
	// Sessions is the conversation lifecycle. idle_unload unloads a session
	// after that long without activity; its Lua locals are gone after that, so
	// a loop keeps durable state in session.state.* — the documented contract
	// (D10). Unset is 30m; "0" disables unloading.
	Sessions struct {
		IdleUnload string `yaml:"idle_unload"`
	} `yaml:"sessions"`
	Persistence string            `yaml:"persistence"` // e.g. sqlite://./data/agentflow.db
	Admin       AdminConfig       `yaml:"admin"`
	Studio      StudioConfig      `yaml:"studio"`
	Identity    IdentityConfig    `yaml:"identity"`
	Users       UsersConfig       `yaml:"users"`
	Credentials CredentialsConfig `yaml:"credentials"`
	Cluster     ClusterConfig     `yaml:"cluster"`
	LogPlane    LogPlaneConfig    `yaml:"log_plane"`
	// Region names where this instance runs, for a deployment spread over more
	// than one. Empty (the default) means the deployment has no regions:
	// nothing carries a region label and nothing is compared.
	Region string `yaml:"region"`
	// TimezoneOffsetHours is the instance-wide UTC offset applied to cron
	// trigger matching (e.g. 8 for UTC+8, -5.5 for UTC-5:30). 0 = UTC, the
	// default. every: triggers are unaffected: an interval has no wall clock.
	TimezoneOffsetHours float64 `yaml:"timezone_offset_hours"`

	// Topology names the deployment shape this instance is part of: "single"
	// (one process) or "regional" (several instances in one region sharing one
	// PostgreSQL). Empty means single, which is what every deployment before
	// this setting was — so an existing config boots unchanged.
	//
	// It is one setting rather than a dozen because the tier decides which
	// other settings are valid: single accepts the local backends it always
	// did, while regional refuses the ones that would silently give every
	// instance its own copy of the data (a local blob directory, a SQLite
	// memory store, a per-boot admin token). The refusals live in validate;
	// the checks that need the environment (ADMIN_TOKEN) live at boot.
	Topology string `yaml:"topology"`
	// Zone names the availability zone within the region. It is a label for
	// metrics and the instance registry, never a routing decision: a regional
	// cell treats every zone the same, and a session may move between them.
	Zone string `yaml:"zone"`
	// ShutdownGrace is how long this instance reports itself unready and keeps
	// serving before it closes. A regional cell defaults to 5s so a load
	// balancer has time to stop sending new traffic and in-flight turns can
	// finish; single defaults to 0, preserving the immediate shutdown it always
	// had. "0" explicitly disables the wait.
	ShutdownGrace string `yaml:"shutdown_grace"`
}

// TimezoneOffset is the cron matching offset as a duration. It is a fixed
// offset (not a named zone): cron schedules then never shift under daylight
// saving, matching how deployments describe "09:00 local" for a single region.
func (r Runtime) TimezoneOffset() time.Duration {
	return time.Duration(r.TimezoneOffsetHours * float64(time.Hour))
}

// EphemeralCap is the effective per-tenant bound on concurrent ephemeral
// sessions. Unset means 16 — the default the OpenAI channel documents — and an
// explicit 0 disables the bound. The pointer is what keeps those two apart: an
// int alone cannot tell "not configured" from "disabled".
func (r Runtime) EphemeralCap() int {
	if r.Limits.EphemeralPerTenant == nil {
		return 16
	}
	return *r.Limits.EphemeralPerTenant
}

// IdleUnload is how long a conversation may sit idle before the supervisor
// unloads it. Unset means 30m; an explicit 0 disables unloading. The session
// id and its durable state survive — the next message recreates the actor and
// the loop starts from the top.
func (r Runtime) IdleUnload() time.Duration {
	raw := strings.TrimSpace(r.Sessions.IdleUnload)
	if raw == "" {
		return 30 * time.Minute
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// CredentialsConfig controls the encrypted per-tenant credential store. When
// enabled, the http.request op can resolve an `auth={service=...}` reference
// to a stored API key at request time. The master key is read from the named
// environment variable at boot (never from the config file).
type CredentialsConfig struct {
	Enabled      bool   `yaml:"enabled"`
	Persistence  string `yaml:"persistence"`    // sqlite path or postgres DSN; "" = follow runtime.persistence
	MasterKeyEnv string `yaml:"master_key_env"` // env var holding the master key; default "CREDENTIALS_MASTER_KEY"
	// EngineWide controls credential.get(), the raw-secret op. Empty/"allow"
	// preserves the historical behavior (an agent's credentials allow-list is
	// the only gate); "maintenance" restricts it to engine-fired turns, so a
	// tenant turn cannot read an engine-wide secret even if the agent lists it.
	// The normal per-tenant path is auth={service=...} on http.request and
	// tools.http, which never returns a value to Lua.
	EngineWide string `yaml:"engine_wide"`
}

// EngineWideMaintenance reports whether credential.get() is restricted to
// maintenance contexts.
func (c CredentialsConfig) EngineWideMaintenance() bool {
	return strings.EqualFold(strings.TrimSpace(c.EngineWide), "maintenance")
}

// LogPlaneConfig is where the append-only planes live: the message journal and
// the per-call usage detail.
//
// They are the two things the engine writes constantly, reads rarely (an
// operator view) and expires by age, which is what makes them worth keeping
// somewhere other than the transactional store. What must NOT move with them is
// the ledger's daily rollups: a quota check and a budget refresh read those
// before every call, and a value stale by a replication lag would let a
// deployment overspend.
//
// Empty (the default) keeps both planes in the runtime store, which is what
// every deployment had before there was a choice.
type LogPlaneConfig struct {
	// Persistence is a directory, or "file://<directory>". A backend with its
	// own scheme joins it here as one is written; an unrecognised scheme is a
	// boot error rather than a directory named after it.
	Persistence string `yaml:"persistence"`
}

// LogPlaneDir returns the directory the log plane is written to, or "" when the
// runtime store keeps the log itself.
func (c *Config) LogPlaneDir() string {
	p := strings.TrimSpace(c.Runtime.LogPlane.Persistence)
	if p == "" {
		return ""
	}
	t, err := storedb.ParseTarget(p, storedb.BackendFile)
	if err != nil {
		return p // validation refuses it with a message naming the setting
	}
	return t.Address
}

// ClusterConfig tunes session-hub timing in multi-instance deployments.
// Leases and the session inbox always live in runtime.persistence.
type ClusterConfig struct {
	SessionTTL   string `yaml:"session_ttl"`   // Go duration; default 30s
	PollInterval string `yaml:"poll_interval"` // Go duration; default 250ms
}

// ClusterSessionTTL parses session_ttl. Empty or malformed is 0, meaning the
// hub's own default; malformed values fail at validation.
func (c ClusterConfig) ClusterSessionTTL() time.Duration {
	d, _ := time.ParseDuration(c.SessionTTL)
	return d
}

// ClusterPollInterval parses poll_interval, on the same rule.
func (c ClusterConfig) ClusterPollInterval() time.Duration {
	d, _ := time.ParseDuration(c.PollInterval)
	return d
}

// AdminConfig configures the admin plane: health, readiness, metrics, the admin
// API and the console. Its HTTP routes are served on gateway.listen, the
// engine's one HTTP port, under /admin/ plus /healthz, /readyz and /metrics.
type AdminConfig struct {
	// Socket adds a local-only admin listener: a unix domain socket path on
	// Unix, or a named pipe such as \.\pipegentflow-admin on Windows.
	// It is intended for Studio/admin clients on the same machine; the bearer
	// token is still required. Empty disables it.
	Socket string `yaml:"socket"`
	// TCP serves the admin plane on the shared HTTP listener. nil means
	// enabled; set false to serve it only on Socket. At least one transport
	// must remain.
	TCP *bool `yaml:"tcp"`
	// CORSOrigins is the allow-list for browser clients of the admin API. The
	// standalone Studio app is the reason it exists: it runs on its own origin
	// and authenticates with the bearer token, so its origin must be named
	// here. Empty (the default) keeps the admin API same-origin only.
	CORSOrigins []string `yaml:"cors_origins"`
	// Operators adds named admin bearer tokens (name -> token). Every one has
	// the same authority as ADMIN_TOKEN, but actions are attributed to the
	// name in the request's operator header and logs, so a shared break-glass
	// token is no longer the only audit identity. Token values may be env
	// references or cred:<service>; they are resolved at boot.
	Operators map[string]string `yaml:"operators"`
}

// namedPipePrefix is the Windows named pipe namespace.
const namedPipePrefix = `\\.\pipe\`

// TCPEnabled reports whether the admin plane is served on the HTTP listener.
func (c AdminConfig) TCPEnabled() bool { return c.TCP == nil || *c.TCP }

// StudioConfig configures the admin API that backs the standalone Studio app
// (agent builder, playground, trace). The app itself is a separate process;
// these settings size the runtime side.
type StudioConfig struct {
	// Enabled gates /admin/api/studio. nil (absent) means enabled — the API is
	// inert without the admin token, and disabling it only removes the app.
	Enabled *bool `yaml:"enabled"`
	// Workspace is the directory a single-file deployment writes studio loops
	// and prompts under. Empty means <config dir>/studio.
	Workspace string `yaml:"workspace"`
	// TraceEvents bounds each session's in-memory trace ring (default 2000).
	TraceEvents int `yaml:"trace_events"`
	// TraceSessions bounds how many session rings are retained (default 200).
	TraceSessions int `yaml:"trace_sessions"`
	// TracePayloads includes message and reply text in trace events. Off by
	// default: metadata (op, model, duration, tokens, errors) is always kept.
	TracePayloads bool `yaml:"trace_payloads"`
	// ChatTimeout is how long one playground turn may take (default 120s).
	ChatTimeout string `yaml:"chat_timeout"`
}

// IsEnabled reports whether the studio API is on (the default).
func (s StudioConfig) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// Timeout parses ChatTimeout, falling back to 120s. Malformed values are a
// boot error in validate.
func (s StudioConfig) Timeout() time.Duration {
	if s.ChatTimeout == "" {
		return 120 * time.Second
	}
	d, err := time.ParseDuration(s.ChatTimeout)
	if err != nil {
		return 120 * time.Second
	}
	return d
}

// IdentityConfig configures the optional user-identity layer and the policy
// applied to senders whose channel handle has not been linked to a profile.
type IdentityConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Persistence string `yaml:"persistence"` // sqlite path or postgres DSN; "" = follow runtime.persistence
	// RequireRegistration (default true) keeps an unknown channel handle in the
	// shared service stratum — no personal scope, no quota identity — until it
	// is linked to a profile. Set false to restore auto-claiming a profile on
	// first contact.
	RequireRegistration *bool `yaml:"require_registration"`
	// Unregistered selects what happens to a channel handle that is not linked
	// to a profile: "allow" (default) keeps it in the shared service stratum,
	// "reject" drops the inbound and asks the person to link/register.
	Unregistered string `yaml:"unregistered"`
}

// RegistrationRequired reports whether an unknown handle must be linked to a
// profile before it gets any per-user state. Defaults to true.
func (i IdentityConfig) RegistrationRequired() bool {
	return i.RequireRegistration == nil || *i.RequireRegistration
}

// RejectUnregistered reports whether unlinked handles are refused.
func (i IdentityConfig) RejectUnregistered() bool {
	return strings.EqualFold(strings.TrimSpace(i.Unregistered), "reject")
}

// UsersConfig configures the public /v1/users profile API (profile registration
// and channel linking). Requires runtime.identity.enabled.
type UsersConfig struct {
	// Enabled exposes the /v1/users API on the shared httpd listener. Off by
	// default: the surface is public, so opening it is a deliberate act.
	Enabled bool `yaml:"enabled"`
	// Registration selects who may create a profile: "open" (default) or
	// "invite", where an operator issues single-use codes first.
	Registration string `yaml:"registration"`
	// LinkTTL is how long a channel-link challenge stays valid (default 10m).
	LinkTTL string `yaml:"link_ttl"`
	// CORSOrigins is the allow-list of browser origins permitted to call the
	// user API with credentials. Empty (the default) means no CORS headers at
	// all — an external frontend cannot call the API until an operator names
	// its origin. Each entry is a full origin ("https://app.example.com"), no
	// trailing slash.
	CORSOrigins []string `yaml:"cors_origins"`
	// JITProvisioning creates a profile on a verified first login (default
	// true). Set false when every profile must be provisioned by an operator
	// before anyone can sign in.
	JITProvisioning *bool `yaml:"jit_provisioning"`
	// OIDC, when set, makes the engine trust access tokens from an external
	// identity provider. Login — passwords, OAuth, MFA, session lifetimes and
	// revocation — lives there; the engine only verifies what it is handed.
	OIDC *OIDCConfig `yaml:"oidc"`
	// PushSameTenant makes session.push_user refuse a target other than the
	// current turn's tenant. The default (false) preserves deployments whose
	// agents notify users across tenants as an operator service; a multi-tenant
	// chat product should set it true.
	PushSameTenant bool `yaml:"push_same_tenant"`
}

// OIDCConfig describes the identity provider whose tokens this deployment
// accepts.
type OIDCConfig struct {
	// Issuer is the expected `iss` claim, and the base for key discovery when
	// JWKSURL is empty.
	Issuer string `yaml:"issuer"`
	// Audience, when set, must appear in the token's `aud` claim. Setting it is
	// what stops a token minted for another service being replayed here.
	Audience string `yaml:"audience"`
	// JWKSURL overrides OpenID discovery (<issuer>/.well-known/openid-configuration).
	JWKSURL string `yaml:"jwks_url"`
	// Claim names carrying the profile fields; empty means sub/email/name.
	SubjectClaim string `yaml:"subject_claim"`
	EmailClaim   string `yaml:"email_claim"`
	NameClaim    string `yaml:"name_claim"`
}

// RegistrationMode returns the normalized registration mode ("open" default).
func (u UsersConfig) RegistrationMode() string {
	if u.Registration == "" {
		return "open"
	}
	return u.Registration
}

// LinkChallengeTTL returns the channel-link challenge lifetime (default 10m).
func (u UsersConfig) LinkChallengeTTL() time.Duration {
	if u.LinkTTL == "" {
		return 10 * time.Minute
	}
	d, _ := time.ParseDuration(u.LinkTTL) // validated at boot; error impossible here
	return d
}

// JITEnabled reports whether a verified first login creates a profile (default
// true).
func (u UsersConfig) JITEnabled() bool {
	return u.JITProvisioning == nil || *u.JITProvisioning
}

// OriginAllowed reports whether a browser origin may call the user API. The
// list is empty by default, which means no origin may: opening the API to a
// frontend is a deliberate act, and an unreviewed wildcard would let any page a
// user visits call the API as them.
func (u UsersConfig) OriginAllowed(origin string) bool {
	if origin == "" {
		return false
	}
	for _, o := range u.CORSOrigins {
		if o == origin {
			return true
		}
	}
	return false
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
	// Stream, when set to false, makes chat calls to this model one ordinary
	// request instead of a streamed one. It is for endpoints whose streaming
	// is broken for some responses (a proxy that cannot relay streamed tool
	// calls, say). Replies then arrive whole, so llm.stream yields one delta.
	// Chat Completions (provider: openai) only; unset means streamed.
	Stream *bool `yaml:"stream,omitempty"`
	// ServerTools names provider-native server-side tools to enable on every
	// request for this model (e.g. "web_search", "x_search", "google_search").
	// Unlike client-side function tools (opts.Tools), these are executed by the
	// provider inside the completion; the runtime only injects the native tool
	// entry into the request body. The values are provider-specific strings,
	// keeping the runtime generic.
	ServerTools []string `yaml:"server_tools,omitempty"`
	// KeySource selects whose key a request carries. "" or "config" (the
	// default) sends api_key. "user" ignores api_key and sends the acting
	// user's own key, which the runtime's per-user key resolver fetches from
	// the credential store under Credential. That makes each person's calls
	// bill to their own provider account. There is no fallback to an operator
	// key: a call with no user in context, or a user with no key, fails.
	KeySource string `yaml:"key_source,omitempty"`
	// Credential names the credential-store service holding a key_source:user
	// model's per-user key. Defaults to "llm_key".
	Credential string `yaml:"credential,omitempty"`
}

// UserKeyed reports whether this model takes the acting user's own key.
func (m Model) UserKeyed() bool { return m.KeySource == "user" }

// CredentialService is the credential-store service of a user-keyed model.
func (m Model) CredentialService() string {
	if m.Credential == "" {
		return "llm_key"
	}
	return m.Credential
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

// SafetyConfig holds deployment-wide model classifiers and custom script filters.
// Profiles reference them by name as "classifier:<name>" or "script:<name>" in
// profiles.safety.<name>.filters.
type SafetyConfig struct {
	Classifiers   map[string]ClassifierProfile   `yaml:"classifiers,omitempty"`
	ScriptFilters map[string]ScriptFilterProfile `yaml:"script_filters,omitempty"`
}

// ScriptFilterProfile defines one sandboxed TypeScript/JS ingress/egress safety filter.
type ScriptFilterProfile struct {
	Source      string   `yaml:"source,omitempty" json:"source,omitempty"`
	File        string   `yaml:"file,omitempty" json:"file,omitempty"`
	Resolved    string   `yaml:"-" json:"-"`
	Directions  []string `yaml:"directions,omitempty" json:"directions,omitempty"`
	Sources     []string `yaml:"sources,omitempty" json:"sources,omitempty"`
	Timeout     string   `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	InstrBudget int64    `yaml:"instr_budget,omitempty" json:"instr_budget,omitempty"`
	OnError     string   `yaml:"on_error,omitempty" json:"on_error,omitempty"` // pass | block
	Refusal     string   `yaml:"refusal,omitempty" json:"refusal,omitempty"`
	Fallback    string   `yaml:"fallback,omitempty" json:"fallback,omitempty"`
}

// TimeoutD returns the script filter timeout, defaulting to 200ms.
func (p ScriptFilterProfile) TimeoutD() time.Duration {
	if p.Timeout == "" {
		return 200 * time.Millisecond
	}
	d, err := time.ParseDuration(p.Timeout)
	if err != nil || d <= 0 {
		return 200 * time.Millisecond
	}
	return d
}

// EffectiveSource returns the resolved script source (inline or loaded from file).
func (p ScriptFilterProfile) EffectiveSource() string {
	if p.Resolved != "" {
		return p.Resolved
	}
	return p.Source
}

// ClassifierProfile is one model-backed moderation/classification policy.
// The model returns a label and a score; this policy maps labels to actions.
type ClassifierProfile struct {
	Model  string `yaml:"model"`
	Prompt string `yaml:"prompt"`
	Mode   string `yaml:"mode"` // enforce | shadow
	// Directions selects where the classifier runs. Empty means both.
	Directions []string `yaml:"directions"`
	// Sources selects message sources. Empty means channel and agent.
	Sources []string `yaml:"sources"`
	// Threshold is the minimum score for a label's action to apply. Nil
	// means 0.7; an explicit 0 is a valid "block everything" policy.
	Threshold   *float64          `yaml:"threshold"`
	Categories  map[string]string `yaml:"categories"` // label -> allow | review | block
	Timeout     string            `yaml:"timeout"`
	OnError     string            `yaml:"on_error"`            // block | pass
	OnSaturated string            `yaml:"on_saturated"`        // block | pass
	OnBudget    string            `yaml:"on_budget_exhausted"` // block | pass
	// MaxTokens caps the classifier's answer; empty/zero means 64. Keep it
	// small: the model returns one label, a score and a short reason.
	MaxTokens      int              `yaml:"max_tokens"`
	MaxInputChars  int              `yaml:"max_input_chars"`
	MaxConcurrency int              `yaml:"max_concurrency"`
	Cache          ClassifierCache  `yaml:"cache"`
	Budget         ClassifierBudget `yaml:"budget"`
	Refusal        string           `yaml:"refusal"`
	Fallback       string           `yaml:"fallback"`
}

type ClassifierCache struct {
	Size int    `yaml:"size"`
	TTL  string `yaml:"ttl"`
}

type ClassifierBudget struct {
	TokensPerDay int64 `yaml:"tokens_per_day"`
}

// ThresholdValue returns the configured score threshold or the default.
func (p ClassifierProfile) ThresholdValue() float64 {
	if p.Threshold == nil {
		return 0.7
	}
	return *p.Threshold
}

// TimeoutD returns the classifier timeout, defaulting to two seconds.
func (p ClassifierProfile) TimeoutD() time.Duration {
	if p.Timeout == "" {
		return 2 * time.Second
	}
	d, err := time.ParseDuration(p.Timeout)
	if err != nil || d <= 0 {
		return 2 * time.Second
	}
	return d
}

// CacheTTLD returns the cache TTL, defaulting to five minutes.
func (p ClassifierProfile) CacheTTLD() time.Duration {
	if p.Cache.TTL == "" {
		return 5 * time.Minute
	}
	d, err := time.ParseDuration(p.Cache.TTL)
	if err != nil || d <= 0 {
		return 5 * time.Minute
	}
	return d
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
	Backend   string   `yaml:"backend"`
	Table     string   `yaml:"table"`
	Retention string   `yaml:"retention"`
	Window    int      `yaml:"window"`
	Requires  []string `yaml:"requires"`
	// Scope selects the isolation granularity of a private store: "user"
	// (default) scopes rows per user at the key level; "agent" keeps one pool
	// for the whole agent across users. A bound Pool supersedes it.
	Scope string `yaml:"scope"`
	// Pool binds the store to a named shared pool (created through the
	// tenancy admin API before the engine boots agents against it): rows live
	// under the pool's carve, and every access needs a grant — r/w or
	// r-only, per (pool, agent, tenant). The name must resolve at boot.
	Pool string `yaml:"pool"`
	// LocalityOK acknowledges that a durable store on an instance-local
	// backend is intentional (a cache the deployment can afford to lose, or a
	// single instance whose process lifetime is the deployment's). In a
	// regional cell it is the only way past the refusal that keeps a
	// conversation from disappearing when its session moves.
	LocalityOK bool `yaml:"locality_ok"`
}

// ShellProfile is what an agent's shell is: either one-off sandbox jobs on a
// project (provider sandbox, the default) or a connection to a real remote
// machine (provider ssh).
type ShellProfile struct {
	Provider string `yaml:"provider"` // sandbox (default) | ssh
	// Environment names an entry of environments (sandbox).
	Environment string `yaml:"environment"`
	// Project is the files-store project jobs run in when a call names none
	// (sandbox; default "workspace").
	Project string `yaml:"project"`
	// Timeout is a job's time limit, capped by runtime.sandbox.max_timeout.
	Timeout string `yaml:"timeout"`
	// Ignore lists paths a job may create that are not written back.
	Ignore   []string          `yaml:"ignore"`
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
	Model         string          `yaml:"model"`
	Loop          string          `yaml:"loop"`
	Instructions  InstructionsRef `yaml:"instructions"`
	HistoryBudget int             `yaml:"history_budget"`
	Memory        string          `yaml:"memory"`
	Shell         string          `yaml:"shell"`
	Safety        string          `yaml:"safety"`
	Skills        []string        `yaml:"skills"`
	Capabilities  []string        `yaml:"capabilities"`
	CanContact    []string        `yaml:"can_contact"`
	Credentials   []string        `yaml:"credentials"` // credential.get allow-list for spawned children
	// InheritUser mirrors agents.<name>.inherit_user for a spawned child: false
	// runs the child in the service stratum whatever tenant delegated to it.
	// Absent means inherit.
	InheritUser *bool `yaml:"inherit_user"`
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

// MarshalYAML writes the setting the way it is read: a profile name, an inline
// profile, or nothing. Without it the struct's own fields would be written,
// which the strict loader then refuses.
func (m MemoryAgentConfig) MarshalYAML() (any, error) {
	switch {
	case m.IsInline:
		return m.Inline, nil
	case m.Profile != "":
		return m.Profile, nil
	default:
		return nil, nil
	}
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
	// HTTP declares model-visible tools backed by an external HTTP API. They
	// are registered as http:<name>; no Go code is needed per integration.
	HTTP []HTTPTool `yaml:"http"`
}

// HTTPTool is one declarative REST tool. It is the YAML/Lua-free counterpart of
// a tool.def wrapper: the runtime owns request construction, credential
// resolution, address guarding, response caps and the model schema.
type HTTPTool struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	Method      string `yaml:"method"`
	URL         string `yaml:"url"`
	// Query maps a query parameter name to a template. A template is either
	// "{arg}" (the argument value, URL-escaped) or literal text with embedded
	// {arg} placeholders.
	Query map[string]string `yaml:"query,omitempty"`
	// Headers are static request headers. Secrets belong in auth:, not here:
	// header values are compiled into the tool definition and visible to the
	// operator.
	Headers map[string]string `yaml:"headers,omitempty"`
	// Body maps JSON body keys to templates. With no body mapping, a non-GET
	// request sends the model's arguments as the JSON body.
	Body map[string]any `yaml:"body,omitempty"`
	// Params is the JSON Schema the model sees. It defaults to a permissive
	// object so a tool without a declared schema is still callable.
	Params map[string]any `yaml:"params,omitempty"`
	// Response optionally selects a value from a JSON response using a dotted
	// path (e.g. "customer.id" or "items[0].name"). Empty returns the raw body
	// with status and truncation metadata.
	Response string `yaml:"response,omitempty"`
	// Auth names a per-tenant credential service to inject as a header. The
	// value is resolved at call time and never appears in the model context.
	Auth *HTTPAuth `yaml:"auth,omitempty"`
	// Timeout is a Go duration (default 30s, capped at 120s).
	Timeout string `yaml:"timeout,omitempty"`
	// MaxBytes caps the response body (default 1 MiB, capped at 10 MiB).
	MaxBytes int64 `yaml:"max_bytes,omitempty"`
	// Hosts, when set, is an allow-list of hostnames (exact or *.suffix) the
	// tool may call, in addition to the engine-wide address guard.
	Hosts []string `yaml:"hosts,omitempty"`
	// NeedsConfirm asks the user before the tool runs.
	NeedsConfirm *bool `yaml:"needs_confirm,omitempty"`
}

// HTTPAuth is a credential reference for a declarative HTTP tool.
type HTTPAuth struct {
	Service string `yaml:"service"`
}

type ToolsPolicy struct {
	Default   string                      `yaml:"default"`
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
	APIKey    string `yaml:"api_key"`   // doubao, ollama, google_search, x_search: required; stackoverflow, github: optional (lifts the anonymous quota)
	BaseURL   string `yaml:"base_url"`  // optional endpoint override
	Timeout   string `yaml:"timeout"`   // per-request bound; default 30s
	Site      string `yaml:"site"`      // stackoverflow engine: any StackExchange site (default "stackoverflow")
	Model     string `yaml:"model"`     // google_search (default gemini-3.8-flash), x_search (default grok-4.6): grounding model
	Reasoning string `yaml:"reasoning"` // x_search (default low; supports none, low, medium, high)
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
	Persistent    bool              `yaml:"persistent"`
	Budget        BudgetConfig      `yaml:"budget"`
	// Credentials is the allow-list of engine-wide credential names this
	// agent may fetch with credential.get(). Empty or missing = no access.
	Credentials []string `yaml:"credentials"`
	// InheritUser decides whether this agent takes the tenant of a message that
	// arrived over a delegation hop (another agent's agent.send/agent.request).
	// nil (absent) means inherit, which is what keeps a fleet working for one
	// tenant on that tenant's own scope instead of the fleet-wide service
	// stratum. Inherit is the default because the alternative — an agent that
	// silently runs tenant-less — is the isolation bug, not the opt-out.
	// `inherit_user: false` runs the agent in the service stratum regardless of
	// who delegated to it; the person talking to it directly still reaches it.
	InheritUser *bool `yaml:"inherit_user"`
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
	Name   string   `yaml:"name"`
	Type   string   `yaml:"type"` // webhook | telegram | ghhook | bridge | openai | qq | feishu | wecom
	Mode   string   `yaml:"mode"` // telegram: polling (default) | webhook | auto; qq | feishu: websocket (default) | webhook; wecom: webhook
	Agent  string   `yaml:"agent"`
	Agents []string `yaml:"agents"` // openai: the agents exposed as model ids
	Path   string   `yaml:"path"`   // route mounted on the shared server, e.g. /webhook/<chan>/<uuid>/
	Token  string   `yaml:"token"`
	Secret string   `yaml:"secret"` // ghhook: webhook secret; qq/feishu: AppSecret; wecom: CorpSecret (env-interpolated)
	// AppID is the bot AppID for qq (q.qq.com) or feishu/lark (open.feishu.cn).
	AppID string `yaml:"app_id"`
	// CorpID is the wecom enterprise CorpID (ww...).
	CorpID string `yaml:"corp_id"`
	// AgentID is the wecom self-built application AgentID.
	AgentID int64 `yaml:"agent_id"`
	// EncryptKey is the feishu webhook Encrypt Key (optional unless encrypting
	// callbacks) or the wecom 43-character EncodingAESKey.
	EncryptKey string `yaml:"encrypt_key"`
	// Scenes are where a qq channel takes messages from: c2c (private chats),
	// group (@-mentions in groups), guild (@-mentions in guild channels) and dm
	// (guild direct messages). Empty means c2c, group and guild.
	Scenes []string `yaml:"scenes"`
	// APIBase overrides the qq channel's API domain (default
	// https://api.bot.qq.com), for a proxy or a sandbox endpoint.
	APIBase string `yaml:"api_base"`
	// Markdown sends a qq channel's text replies in private chats and groups
	// as markdown messages, a capability the platform grants per bot. A
	// refused markdown message is resent as plain text.
	Markdown bool `yaml:"markdown"`
	// Auth is the openai channel's credential mode: user_token (a per-user
	// bearer token, requires runtime.identity.enabled), key (a shared key from
	// keys:), or none (loopback listeners only).
	Auth string `yaml:"auth"`
	// Keys are the accepted shared keys for auth: key. Like every secret field
	// they may be ${VAR} or cred:<service> references, resolved at construction.
	Keys []string `yaml:"keys"`
	// MaxRequestBytes bounds an openai request body (default 32 MiB).
	MaxRequestBytes int `yaml:"max_request_bytes"`
	// ExposeReasoning sends model reasoning as reasoning_content. Off by
	// default: it leaks the model's thinking to the caller.
	ExposeReasoning bool `yaml:"expose_reasoning"`
	// Journal is the audit mode for this channel: full | metadata | off.
	// Empty means full, except for type: openai, whose default is metadata
	// because coding contexts are large and private.
	Journal string `yaml:"journal"`
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
	// DeliverURL is the bridge adapter's outbound endpoint. Set for
	// type: bridge; the adapter translates the neutral reply into the
	// platform's API call.
	DeliverURL string `yaml:"deliver_url"`
}

// ChannelMedia gates inbound media on a channel. Media is opt-in: with no
// media block (or an empty allow list) attachments are dropped and only the
// text/caption flows. max_bytes defaults to 8 MiB when allow is set.
type ChannelMedia struct {
	MaxBytes int      `yaml:"max_bytes"`
	Allow    []string `yaml:"allow"` // MIME patterns: image/*, application/pdf, audio/mpeg, ...
}

// DefaultCapabilities is what an agent gets if capabilities are omitted.
// "users" is in the default set because the surface is read-only and scoped to
// the caller's own turn; the profile directory behind it is gated on
// maintenance provenance in the handler, not on the capability.
var DefaultCapabilities = []string{"llm.chat", "memory", "tools", "apis", "agent.send", "net.http", "users"}

// memoryProviderKnown reports whether name is a supported memory backend
// provider, for the hint that catches the retired "builtin:" spelling.
func memoryProviderKnown(name string) bool {
	for _, p := range memoryProviders {
		if p == name {
			return true
		}
	}
	return false
}

// memoryProviders is every memory backend provider the engine can open.
var memoryProviders = []string{"sqlite", "postgres", "pgvector", "qdrant", "redis", "redisvector", "volatile"}

// MemoryProviders returns the memory backend providers, in the order an editor
// offers them.
func MemoryProviders() []string { return append([]string(nil), memoryProviders...) }

// ImplicitMainDB is the main_db backend a deployment that declares no memory
// backends gets: the runtime store, so a minimal config needs no extra setting.
func (c *Config) ImplicitMainDB() Backend {
	if storedb.BackendFor(c.PersistencePath()) == storedb.BackendPostgres {
		return Backend{Provider: "postgres", Config: map[string]any{"url": c.PersistencePath()}}
	}
	return Backend{Provider: "sqlite", Config: map[string]any{"path": c.PersistencePath()}}
}

// validateMemoryProviders refuses a backend whose provider the engine cannot open.
func validateMemoryProviders(path string, c *Config) error {
	for bname, b := range c.Memory.Backends {
		if memoryProviderKnown(b.Provider) {
			continue
		}
		// The prefix used to be required. Saying so beats "unsupported
		// provider", which reads as if the backend were unknown when it is
		// only misspelled.
		if name, ok := strings.CutPrefix(b.Provider, "builtin:"); ok && memoryProviderKnown(name) {
			return fmt.Errorf("%s: memory backend %q has provider %q — providers are unprefixed now, write %q",
				path, bname, b.Provider, name)
		}
		return fmt.Errorf("%s: memory backend %q has unsupported provider %q", path, bname, b.Provider)
	}
	return nil
}

// validateMemoryScoping runs the store isolation rules over every declared
// profile and the built-in preset.
func validateMemoryScoping(path string, c *Config) error {
	check := func(label string, stores map[string]Store) error {
		for sname, s := range stores {
			if err := validateStoreScoping(path, label, sname, s); err != nil {
				return err
			}
		}
		return nil
	}
	if err := check("built-in memory profile", DefaultMemoryProfile().Stores); err != nil {
		return err
	}
	for pname, p := range c.Profiles.Memory {
		if err := check(fmt.Sprintf("memory profile %q", pname), p.Stores); err != nil {
			return err
		}
	}
	for aname, a := range c.Agents {
		if a.Memory.IsInline {
			if err := check(fmt.Sprintf("agent %q inline memory", aname), a.Memory.Inline.Stores); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidateMemorySections checks a replacement memory.backends and
// profiles.memory against an otherwise loaded config, with the checks a
// restart would run on them: providers, retention, store isolation, locality,
// the reserved profile name, and every agent's memory reference. It is how
// Studio dry-runs a memory edit without writing the file.
func ValidateMemorySections(path string, base *Config, backends map[string]Backend, profiles map[string]MemoryProfile) error {
	if base == nil {
		return fmt.Errorf("%s: no configuration to validate against", path)
	}
	clone := *base
	clone.Memory.Backends = make(map[string]Backend, len(backends))
	for k, v := range backends {
		clone.Memory.Backends[k] = v
	}
	if len(clone.Memory.Backends) == 0 {
		clone.Memory.Backends["main_db"] = base.ImplicitMainDB()
	}
	clone.Profiles.Memory = profiles
	for _, check := range []func(string, *Config) error{
		validateMemoryRetention, validateMemoryScoping, validateMemoryProviders, validateMemoryLocality,
	} {
		if err := check(path, &clone); err != nil {
			return err
		}
	}
	if _, clash := profiles["conversational"]; clash {
		return fmt.Errorf("%s: profiles.memory defines %q, which is reserved for the built-in profile", path, "conversational")
	}
	for _, name := range sortedAgentNames(clone.Agents) {
		if err := clone.ValidateAgent(name, clone.Agents[name], path); err != nil {
			return err
		}
	}
	return nil
}

func sortedAgentNames(m map[string]Agent) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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

// ExpandEnv resolves ${VAR} (and ${VAR:-default}) references in config bytes
// against the process environment, leaving everything else alone.
//
// It is the same expansion Load applies to a config file before decoding, and
// it is exported so a caller that has to reason about a value's pre-expansion
// text can ask the question exactly rather than approximating it. The console
// uses it to decide whether a live model key is still the file's placeholder —
// which it must write back as written — or a value the operator has since
// replaced, which it must write as given.
func ExpandEnv(b []byte) []byte { return expandEnv(b) }

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := decodeStrictRaw(b, &c, path); err != nil {
		return nil, err
	}
	expandDeferredSecrets(&c)
	if err := validate(path, &c); err != nil {
		return nil, err
	}
	// Every relative path in a single-file config resolves against that file's
	// directory, exactly as it does in a config directory. Without this, the
	// same config validated when the CLI changed into its directory and failed
	// at boot when the engine was started from elsewhere.
	rebaseConfigPaths(filepath.Dir(path), &c)
	if err := resolvePrompts(&c); err != nil {
		return nil, err
	}
	c.Epoch = ComputeEpoch(path)
	return &c, nil
}

// ComputeEpoch hashes the configuration fragments, in the order they are
// applied, so a fleet can tell whether every instance loaded the same
// deployment. The raw bytes are hashed — before ${VAR} expansion — so rotating
// a secret leaves the epoch alone while any change to shape moves it. File
// names are part of the hash because moving a setting between fragments is a
// change even when the merged result is identical.
func ComputeEpoch(files ...string) string {
	h := sha256.New()
	for _, f := range files {
		if f == "" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue // a fragment that is absent contributes nothing
		}
		// The name participates in the hash: moving a setting between fragments
		// is a change even when the merged result is identical. NUL separates
		// the fields so no concatenation can alias another.
		h.Write([]byte(filepath.Base(f)))
		h.Write([]byte{0})
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
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

// validateMemoryRetention rejects a store retention that would silently become
// zero — the failure mode the field shipped with. It walks every declared
// profile and the built-in preset, neither of which is otherwise checked here;
// the built-in one matters because it ships the two spellings ("30d",
// "forever") a deployment inherits without writing anything at all.
func validateMemoryRetention(path string, c *Config) error {
	check := func(label string, stores map[string]Store) error {
		for sname, s := range stores {
			if s.Retention == "" {
				continue
			}
			if _, err := ParseRetention(s.Retention); err != nil {
				return fmt.Errorf("%s: %s store %q: invalid retention %q (use ms, s, m, h, d, or \"forever\"): %w",
					path, label, sname, s.Retention, err)
			}
		}
		return nil
	}
	if err := check("built-in memory profile", DefaultMemoryProfile().Stores); err != nil {
		return err
	}
	for pname, p := range c.Profiles.Memory {
		if err := check(fmt.Sprintf("memory profile %q", pname), p.Stores); err != nil {
			return err
		}
	}
	for aname, a := range c.Agents {
		if a.Memory.IsInline {
			if err := check(fmt.Sprintf("agent %q inline memory", aname), a.Memory.Inline.Stores); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateMemoryLocality enforces the two rules locality makes true.
//
// In a regional cell, a store that carries durable conversation data must not
// live on an instance-local backend: the session can move to another instance,
// and its history would disappear. The stores that carry it are the ones the
// recall plugins read — `dialogue` and `facts`, plus `semantic` when
// plugin:semantic is selected — and a profile may opt out with locality_ok.
//
// Every tier bounds volatile memory: the backend either sets max_entries or
// every store bound to it sets a retention. Volatile is process-wide memory
// shared by every session, and without one of the two it grows until the
// process dies.
func validateMemoryLocality(path string, c *Config) error {
	type profileCheck struct {
		label string
		p     MemoryProfile
	}
	profiles := []profileCheck{{"built-in memory profile", DefaultMemoryProfile()}}
	for name, p := range c.Profiles.Memory {
		profiles = append(profiles, profileCheck{fmt.Sprintf("memory profile %q", name), p})
	}
	for name, a := range c.Agents {
		if a.Memory.IsInline {
			profiles = append(profiles, profileCheck{fmt.Sprintf("agent %q inline memory profile", name), a.Memory.Inline})
		}
	}

	for _, prof := range profiles {
		if c.IsRegional() {
			durable := map[string]bool{"dialogue": true, "facts": true}
			if prof.p.Recall == "plugin:semantic" {
				durable["semantic"] = true
			}
			for sname, s := range prof.p.Stores {
				if !durable[sname] || s.LocalityOK {
					continue
				}
				b, ok := c.Memory.Backends[s.Backend]
				if !ok {
					continue // unresolved backend: another check or boot reports it
				}
				if locality.For(b.Provider, "regional") == locality.Instance {
					return fmt.Errorf("%s: regional topology: %s store %q is on backend %q (provider %s), which is instance-local; a session that moves to another instance would lose its conversation. Move it to a shared backend or set locality_ok: true on the store", path, prof.label, sname, s.Backend, b.Provider)
				}
			}
		}
		for sname, s := range prof.p.Stores {
			b, ok := c.Memory.Backends[s.Backend]
			if !ok || b.Provider != "volatile" {
				continue
			}
			if retentionBounded(s.Retention) || backendMaxEntries(b.Config) > 0 {
				continue
			}
			return fmt.Errorf("%s: %s store %q is bound to volatile backend %q, which sets no max_entries and carries no TTL; volatile memory is shared by every session in the process and grows without bound. Set max_entries on the backend or retention on the store", path, prof.label, sname, s.Backend)
		}
	}
	return nil
}

// retentionBounded reports whether a store's retention actually expires rows.
// "forever", empty and zero all mean no TTL.
func retentionBounded(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return false
	}
	d, err := ParseRetention(raw)
	return err == nil && d > 0
}

// backendMaxEntries reads an optional max_entries from a backend's config map.
// YAML decodes integers as int; the wider forms are accepted so a config built
// by a program is not refused over a type.
func backendMaxEntries(cfg map[string]any) int {
	switch v := cfg["max_entries"].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case uint64:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}

// poolNameRe keeps pool names key-safe: the name lives in tenancy row keys
// and in grant rows, whose separator is "|".
var poolNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// validateStoreScoping checks one store's isolation config: a pool binding
// takes no scope of its own, and pool names must be key-safe.
func validateStoreScoping(path, owner, sname string, s Store) error {
	if s.Pool != "" {
		if s.Scope != "" {
			return fmt.Errorf("%s: %s store %q sets both pool and scope — a pool-bound store does not take a scope; the pool is the isolation boundary", path, owner, sname)
		}
		if !poolNameRe.MatchString(s.Pool) {
			return fmt.Errorf("%s: %s store %q: pool name %q must be 1-64 chars of letters, digits or . _ -", path, owner, sname, s.Pool)
		}
		return nil
	}
	switch s.Scope {
	case "", "user", "agent":
	default:
		return fmt.Errorf("%s: %s store %q: unsupported scope %q (want user or agent)", path, owner, sname, s.Scope)
	}
	return nil
}

// validateSafetyProfiles rejects a safety reference the engine cannot resolve.
// Every branch of resolveSafety that is not "", "none" or "default" needs a
// profiles.safety entry, and every filter named inside one needs to be a filter
// the baseline actually has — an unknown filter would otherwise be dropped from
// the chain without a word.
func validateSafetyProfiles(path string, c *Config) error {
	known := map[string]bool{}
	for _, n := range safety.FilterNames() {
		known[n] = true
	}
	var customRefs []string
	for name := range c.Safety.Classifiers {
		ref := "classifier:" + name
		known[ref] = true
		customRefs = append(customRefs, ref)
	}
	for name := range c.Safety.ScriptFilters {
		ref := "script:" + name
		known[ref] = true
		customRefs = append(customRefs, ref)
	}
	sort.Strings(customRefs)
	knownList := append(append([]string{}, safety.FilterNames()...), customRefs...)
	for pname, p := range c.Profiles.Safety {
		for _, f := range p.Filters {
			if !known[f] {
				return fmt.Errorf("%s: profiles.safety %q names unknown filter %q (known: %s)",
					path, pname, f, strings.Join(knownList, ", "))
			}
		}
	}
	for name, a := range c.Agents {
		switch a.Safety {
		case "", "none", "default":
		default:
			if _, ok := c.Profiles.Safety[a.Safety]; !ok {
				return fmt.Errorf("%s: agent %q references unknown safety profile %q (use \"default\", \"none\", or a profiles.safety entry)",
					path, name, a.Safety)
			}
		}
	}
	for name, p := range c.Profiles.Agent {
		switch p.Safety {
		case "", "none", "default":
		default:
			if _, ok := c.Profiles.Safety[p.Safety]; !ok {
				return fmt.Errorf("%s: spawn profile %q references unknown safety profile %q (use \"default\", \"none\", or a profiles.safety entry)",
					path, name, p.Safety)
			}
		}
	}
	return nil
}

// ValidateSafetySections checks a proposed replacement for the deployment's
// safety profiles, model-backed classifiers, and custom script filters against the
// rest of the config. It runs the same checks boot runs, so a Studio edit is
// refused for the reasons a restart would fail.
func ValidateSafetySections(path string, base *Config, profiles map[string]SafetyProfile, classifiers map[string]ClassifierProfile, scriptFilters map[string]ScriptFilterProfile) error {
	if base == nil {
		return fmt.Errorf("%s: no configuration to validate against", path)
	}
	clone := *base
	clone.Profiles.Safety = profiles
	clone.Safety.Classifiers = classifiers
	clone.Safety.ScriptFilters = scriptFilters
	if err := validateClassifiers(path, &clone); err != nil {
		return err
	}
	if err := validateScriptFilters(path, &clone); err != nil {
		return err
	}
	return validateSafetyProfiles(path, &clone)
}

// validateClassifiers checks the model-backed safety classifiers. The model
// must exist and be engine-keyed: a user-keyed moderation model would let a
// user without a key disable the classifier. Labels with no allow action are
// rejected because the model would then have no way to say "benign".
func validateClassifiers(path string, c *Config) error {
	validSource := map[string]bool{
		"channel": true, "agent": true, "scheduler": true, "system": true,
	}
	for name, p := range c.Safety.Classifiers {
		if name == "" || strings.ContainsAny(name, " \t/:|") {
			return fmt.Errorf("%s: safety.classifiers: %q is not a valid classifier name", path, name)
		}
		if p.Model == "" {
			return fmt.Errorf("%s: safety.classifiers.%s: model is required", path, name)
		}
		m, ok := c.Models[p.Model]
		if !ok {
			return fmt.Errorf("%s: safety.classifiers.%s references unknown model %q", path, name, p.Model)
		}
		switch m.Provider {
		case "anthropic", "openai", "openai-responses", "gemini":
		default:
			return fmt.Errorf("%s: safety.classifiers.%s model %q uses provider %q, which cannot answer a classification call", path, name, p.Model, m.Provider)
		}
		if m.UserKeyed() {
			return fmt.Errorf("%s: safety.classifiers.%s model %q has key_source: user; a classifier must use an engine-owned key so a missing user key cannot disable moderation", path, name, p.Model)
		}
		if p.Prompt != "" {
			if _, ok := c.Prompts[p.Prompt]; !ok {
				return fmt.Errorf("%s: safety.classifiers.%s references unknown prompt %q", path, name, p.Prompt)
			}
		}
		switch p.Mode {
		case "", "enforce", "shadow":
		default:
			return fmt.Errorf("%s: safety.classifiers.%s: mode %q is not enforce or shadow", path, name, p.Mode)
		}
		for _, d := range p.Directions {
			switch d {
			case "ingress", "egress":
			default:
				return fmt.Errorf("%s: safety.classifiers.%s: direction %q is not ingress or egress", path, name, d)
			}
		}
		for _, s := range p.Sources {
			if !validSource[s] {
				return fmt.Errorf("%s: safety.classifiers.%s: source %q is not channel, agent, scheduler, or system", path, name, s)
			}
		}
		if p.Threshold != nil && (*p.Threshold < 0 || *p.Threshold > 1) {
			return fmt.Errorf("%s: safety.classifiers.%s: threshold must be between 0 and 1", path, name)
		}
		if p.Timeout != "" {
			if d, err := time.ParseDuration(p.Timeout); err != nil || d <= 0 {
				return fmt.Errorf("%s: safety.classifiers.%s: timeout %q must be a positive duration", path, name, p.Timeout)
			}
		}
		for _, field := range []struct {
			name  string
			value string
		}{
			{"on_error", p.OnError},
			{"on_saturated", p.OnSaturated},
			{"on_budget_exhausted", p.OnBudget},
		} {
			switch field.value {
			case "", "block", "pass":
			default:
				return fmt.Errorf("%s: safety.classifiers.%s: %s %q is not block or pass", path, name, field.name, field.value)
			}
		}
		if len(p.Categories) == 0 {
			return fmt.Errorf("%s: safety.classifiers.%s: categories must name at least one label", path, name)
		}
		allow := false
		for label, rawAction := range p.Categories {
			action, target := safety.ParseCategoryAction(rawAction)
			switch action {
			case "allow":
				allow = true
			case "review", "block":
			case "redirect":
				if _, ok := c.Models[target]; !ok {
					return fmt.Errorf("%s: safety.classifiers.%s.categories.%s: redirect targets unknown model %q", path, name, label, target)
				}
			default:
				return fmt.Errorf("%s: safety.classifiers.%s: category %q action %q is not allow, review, block, or redirect:<model>", path, name, label, rawAction)
			}
		}
		if !allow {
			return fmt.Errorf("%s: safety.classifiers.%s: categories need at least one allow label (for example safe: allow), or the model has no way to report benign text", path, name)
		}
		if p.MaxTokens < 0 {
			return fmt.Errorf("%s: safety.classifiers.%s: max_tokens must not be negative", path, name)
		}
		if p.MaxInputChars < 0 {
			return fmt.Errorf("%s: safety.classifiers.%s: max_input_chars must not be negative", path, name)
		}
		if p.MaxConcurrency < 0 {
			return fmt.Errorf("%s: safety.classifiers.%s: max_concurrency must not be negative", path, name)
		}
		if p.Cache.Size < 0 {
			return fmt.Errorf("%s: safety.classifiers.%s: cache.size must not be negative", path, name)
		}
		if p.Cache.TTL != "" {
			if d, err := time.ParseDuration(p.Cache.TTL); err != nil || d <= 0 {
				return fmt.Errorf("%s: safety.classifiers.%s: cache.ttl %q must be a positive duration", path, name, p.Cache.TTL)
			}
		}
		if p.Budget.TokensPerDay < 0 {
			return fmt.Errorf("%s: safety.classifiers.%s: budget.tokens_per_day must not be negative", path, name)
		}
	}
	return nil
}

func validateScriptFilters(path string, c *Config) error {
	validSource := map[string]bool{
		"channel": true, "agent": true, "scheduler": true, "system": true,
	}
	baseDir := ""
	if path != "" && path != "config" && path != "test" {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			baseDir = path
		} else {
			baseDir = filepath.Dir(path)
		}
	}
	for name, p := range c.Safety.ScriptFilters {
		if name == "" || strings.ContainsAny(name, " \t/:|") || len(name) > 64 {
			return fmt.Errorf("%s: safety.script_filters: %q is not a valid script filter name", path, name)
		}
		hasSrc := strings.TrimSpace(p.Source) != ""
		hasFile := strings.TrimSpace(p.File) != ""
		if hasSrc && hasFile {
			return fmt.Errorf("%s: safety.script_filters.%s: cannot set both source and file", path, name)
		}
		if !hasSrc && !hasFile {
			return fmt.Errorf("%s: safety.script_filters.%s: source or file is required", path, name)
		}
		resolved := p.Source
		if hasFile {
			filePath := strings.TrimSpace(p.File)
			if !filepath.IsAbs(filePath) && baseDir != "" {
				filePath = filepath.Join(baseDir, filePath)
			}
			raw, err := os.ReadFile(filePath)
			if err != nil {
				return fmt.Errorf("%s: safety.script_filters.%s: read file %q: %w", path, name, p.File, err)
			}
			resolved = string(raw)
		}
		if err := safety.ValidateScriptSource(resolved); err != nil {
			return fmt.Errorf("%s: safety.script_filters.%s: %w", path, name, err)
		}
		p.Resolved = resolved
		c.Safety.ScriptFilters[name] = p

		for _, d := range p.Directions {
			switch d {
			case "ingress", "egress":
			default:
				return fmt.Errorf("%s: safety.script_filters.%s: direction %q is not ingress or egress", path, name, d)
			}
		}
		for _, s := range p.Sources {
			if !validSource[s] {
				return fmt.Errorf("%s: safety.script_filters.%s: source %q is not channel, agent, scheduler, or system", path, name, s)
			}
		}
		if p.Timeout != "" {
			d, err := time.ParseDuration(p.Timeout)
			if err != nil || d <= 0 {
				return fmt.Errorf("%s: safety.script_filters.%s: timeout %q must be a positive duration", path, name, p.Timeout)
			}
			if d > 10*time.Second {
				return fmt.Errorf("%s: safety.script_filters.%s: timeout %q must be at most 10s", path, name, p.Timeout)
			}
		}
		if p.InstrBudget < 0 {
			return fmt.Errorf("%s: safety.script_filters.%s: instr_budget must not be negative", path, name)
		}
		switch p.OnError {
		case "", "pass", "block":
		default:
			return fmt.Errorf("%s: safety.script_filters.%s: on_error %q is not pass or block", path, name, p.OnError)
		}
	}
	return nil
}

// validateHTTPTools checks the declarative tools.http block. These tools are
// model-visible and reach the network, so names, URLs, timeouts and response
// caps are all validated at boot rather than failing on a turn.
func validateHTTPTools(path string, c *Config) error {
	seen := map[string]bool{}
	for i, t := range c.Tools.HTTP {
		label := fmt.Sprintf("tools.http[%d]", i)
		if t.Name == "" {
			return fmt.Errorf("%s: %s: name is required", path, label)
		}
		for _, r := range t.Name {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
				return fmt.Errorf("%s: %s: name %q may contain only letters, digits and . _ -", path, label, t.Name)
			}
		}
		if seen[t.Name] {
			return fmt.Errorf("%s: %s: duplicate tool name %q", path, label, t.Name)
		}
		seen[t.Name] = true
		if t.URL == "" {
			return fmt.Errorf("%s: %s: url is required", path, label)
		}
		u, err := url.Parse(t.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%s: %s: url must be an absolute http(s) URL (got %q)", path, label, t.URL)
		}
		method := strings.ToUpper(strings.TrimSpace(t.Method))
		switch method {
		case "", "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
		default:
			return fmt.Errorf("%s: %s: unsupported method %q", path, label, t.Method)
		}
		if t.Params != nil {
			if typ, _ := t.Params["type"].(string); typ != "" && typ != "object" {
				return fmt.Errorf("%s: %s: params must be a JSON object schema (got type %q)", path, label, typ)
			}
		}
		if t.Timeout != "" {
			d, err := time.ParseDuration(t.Timeout)
			if err != nil || d <= 0 || d > 120*time.Second {
				return fmt.Errorf("%s: %s: timeout must be a positive duration up to 120s (got %q)", path, label, t.Timeout)
			}
		}
		if t.MaxBytes < 0 || t.MaxBytes > 10<<20 {
			return fmt.Errorf("%s: %s: max_bytes must be between 0 and 10485760", path, label)
		}
		if t.Auth != nil && t.Auth.Service == "" {
			return fmt.Errorf("%s: %s: auth.service is required when auth is set", path, label)
		}
		for _, h := range t.Hosts {
			if strings.TrimSpace(h) == "" || strings.Contains(h, "/") || strings.Contains(h, "://") {
				return fmt.Errorf("%s: %s: hosts entries must be bare hostnames (got %q)", path, label, h)
			}
		}
	}
	return nil
}

// validateRuntimeLabel checks one of the runtime labels (region, zone) that
// ends up in a metric label and a log line. It is a token, not free text, and
// empty is always allowed.
func validateRuntimeLabel(path, key, value string) error {
	if value == "" {
		return nil
	}
	if len(value) > 32 {
		return fmt.Errorf("%s: %s %q is longer than 32 characters", path, key, value)
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
		case ch == '-' || ch == '_' || ch == '.':
		default:
			return fmt.Errorf("%s: %s %q may contain only letters, digits and . _ -", path, key, value)
		}
	}
	return nil
}

func validate(path string, c *Config) error {
	// An empty deployment is legal: the engine comes up as an admin/Studio
	// control plane and the first agent can be created there. Channels and
	// triggers that name an agent still fail below, so "no agents" cannot hide
	// a reference to one that does not exist.
	// runtime.scheduler.workers is not defaulted here: main treats unset as
	// "use 8", so writing 8 here would make the default
	// look like an explicit config choice.
	if c.Runtime.Persistence == "" {
		c.Runtime.Persistence = "sqlite://./data/agentflow.db"
	}
	// The built-in conversational profile names backend main_db. A deployment
	// that declares no memory backends at all gets it on the runtime store: one
	// file or one server, no extra setting, which is what a minimal config
	// promises. An explicit backend named main_db wins.
	if len(c.Memory.Backends) == 0 {
		c.Memory.Backends = map[string]Backend{"main_db": c.ImplicitMainDB()}
	}
	if off := c.Runtime.TimezoneOffsetHours; off < -12 || off > 14 {
		return fmt.Errorf("%s: runtime.timezone_offset_hours %g is outside the real-world range -12..14", path, off)
	}
	// A region ends up in a metric label and a log line, so it is a token
	// rather than free text. Empty is the default and means the deployment has
	// no regions at all. A zone is the same kind of label; it is optional even
	// in a regional cell.
	if err := validateRuntimeLabel(path, "runtime.region", c.Runtime.Region); err != nil {
		return err
	}
	if err := validateRuntimeLabel(path, "runtime.zone", c.Runtime.Zone); err != nil {
		return err
	}
	// The tier is the one setting the rest of the file is judged against, so an
	// unknown value fails here rather than falling through to single.
	switch t := strings.ToLower(strings.TrimSpace(c.Runtime.Topology)); t {
	case "", "single", "regional":
	default:
		return fmt.Errorf("%s: runtime.topology %q is not a tier (want \"single\" or \"regional\")", path, c.Runtime.Topology)
	}
	// Limits are counts and rates: a negative value is meaningless, and the
	// difference between "unset" (the default) and 0 (disabled) is carried by
	// the pointer on ephemeral_per_tenant.
	if n := c.Runtime.Limits.SessionsPerTenant; n < 0 {
		return fmt.Errorf("%s: runtime.limits.sessions_per_tenant must be >= 0 (0 disables the bound)", path)
	}
	if n := c.Runtime.Limits.EgressPerMinute; n < 0 {
		return fmt.Errorf("%s: runtime.limits.egress_per_minute must be >= 0 (0 disables the bound)", path)
	}
	if p := c.Runtime.Limits.EphemeralPerTenant; p != nil && *p < 0 {
		return fmt.Errorf("%s: runtime.limits.ephemeral_per_tenant must be >= 0 (0 disables the bound; unset means 16)", path)
	}
	for key, raw := range map[string]string{
		"runtime.cluster.session_ttl":   c.Runtime.Cluster.SessionTTL,
		"runtime.cluster.poll_interval": c.Runtime.Cluster.PollInterval,
		"runtime.studio.chat_timeout":   c.Runtime.Studio.ChatTimeout,
	} {
		if raw == "" {
			continue
		}
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("%s: %s: %v", path, key, err)
		}
		if d <= 0 {
			return fmt.Errorf("%s: %s must be positive", path, key)
		}
	}
	// Studio trace bounds size in-memory rings. Negative is meaningless, and
	// an unbounded value is a memory hazard; zero means the built-in default.
	if n := c.Runtime.Studio.TraceEvents; n < 0 || n > 100_000 {
		return fmt.Errorf("%s: runtime.studio.trace_events must be between 0 and 100000", path)
	}
	if n := c.Runtime.Studio.TraceSessions; n < 0 || n > 10_000 {
		return fmt.Errorf("%s: runtime.studio.trace_sessions must be between 0 and 10000", path)
	}
	// Grace 0 is meaningful (shut down immediately), so this cannot join the
	// positive-duration loop above.
	if raw := strings.TrimSpace(c.Runtime.ShutdownGrace); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("%s: runtime.shutdown_grace: %v", path, err)
		}
		if d < 0 {
			return fmt.Errorf("%s: runtime.shutdown_grace must be >= 0", path)
		}
	}
	// idle_unload 0 is meaningful (disabled), so this cannot join the
	// positive-only duration loop either.
	if raw := strings.TrimSpace(c.Runtime.Sessions.IdleUnload); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("%s: runtime.sessions.idle_unload: %v", path, err)
		}
		if d < 0 {
			return fmt.Errorf("%s: runtime.sessions.idle_unload must be >= 0 (0 disables unloading)", path)
		}
	}
	if c.Runtime.VM.MemoryLimit != nil {
		raw := strings.TrimSpace(*c.Runtime.VM.MemoryLimit)
		if raw != "" {
			if _, err := ParseByteSize(raw); err != nil {
				return fmt.Errorf("%s: runtime.vm.memory_limit: %w", path, err)
			}
		}
	}
	if !c.Runtime.Admin.TCPEnabled() && strings.TrimSpace(c.Runtime.Admin.Socket) == "" {
		return fmt.Errorf("%s: runtime.admin.tcp is false but no runtime.admin.socket is configured", path)
	}
	if sock := strings.TrimSpace(c.Runtime.Admin.Socket); sock != "" {
		if runtime.GOOS == "windows" {
			if !strings.HasPrefix(sock, namedPipePrefix) {
				return fmt.Errorf("%s: runtime.admin.socket on Windows must be a named pipe like %sagentflow-admin (got %q)", path, namedPipePrefix, sock)
			}
		} else if strings.HasPrefix(sock, namedPipePrefix) {
			return fmt.Errorf("%s: runtime.admin.socket %q is a Windows named pipe; use a filesystem socket path on this platform", path, sock)
		}
	}
	// CORS origins are exact origins, never a wildcard: the admin API carries
	// the bearer token, and "*" here would let any page on the machine read it
	// back out of a response.
	for _, origin := range c.Runtime.Admin.CORSOrigins {
		if origin == "*" {
			return fmt.Errorf("%s: runtime.admin.cors_origins: \"*\" is refused; name the exact origin (e.g. http://127.0.0.1:5173)", path)
		}
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return fmt.Errorf("%s: runtime.admin.cors_origins: %q is not an origin like http://127.0.0.1:5173", path, origin)
		}
	}
	for name, token := range c.Runtime.Admin.Operators {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("%s: runtime.admin.operators: an operator name is empty", path)
		}
		if strings.TrimSpace(token) == "" {
			return fmt.Errorf("%s: runtime.admin.operators.%s: token is empty", path, name)
		}
	}
	// Every store target is parsed here, once, so that a scheme this build has
	// no backend for is a boot error naming the schemes that exist — rather than
	// a directory named after the scheme nothing could open.
	for _, t := range []struct {
		key     string
		target  string
		allowed []string
	}{
		{"runtime.persistence", c.Runtime.Persistence, []string{storedb.BackendSQLite, storedb.BackendPostgres}},
		{"runtime.identity.persistence", c.Runtime.Identity.Persistence, []string{storedb.BackendSQLite, storedb.BackendPostgres}},
		{"runtime.credentials.persistence", c.Runtime.Credentials.Persistence, []string{storedb.BackendSQLite, storedb.BackendPostgres}},
		{"runtime.log_plane.persistence", c.Runtime.LogPlane.Persistence, []string{storedb.BackendFile}},
	} {
		if strings.TrimSpace(t.target) == "" {
			continue
		}
		if _, err := storedb.ParseTarget(t.target, t.allowed...); err != nil {
			// The parser's own "storedb:" prefix reads oddly inside a config error.
			return fmt.Errorf("%s: %s: %s", path, t.key, strings.TrimPrefix(err.Error(), "storedb: "))
		}
	}
	if c.Runtime.Credentials.Enabled && c.CredentialsMasterKeyEnv() == "" {
		return fmt.Errorf("%s: runtime.credentials.enabled requires master_key_env to name the env var holding the master key", path)
	}
	switch strings.ToLower(strings.TrimSpace(c.Runtime.Credentials.EngineWide)) {
	case "", "allow", "maintenance":
	default:
		return fmt.Errorf("%s: runtime.credentials.engine_wide %q is not a mode (want \"allow\", \"maintenance\", or unset)", path, c.Runtime.Credentials.EngineWide)
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

	// tools.policy.default. ToolVisibility reads this as "anything that is not
	// exactly none allows everything", so a typo like "non" exposed every tool
	// in the registry. The permissive default stays — an omitted key means every
	// tool, and that is what the docs and every existing config assume — so what
	// closes the hole is rejecting the value that is neither, not inverting the
	// comparison (which would deny every tool to every config that omits it).
	switch strings.ToLower(c.Tools.Policy.Default) {
	case "", "all", "none":
	default:
		return fmt.Errorf("%s: tools.policy.default %q is not a policy (want \"all\", \"none\", or unset)", path, c.Tools.Policy.Default)
	}
	if err := validateSandbox(path, c); err != nil {
		return err
	}
	if err := validateHTTPTools(path, c); err != nil {
		return err
	}
	for name, dir := range c.Packages {
		if name == "" || strings.ContainsAny(name, " \t/:|") {
			return fmt.Errorf("%s: packages: %q is not a valid package name", path, name)
		}
		if dir == "" {
			return fmt.Errorf("%s: packages.%s: dir is required", path, name)
		}
		// Existence is checked when a loop resolves its imports, after a
		// configdir has rebased the path onto its own directory. Checking here
		// would test a relative path against the process CWD instead.
	}
	if err := validateAPIs(path, c); err != nil {
		return err
	}

	// Memory store retention. This runs over every declared profile AND the
	// built-in preset, because a retention that does not parse is silently
	// zero — which is how the shipped `retention: "30d"` came to mean "never
	// expires" while looking configured.
	if err := validateMemoryRetention(path, c); err != nil {
		return err
	}

	// Store isolation: the shared: refusal and the pool-binding rules, over
	// every declared profile and the built-in preset — a store a profile
	// declares but no agent references is still refused, because a boot that
	// tolerates it would silently reinterpret it if an agent picked it up.
	if err := validateMemoryScoping(path, c); err != nil {
		return err
	}

	// Model-backed classifiers and custom script filters must resolve before profiles reference them.
	if err := validateClassifiers(path, c); err != nil {
		return err
	}
	if err := validateScriptFilters(path, c); err != nil {
		return err
	}

	// Safety profile names. An unknown one used to resolve to safety.None with
	// no warning, so a typo in an agent's safety: field turned the core-owned
	// chain off silently — the opposite of failing closed.
	if err := validateSafetyProfiles(path, c); err != nil {
		return err
	}

	allowedCaps := c.allowedCapabilitySet()

	for name, a := range c.Agents {
		if a.Loop == "" {
			return fmt.Errorf("%s: agent %q has no loop", path, name)
		}
		if a.Model != "" {
			if _, ok := c.Models[a.Model]; !ok {
				return fmt.Errorf("%s: agent %q references unknown model %q", path, name, a.Model)
			}
		}
		if err := c.ValidateAgent(name, a, path); err != nil {
			return err
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
			if err := validateShellProfile(path, "spawn:"+pname, p.Shell, c.Profiles.Shell[p.Shell], c); err != nil {
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

	if err := validateChannelList(path, c, c.Gateway.Channels); err != nil {
		return err
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
		switch m.KeySource {
		case "", "config":
		case "user":
			// Per-user keys live in the credential store; without it every call
			// would fail at request time, so refuse the configuration at boot.
			if !c.Runtime.Credentials.Enabled {
				return fmt.Errorf("%s: model %q has key_source: user, which requires runtime.credentials.enabled", path, name)
			}
			if m.APIKey != "" {
				return fmt.Errorf("%s: model %q has key_source: user and an api_key; remove api_key (a user-keyed model never sends an operator key)", path, name)
			}
		default:
			return fmt.Errorf("%s: model %q has unsupported key_source %q (want config or user)", path, name, m.KeySource)
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

	if err := validateMemoryProviders(path, c); err != nil {
		return err
	}
	if err := validateMemoryLocality(path, c); err != nil {
		return err
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
	if c.Media.Retention != "" {
		d, err := time.ParseDuration(c.Media.Retention)
		if err != nil || d < 0 {
			return fmt.Errorf("%s: media.retention must be a non-negative duration (got %q)", path, c.Media.Retention)
		}
	}
	if c.Media.DerivedTTL != "" {
		d, err := time.ParseDuration(c.Media.DerivedTTL)
		if err != nil || d < 0 {
			return fmt.Errorf("%s: media.derived_ttl must be a non-negative duration (got %q)", path, c.Media.DerivedTTL)
		}
	}
	if c.Media.GCGrace != "" {
		d, err := time.ParseDuration(c.Media.GCGrace)
		if err != nil || d < 0 {
			return fmt.Errorf("%s: media.gc_grace must be a non-negative duration (got %q)", path, c.Media.GCGrace)
		}
	}
	if c.Media.TempURL.Enabled {
		pub := strings.TrimSpace(c.Gateway.PublicURL)
		if !strings.HasPrefix(pub, "http://") && !strings.HasPrefix(pub, "https://") {
			return fmt.Errorf("%s: media.temp_url.enabled requires gateway.public_url to be an http(s) URL", path)
		}
		p := c.Media.TempURL.MediaTempURLPath()
		if !strings.HasPrefix(p, "/") || !strings.HasSuffix(p, "/") {
			return fmt.Errorf("%s: media.temp_url.path must start and end with / (got %q)", path, p)
		}
		if c.Media.TempURL.TTL != "" {
			d, err := time.ParseDuration(c.Media.TempURL.TTL)
			if err != nil || d <= 0 {
				return fmt.Errorf("%s: media.temp_url.ttl must be a positive duration (got %q)", path, c.Media.TempURL.TTL)
			}
		}
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
	if c.Usage.RetentionDays != nil && *c.Usage.RetentionDays < 0 {
		return fmt.Errorf("%s: usage.retention_days must be >= 0 (0 = keep forever)", path)
	}
	if c.Usage.DefaultTokensPerDay < 0 {
		return fmt.Errorf("%s: usage.default_tokens_per_day must be >= 0 (0 = unlimited)", path)
	}
	if err := validateObservability(path, c.Observability); err != nil {
		return err
	}

	// The users API needs the identity layer: profiles, channel links and
	// unregistered-handle policy all live in the identity store, so enabling
	// one without the other is a boot error with the corrective spelling
	// rather than an endpoint that silently answers nothing.
	if c.Runtime.Users.Enabled && !c.Runtime.Identity.Enabled {
		return fmt.Errorf("%s: runtime.users.enabled requires runtime.identity.enabled (the profile store lives there)", path)
	}
	switch c.Runtime.Users.RegistrationMode() {
	case "open", "invite":
	default:
		return fmt.Errorf("%s: runtime.users.registration must be open or invite (got %q)", path, c.Runtime.Users.Registration)
	}
	switch strings.ToLower(strings.TrimSpace(c.Runtime.Identity.Unregistered)) {
	case "", "allow", "reject":
	default:
		return fmt.Errorf("%s: runtime.identity.unregistered must be allow or reject (got %q)", path, c.Runtime.Identity.Unregistered)
	}
	if c.Runtime.Users.LinkTTL != "" {
		if _, err := time.ParseDuration(c.Runtime.Users.LinkTTL); err != nil {
			return fmt.Errorf("%s: runtime.users.link_ttl: %v", path, err)
		}
	}

	// The identity provider, when configured, must be describable: an issuer is
	// required (it is both the discovery base and the expected `iss`), and a
	// jwks_url is only needed when discovery is not available.
	if c.Runtime.Users.OIDC != nil && c.Runtime.Users.OIDC.Issuer == "" {
		return fmt.Errorf("%s: runtime.users.oidc.issuer is required (set it or remove the block)", path)
	}
	for _, o := range c.Runtime.Users.CORSOrigins {
		// An origin is scheme://host[:port] and nothing else: a trailing slash
		// or a path would never match the browser's Origin header, so a typo
		// here would fail silently at request time.
		if !strings.HasPrefix(o, "http://") && !strings.HasPrefix(o, "https://") {
			return fmt.Errorf("%s: runtime.users.cors_origins entry %q must start with http:// or https://", path, o)
		}
		if strings.HasSuffix(o, "/") || strings.Contains(strings.TrimPrefix(strings.TrimPrefix(o, "https://"), "http://"), "/") {
			return fmt.Errorf("%s: runtime.users.cors_origins entry %q must be a bare origin (no trailing slash, no path)", path, o)
		}
	}

	for sname, s := range c.MCP.Servers {
		if s.Command == "" && s.URL == "" {
			return fmt.Errorf("%s: mcp server %q has neither command nor url", path, sname)
		}
	}

	// A regional cell's promises are only true if every instance shares the
	// state they claim to share. Each refusal names the setting and the fix,
	// because silently accepting one is how a "fleet" ends up with per-instance
	// copies of data: local blobs, a local memory store, a local log plane.
	// Rules that need the environment (ADMIN_TOKEN) are checked at boot in
	// main, where the environment is resolvable.
	if c.IsRegional() {
		if storedb.BackendFor(c.Runtime.Persistence) != storedb.BackendPostgres {
			return fmt.Errorf("%s: regional topology: runtime.persistence is not a PostgreSQL target; every instance would keep its own database. Set runtime.persistence to the shared postgres:// DSN", path)
		}
		for _, t := range []struct {
			key    string
			target string
		}{
			{"runtime.identity.persistence", c.Runtime.Identity.Persistence},
			{"runtime.credentials.persistence", c.Runtime.Credentials.Persistence},
		} {
			if strings.TrimSpace(t.target) == "" {
				continue
			}
			parsed, err := storedb.ParseTarget(t.target, storedb.BackendSQLite, storedb.BackendPostgres)
			if err == nil && parsed.Backend != storedb.BackendPostgres {
				return fmt.Errorf("%s: regional topology: %s is a SQLite target; each instance would keep its own copy. Unset it to follow runtime.persistence, or point it at the shared PostgreSQL server", path, t.key)
			}
		}
		for name, b := range c.Memory.Backends {
			if b.Provider == "sqlite" {
				return fmt.Errorf("%s: regional topology: memory backend %q uses provider sqlite; every instance would keep its own memory. Use postgres, pgvector, qdrant or redisvector", path, name)
			}
		}
		if c.Media.Backend != "s3" {
			return fmt.Errorf("%s: regional topology: media.backend is %q; every instance would keep its own blobs. Set media.backend: s3", path, backendOrDefault(c.Media.Backend, "fs"))
		}
		if c.Files.HasExplicitStore() && c.Files.Backend != "s3" {
			return fmt.Errorf("%s: regional topology: files.backend is %q; every instance would keep its own files. Set files.backend: s3 or omit files.backend/dir to share media's S3 store", path, backendOrDefault(c.Files.Backend, "fs"))
		}
		if strings.TrimSpace(c.Runtime.LogPlane.Persistence) != "" {
			return fmt.Errorf("%s: regional topology: runtime.log_plane.persistence is set; a local log-plane directory would be per-instance. Unset it to keep the log in the runtime store", path)
		}
	}

	return nil
}

// backendOrDefault renders an optional backend setting for an error message,
// naming the default when the key is absent.
func backendOrDefault(value, def string) string {
	if value == "" {
		return def
	}
	return value
}

// allowedCapabilitySet is the global capability ceiling: the configured
// plugins.allow_capabilities, or the permissive backward-compatible default
// set when the ceiling is empty.
func (c *Config) allowedCapabilitySet() map[string]bool {
	allowedCaps := map[string]bool{}
	for _, cap := range c.Plugins.AllowCapabilities {
		allowedCaps[cap] = true
	}
	// If the global ceiling is empty, be permissive for backward compat.
	if len(allowedCaps) == 0 {
		for _, cap := range DefaultCapabilities {
			allowedCaps[cap] = true
		}
		allowedCaps["shell.exec"] = true
		allowedCaps["agent.spawn"] = true
		allowedCaps["agent.request"] = true
		allowedCaps["channel.push"] = true
		allowedCaps["net.mail"] = true
		allowedCaps["files"] = true
		allowedCaps["users"] = true
		allowedCaps["session.state"] = true
	}
	return allowedCaps
}

// ValidateAgent checks one agent definition against this config — the same
// rules the boot-time validate applies to every entry of agents:. The
// runtime agent registry (PUT /admin/api/agents/{name}) runs it before
// building the definition, so an agent installed live is held to exactly the
// standard a boot one is. Model existence is the caller's check: boot
// validates against the file's models:, the registry against the live model
// manager, which a runtime model upsert may have extended. `a` is taken by
// value and defaults applied to it are not written back — mirroring the
// range-loop copy in validate.
func (c *Config) ValidateAgent(name string, a Agent, path string) error {
	allowedCaps := c.allowedCapabilitySet()

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
			if err := validateShellProfile(path, name, a.Shell, c.Profiles.Shell[a.Shell], c); err != nil {
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
		if a.Memory.Profile != "conversational" {
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
				if err := validateStoreScoping(path, name, sname, store); err != nil {
					return err
				}
			}
		}
	}
	if len(a.Skills) > 0 {
		if !allowedCaps["tools"] {
			return fmt.Errorf("%s: agent %q has skills but lacks tools capability", path, name)
		}
	}
	// can_contact targets must resolve to a configured agent or a spawn
	// profile. Default-deny ACLs are enforced at runtime; this only rejects
	// broken references at install time.
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

// defaultPersistence is the runtime store a configuration that names none gets:
// one SQLite file beside the blobs.
const defaultPersistence = "sqlite://./data/agentflow.db"

// Topology returns the deployment tier: "single" when runtime.topology is
// unset, otherwise the configured value. It is the one place the rest of the
// engine asks which tier it is running as, so an unset key cannot be read
// differently by two callers.
func (c *Config) Topology() string {
	switch t := strings.ToLower(strings.TrimSpace(c.Runtime.Topology)); t {
	case "", "single":
		return "single"
	default:
		return t
	}
}

// IsRegional reports whether this instance is part of a regional cell.
func (c *Config) IsRegional() bool { return c.Topology() == "regional" }

// ShutdownGrace returns the drain window before shutdown: the configured value,
// or the tier's default (5s regional, 0 single). A value that does not parse is
// 0 here; validation refuses it with a message naming the setting.
func (c *Config) ShutdownGrace() time.Duration {
	raw := strings.TrimSpace(c.Runtime.ShutdownGrace)
	if raw == "" {
		if c.IsRegional() {
			return 5 * time.Second
		}
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// PersistencePath is the runtime store's target, resolved to the address its
// backend opens: a file path for SQLite, a DSN for PostgreSQL.
func (c *Config) PersistencePath() string {
	p := c.Runtime.Persistence
	if p == "" {
		p = defaultPersistence
	}
	return storeAddress(p)
}

// storeAddress resolves a store target to what its backend opens, and returns
// it as written when it will not parse: validation has already refused that
// configuration with a message naming the setting, and this is not the place a
// boot should die with a second, worse one.
func storeAddress(target string) string {
	t, err := storedb.ParseTarget(target, storedb.BackendSQLite, storedb.BackendPostgres)
	if err != nil {
		return strings.TrimSpace(target)
	}
	return t.Address
}

// ClusterStore returns where deployment-shared lease and session-inbox state
// lives (always runtime.persistence).
func (c *Config) ClusterStore() string {
	return c.PersistencePath()
}

// DataDir is the directory local blobs (media, files) default to. It is the
// persistence directory when the runtime store is a SQLite file — the two live
// together — and "./data" when persistence is a server DSN, where there is no
// local directory to sit beside. A target that will not parse takes the same
// answer: validation refuses it before anything reads this.
func (c *Config) DataDir() string {
	t, err := storedb.ParseTarget(c.Runtime.Persistence, storedb.BackendSQLite, storedb.BackendPostgres)
	if err != nil || t.Backend != storedb.BackendSQLite {
		return "./data"
	}
	dir := filepath.Dir(t.Address)
	if dir == "" || dir == "." {
		return "./data"
	}
	return dir
}

// IdentityStore returns where the identity registry keeps its data: a SQLite
// file path, or a PostgreSQL DSN. It follows runtime.persistence unless the
// identity block names its own target, so a deployment that points the runtime
// store at a server gets shared identities — and shared user scopes — without
// saying so twice.
func (c *Config) IdentityStore() string {
	if c.Runtime.Identity.Persistence != "" {
		return storeAddress(c.Runtime.Identity.Persistence)
	}
	return c.storeBesideRuntime("identity.db")
}

// CredentialsStore returns where the credential store keeps its data, resolved
// on the same rule as IdentityStore: an explicit target wins, otherwise it
// follows the runtime store — one file per store on one machine, one server for
// a fleet.
func (c *Config) CredentialsStore() string {
	if c.Runtime.Credentials.Persistence != "" {
		return storeAddress(c.Runtime.Credentials.Persistence)
	}
	return c.storeBesideRuntime("credentials.db")
}

// storeBesideRuntime resolves a per-store target from the runtime persistence
// target: the same DSN when persistence is a server, else a sibling file in the
// persistence directory.
func (c *Config) storeBesideRuntime(name string) string {
	p := c.PersistencePath()
	if t, err := storedb.ParseTarget(p, storedb.BackendSQLite, storedb.BackendPostgres); err == nil && t.Backend != storedb.BackendSQLite {
		return t.Address
	}
	dir := filepath.Dir(p)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

// SameStoreTarget reports whether two store targets name the same store. For
// PostgreSQL that means the same host, port and database — the credentials and
// connection parameters in a DSN do not change which database it is. For SQLite
// it means the same cleaned absolute path. A target that will not parse is only
// equal to itself as written.
//
// It exists for the regional tier's "must be one store" rules, where two
// spellings of the same DSN are the same store and two databases on one server
// are not.
func SameStoreTarget(a, b string) bool {
	ta, errA := storedb.ParseTarget(a, storedb.BackendSQLite, storedb.BackendPostgres)
	tb, errB := storedb.ParseTarget(b, storedb.BackendSQLite, storedb.BackendPostgres)
	if errA != nil || errB != nil {
		return strings.TrimSpace(a) == strings.TrimSpace(b)
	}
	if ta.Backend != tb.Backend {
		return false
	}
	if ta.Backend == storedb.BackendPostgres {
		ka, okA := postgresDatabaseKey(ta.Address)
		kb, okB := postgresDatabaseKey(tb.Address)
		if okA && okB {
			return ka == kb
		}
		return ta.Address == tb.Address
	}
	return sameFilePath(ta.Address, tb.Address)
}

// SamePostgresServer reports whether two targets are on the same PostgreSQL
// server (host and port), ignoring which database they name. A target that is
// not a PostgreSQL DSN, or a DSN shape this helper cannot read, reports false:
// the callers warn about a possible mistake, and a warning that fires when it
// cannot tell is safer than silence.
func SamePostgresServer(a, b string) bool {
	ka, okA := postgresServerKey(a)
	kb, okB := postgresServerKey(b)
	return okA && okB && ka == kb
}

// postgresDatabaseKey canonicalises a PostgreSQL DSN to host:port/database.
func postgresDatabaseKey(target string) (string, bool) {
	server, db, ok := splitPostgresDSN(target)
	if !ok {
		return "", false
	}
	return server + "/" + db, true
}

// postgresServerKey canonicalises a PostgreSQL DSN to host:port.
func postgresServerKey(target string) (string, bool) {
	server, _, ok := splitPostgresDSN(target)
	return server, ok
}

// splitPostgresDSN reads the host, port and database out of a URL-form
// PostgreSQL DSN. The keyword=value form pgx also accepts is not parsed: it is
// rare in this config and an unparsed target simply compares by its full
// string, which is the conservative answer.
func splitPostgresDSN(target string) (server, database string, ok bool) {
	t, err := storedb.ParseTarget(target, storedb.BackendSQLite, storedb.BackendPostgres)
	if err != nil || t.Backend != storedb.BackendPostgres {
		return "", "", false
	}
	u, err := url.Parse(t.Address)
	if err != nil || u.Hostname() == "" {
		return "", "", false
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	server = strings.ToLower(u.Hostname()) + ":" + port
	database = strings.TrimPrefix(u.Path, "/")
	return server, database, true
}

// sameFilePath reports whether two SQLite paths name the same file.
func sameFilePath(a, b string) bool {
	abs := func(p string) string {
		if v, err := filepath.Abs(p); err == nil {
			p = v
		}
		return filepath.Clean(p)
	}
	return abs(a) == abs(b)
}

// CredentialsMasterKeyEnv returns the env var holding the credential master
// key, defaulting to CREDENTIALS_MASTER_KEY.
func (c *Config) CredentialsMasterKeyEnv() string {
	if c.Runtime.Credentials.MasterKeyEnv != "" {
		return c.Runtime.Credentials.MasterKeyEnv
	}
	return "CREDENTIALS_MASTER_KEY"
}

func validateObservability(path string, obs ObservabilityConfig) error {
	tr := obs.Tracing
	if !tr.Enabled {
		return nil
	}
	provider := strings.ToLower(strings.TrimSpace(tr.Provider))
	switch provider {
	case "", "otlp":
		if strings.TrimSpace(tr.Endpoint) == "" {
			return fmt.Errorf("%s: observability.tracing.endpoint is required when enabled with provider otlp", path)
		}
	case "langfuse":
		hasAuth := false
		for k, v := range tr.Headers {
			if strings.EqualFold(strings.TrimSpace(k), "authorization") && strings.TrimSpace(v) != "" {
				hasAuth = true
				break
			}
		}
		if !hasAuth && (strings.TrimSpace(tr.PublicKey) == "" || strings.TrimSpace(tr.SecretKey) == "") {
			return fmt.Errorf("%s: observability.tracing with provider langfuse requires public_key and secret_key (or Authorization header)", path)
		}
	default:
		return fmt.Errorf("%s: observability.tracing.provider must be otlp or langfuse (got %q)", path, tr.Provider)
	}
	if ep := strings.TrimSpace(tr.Endpoint); ep != "" {
		u, err := url.Parse(ep)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%s: observability.tracing.endpoint must be an http(s) URL (got %q)", path, tr.Endpoint)
		}
	}
	if tr.SampleRate != nil && (*tr.SampleRate < 0 || *tr.SampleRate > 1) {
		return fmt.Errorf("%s: observability.tracing.sample_rate must be between 0 and 1", path)
	}
	if tr.BatchSize < 0 {
		return fmt.Errorf("%s: observability.tracing.batch_size must be >= 0", path)
	}
	if strings.TrimSpace(tr.FlushInterval) != "" {
		if _, err := ParseEvery(tr.FlushInterval); err != nil {
			return fmt.Errorf("%s: observability.tracing.flush_interval: %w", path, err)
		}
	}
	if strings.TrimSpace(tr.Timeout) != "" {
		if _, err := ParseEvery(tr.Timeout); err != nil {
			return fmt.Errorf("%s: observability.tracing.timeout: %w", path, err)
		}
	}
	return nil
}
