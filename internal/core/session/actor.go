// Package session implements the session actor: one goroutine owning one
// Luau state, driven by a mailbox, executing op requests from Lua.
//
// Blocking ops (llm.*, channel sends) are dispatched to the shared worker
// pool so the actor goroutine stays responsive to reload and shutdown while
// an op is in flight.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/address"
	"agentflow/internal/core/gateway"
	"agentflow/internal/core/media"
	"agentflow/internal/core/memory"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/safety"
	"agentflow/internal/luacode"
	"agentflow/internal/vm"
)

// Provenance is assigned by the core at delivery time. Lua may inspect this
// metadata but cannot forge it through an op payload.
type Provenance struct {
	Kind      string `json:"kind"` // channel | agent | scheduler | system
	Principal string `json:"principal"`
	Parent    string `json:"parent,omitempty"`
	RequestID string `json:"request_id,omitempty"`

	// UserUUID is the tenant a message belongs to, as the core resolved it.
	// It is THREE-STATE, and the distinction is load-bearing:
	//
	//	nil       unknown — nothing stamped it (a producer with no tenant in
	//	          scope: a boot/scheduler/system message). Only in this state
	//	          does the sender-address fallback apply.
	//	""        known-empty — the core resolved this message and it belongs to
	//	          no tenant: an unlinked handle, or a hop from a turn that had no
	//	          tenant. It must NOT fall back to the sender address, or an
	//	          unlinked handle's identity id would become a scope uuid.
	//	"<uuid>"  the tenant (a profile id).
	//
	// A nil pointer is omitted from JSON while an explicit "" is not, so the
	// three states survive the fleet's serialization path intact.
	//
	// This field — never Message.Payload — is where identity is authoritative.
	// The supervisor rebuilds a hop's payload from the caller's Lua table, so
	// anything carried there is Lua-reachable and forgeable; reading the tenant
	// from the payload would let a loop mint another tenant's scope (§4.2 #4).
	UserUUID *string `json:"user_uuid,omitempty"`

	// MembershipUUID is the group-context uuid for a turn acting in a group:
	// derived by the engine from (personal, group) AFTER it validated the
	// membership — the router Lua proposes the group, the engine resolves and
	// stamps it here, validated rather than trusted (A1). Same three-state
	// semantics as UserUUID. When set, the acting identity's scope is
	// user:<membership_uuid> and ScopeUUID() prefers it.
	MembershipUUID *string `json:"membership_uuid,omitempty"`
}

// Identity is the runtime-owned authority attached to an actor's operations.
type Identity struct {
	SessionID    string
	Agent        string
	ParentID     string
	CanContact   map[string]bool
	Capabilities map[string]bool
	// PushSameTenant refuses session.push_user targets other than the current
	// turn's tenant. It is a deployment policy, set by the supervisor.
	PushSameTenant bool
	// Ephemeral marks a one-turn session. It is set by the supervisor, never by
	// Lua, and is what tells onActorExit not to retire the id: an ephemeral key
	// is unique per request, so one entry per request in the retired map would
	// grow it forever.
	Ephemeral bool
}

// Message is the unified mailbox envelope (user/timer/agent/confirm/system).
// Inbound media rides Attachments as small part descriptors (blob-store
// handles or inline data) — never raw bytes.
type Message struct {
	ID          string         `json:"id"`
	Type        string         `json:"type"`
	From        string         `json:"from"`
	To          string         `json:"to,omitempty"`
	Text        string         `json:"text,omitempty"`
	Channel     string         `json:"channel,omitempty"`
	ReplyTo     string         `json:"reply_to,omitempty"`
	Payload     map[string]any `json:"payload,omitempty"`
	Attachments []media.Part   `json:"attachments,omitempty"`
	Provenance  *Provenance    `json:"provenance,omitempty"`
	Ts          int64          `json:"ts,omitempty"`
}

// ChatMessage is one llm.chat turn crossing the bridge. Plain turns use
// Role+Content. Multimodal turns carry Parts (text + media descriptors);
// Content stays the plain-text fast path and existing loops are unaffected.
// An assistant turn that requested tools sets ToolCalls; a "tool" role turn
// carrying a result sets ToolCallID (+ ToolResult or Content). An assistant
// turn may replay ThinkingBlocks (raw provider blocks from an earlier
// reply.thinking_blocks) — Anthropic requires it on continuations when
// thinking is enabled; other providers ignore it.
type ChatMessage struct {
	Role           string           `json:"role"`
	Content        string           `json:"content,omitempty"`
	Parts          []media.Part     `json:"parts,omitempty"`
	ToolCalls      []ToolCallSpec   `json:"tool_calls,omitempty"`
	ToolCallID     string           `json:"tool_call_id,omitempty"`
	ToolResult     any              `json:"tool_result,omitempty"`
	ThinkingBlocks []map[string]any `json:"thinking_blocks,omitempty"`
}

// ToolCallSpec is one tool invocation crossing the bridge (name + parsed args).
type ToolCallSpec struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
	// Arguments is the model's own argument JSON, when the provider sent it.
	// It rides through Lua as a string, so it survives the trip intact where
	// Args does not (a Lua table cannot tell [] from {}, drops nulls, and
	// rounds large integers). session.reply sends it in preference to Args; a
	// loop that edits a call's args must clear it.
	Arguments string `json:"arguments,omitempty"`
}

// ToolSpec is a provider-agnostic tool definition a loop passes via llm.chat
// opts. It crosses the bridge as {name, description, parameters} and is
// reshaped per provider at request time.
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// UnmarshalJSON accepts a tool definition in either shape a loop may hand to
// llm.chat: the flat { name, description, parameters }, or the wrapped
// { type = "function", function = { ... } } that tools.list returns. Reading
// only the flat one meant a loop that passed tools.list() straight through,
// as the examples do, sent every tool to the model with an empty name.
func (t *ToolSpec) UnmarshalJSON(b []byte) error {
	var raw struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
		Function    *struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Parameters  map[string]any `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	t.Name, t.Description, t.Parameters = raw.Name, raw.Description, raw.Parameters
	if raw.Function != nil && t.Name == "" {
		t.Name, t.Description, t.Parameters = raw.Function.Name, raw.Function.Description, raw.Function.Parameters
	}
	return nil
}

// Op is a request yielded by Lua through the async bridge.
type Op struct {
	Type        string        `json:"type"`
	Text        string        `json:"text,omitempty"`
	Level       string        `json:"level,omitempty"`
	Msg         string        `json:"msg,omitempty"`
	Seconds     float64       `json:"seconds,omitempty"`
	Messages    []ChatMessage `json:"messages,omitempty"`
	Model       string        `json:"model,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Stream      string        `json:"stream,omitempty"`
	Tools       []ToolSpec    `json:"tools,omitempty"`
	ToolChoice  string        `json:"tool_choice,omitempty"`
	Thinking    string        `json:"thinking,omitempty"` // off|low|medium|high|xhigh|max

	// Structured replies (session.reply). ToolCalls reuse the chat-turn spec so
	// a loop can hand an llm.forward result straight back.
	ToolCalls    []ToolCallSpec `json:"tool_calls,omitempty"`
	FinishReason string         `json:"finish_reason,omitempty"`
	Usage        *UsageCounts   `json:"usage,omitempty"`
	Reasoning    string         `json:"reasoning,omitempty"`

	// llm.forward options. Messages replaces the client's list; Prepend and
	// Append add turns around it; Tools adds definitions to the client's;
	// MaxTokensCap clamps the client's max_tokens. The sampling overrides reuse
	// the fields above.
	Prepend           []ChatMessage  `json:"prepend,omitempty"`
	Append            []ChatMessage  `json:"append,omitempty"`
	MaxTokensCap      int            `json:"max_tokens_cap,omitempty"`
	ParallelToolCalls *bool          `json:"parallel_tool_calls,omitempty"`
	TopP              *float64       `json:"top_p,omitempty"`
	Stop              []string       `json:"stop,omitempty"`
	Seed              *int64         `json:"seed,omitempty"`
	ResponseFormat    map[string]any `json:"response_format,omitempty"`

	// User surface (user.*): a credential service name, and the profile a
	// maintenance loop asks about.
	Service string `json:"service,omitempty"`
	UserID  string `json:"user_id,omitempty"`

	// Embedding / rerank ops (llm.embed, llm.rerank).
	// Embed inputs are parts: plain texts ride as {type:"text"}; media
	// parts (image/video/audio/pdf) ride as descriptors resolved at
	// request time (Jina multimodal embeddings convention).
	Inputs    []media.Part `json:"inputs,omitempty"`    // embed: input parts
	Documents []string     `json:"documents,omitempty"` // rerank: candidate documents
	TopN      int          `json:"top_n,omitempty"`     // rerank: keep at most this many
	// Embedding request options (llm.embed opts): task selects the Jina
	// task adapter; dimensions is a Matryoshka truncation; merged folds all
	// inputs into ONE embedding (Jina MergedContentGroup).
	Task       string `json:"task,omitempty"`
	Dimensions int    `json:"dimensions,omitempty"`
	Merged     bool   `json:"merged,omitempty"`

	// Proactive channel egress (session.push).
	Channel string `json:"channel,omitempty"`
	ReplyTo string `json:"reply_to,omitempty"`

	// Attachments are media parts for egress (session.send / session.push).
	// Loops forward inbound part descriptors (blob-store handles) here.
	Attachments []media.Part `json:"attachments,omitempty"`

	// Store / memory / tool ops.
	Table  string         `json:"table,omitempty"`
	Key    string         `json:"key,omitempty"`
	Value  any            `json:"value,omitempty"`
	TTL    float64        `json:"ttl,omitempty"`
	Vector []float32      `json:"vector,omitempty"`
	Query  memory.Query   `json:"query,omitempty"`
	Tool   string         `json:"tool,omitempty"`
	Args   map[string]any `json:"args,omitempty"`
	// ToolNames is the tools.overrides op's declared-tool list: the Lua chunk
	// reports which tools it defined so the handler can return the config
	// overrides targeting them and report ones no loop declares.
	ToolNames []string `json:"tool_names,omitempty"`

	// Files ops (files.*): user-scoped project trees, snapshots, scratch.
	Project   string `json:"project,omitempty"`
	Data      string `json:"data,omitempty"`   // base64 file content (binary puts)
	Handle    string `json:"handle,omitempty"` // existing blob handle (put by handle)
	Mime      string `json:"mime,omitempty"`
	Ref       string `json:"ref,omitempty"` // snapshot ref name or commit id
	CommitMsg string `json:"commit_msg,omitempty"`

	// Shell spawn extras. Volumes are "host:container" specs from the days of
	// a container shell: only the container path still means something — it is
	// where a sandbox mounts the project, and where an SSH spawn copies a
	// Project+Ref snapshot to. ScratchMount copies this session's scratch into
	// the given directory (SSH only). Both copies happen before the handle
	// reports ready.
	Volumes      []string `json:"volumes,omitempty"`
	ScratchMount string   `json:"scratch_mount,omitempty"`

	// SaveTo names a scratch file; http.request / builtin:fetch /
	// builtin:browser save the response body there (keyed by the session).
	SaveTo string `json:"save_to,omitempty"`

	// Owner is the owning session key, stamped by the actor at dispatch (never
	// Lua-settable). It scopes scratch space and tags engine-side saves so a
	// session's temp files cannot collide with or leak into another's.
	Owner string `json:"-"`

	// HTTP op (http.request).
	Method  string            `json:"method,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	Json    map[string]any    `json:"json,omitempty"`
	// QueryParams is the http.request ?key=value table.
	QueryParams map[string]string `json:"query_params,omitempty"`
	// EnvName is the os.env op's variable name.
	EnvName string `json:"name,omitempty"`
	// CredName is the credential.get op's credential name (a stored secret's
	// service name — never the value).
	CredName string `json:"cred_name,omitempty"`
	// Auth names a stored credential to resolve at request time (never the
	// secret itself). The loop references it; Go injects the resolved value.
	Auth *CredentialRef `json:"auth,omitempty"`

	// Mail ops (mail.imap.fetch, mail.smtp.send).
	Mailbox  string   `json:"mailbox,omitempty"`   // IMAP: folder to fetch (default INBOX)
	Limit    int      `json:"limit,omitempty"`     // IMAP: max messages to return (default 10)
	Unseen   bool     `json:"unseen,omitempty"`    // IMAP: fetch only unread
	MailFrom string   `json:"mail_from,omitempty"` // SMTP: From address
	MailTo   []string `json:"mail_to,omitempty"`   // SMTP: recipients
	Subject  string   `json:"subject,omitempty"`   // SMTP: subject
	TextBody string   `json:"text_body,omitempty"` // SMTP: plain body
	MailHost string   `json:"mail_host,omitempty"` // SMTP/IMAP: host
	MailPort int      `json:"mail_port,omitempty"` // SMTP/IMAP: port
	MailUser string   `json:"mail_user,omitempty"` // SMTP/IMAP: username (cred resolves password)

	// Multi-agent operations.
	Address string         `json:"address,omitempty"`
	Payload map[string]any `json:"payload,omitempty"`
	Timeout float64        `json:"timeout,omitempty"`
	Profile string         `json:"profile,omitempty"`
	Spec    map[string]any `json:"spec,omitempty"`
	Request string         `json:"request,omitempty"`

	// Scheduler operations.
	Interval float64 `json:"interval,omitempty"`
	Delay    float64 `json:"delay,omitempty"`
	Cron     string  `json:"cron,omitempty"`
	TimerID  string  `json:"timer_id,omitempty"`

	// Shell ops.
	ShellHandle   string            `json:"shell_handle,omitempty"`
	Cmd           string            `json:"cmd,omitempty"`
	Path          string            `json:"path,omitempty"`
	Content       string            `json:"content,omitempty"`
	Image         string            `json:"image,omitempty"`
	ShellProvider string            `json:"provider,omitempty"`
	WorkDir       string            `json:"workdir,omitempty"`
	Net           string            `json:"network,omitempty"`
	MemLimit      string            `json:"mem_limit,omitempty"`
	CPULimit      float64           `json:"cpu_limit,omitempty"`
	ShellEnv      map[string]string `json:"env,omitempty"`
	ShellOpts     map[string]any    `json:"shell_opts,omitempty"`
	Host          string            `json:"host,omitempty"`
	User          string            `json:"user,omitempty"`
	Password      string            `json:"password,omitempty"`
	KeyFile       string            `json:"key_file,omitempty"`
	// Environment names a declared environment for a sandbox job; JobID names
	// a background job (shell.status / shell.cancel).
	Environment string `json:"environment,omitempty"`
	JobID       string `json:"job_id,omitempty"`

	// Confirm bypass.
	Confirmed bool   `json:"confirmed,omitempty"`
	ConfirmID string `json:"confirm_id,omitempty"`
}

// CredentialRef names a stored credential to resolve at op-handler time. It
// carries only the service name — never the secret — so a credential value
// never crosses the Lua bridge. The resolver maps it to a per-user secret.
type CredentialRef struct {
	Service string `json:"service"`
}

// SchedulerService is implemented by the scheduler package and injected into
// the actor. The actor dispatches scheduler.* ops to it.
type SchedulerService interface {
	Every(owner string, intervalSec float64) (string, error)
	After(owner string, delaySec float64) (string, error)
	Cron(owner string, expr string) (string, error)
	Cancel(owner string, timerID string) error
}

// SchedulerScope is the core-stamped identity a timer fires under. A timer is
// not a maintenance context: it runs as the tenant (and group membership) of
// the turn that scheduled it. Engine-configured triggers are the privileged
// path and keep the "scheduler" provenance kind.
type SchedulerScope struct {
	UserUUID       *string
	MembershipUUID *string
	Principal      string
}

// ScopedSchedulerService is the provenance-aware half. The actor prefers it
// when the injected scheduler implements it; the old interface remains for
// tests and embedders, and its timers fire tenant-less rather than privileged.
type ScopedSchedulerService interface {
	EveryScoped(owner string, intervalSec float64, scope SchedulerScope) (string, error)
	AfterScoped(owner string, delaySec float64, scope SchedulerScope) (string, error)
	CronScoped(owner string, expr string, scope SchedulerScope) (string, error)
}

// Gateway is the egress half of the channel registry. Attachments carry
// media parts (typically blob-store handles); a channel that cannot deliver
// media returns an error rather than silently dropping it.
type Gateway interface {
	Send(channel, replyTo, text string, attachments []media.Part) error
	// SendReply delivers a structured assistant message. Channels without
	// structured support judge it in the registry (text only, or an error for
	// tool calls).
	SendReply(channel, replyTo string, r gateway.Reply) error
	// SendDelta delivers one text fragment of a stream.
	SendDelta(channel, replyTo, text string) error
	// EndTurn closes one turn, with the error that ended it or nil.
	EndTurn(channel, replyTo string, err error)
}

// UsageCounts is the token accounting a reply or an llm.forward result carries
// across the Lua bridge. It mirrors llm.Usage without importing the driver.
type UsageCounts struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	Cached     int `json:"cached,omitempty"`
	CacheWrite int `json:"cache_write,omitempty"`
	Reasoning  int `json:"reasoning,omitempty"`
}

// DeltaSink receives one text fragment of a stream that should reach the
// channel. It is stamped into the op context by the actor: the actor knows
// whether the profile can still veto the answer, and a handler must not.
type DeltaSink func(text string) error

type deltaSinkKeyType struct{}

var deltaSinkKey deltaSinkKeyType

// WithDeltaSink returns a context carrying the turn's delta sink.
func WithDeltaSink(ctx context.Context, sink DeltaSink) context.Context {
	return context.WithValue(ctx, deltaSinkKey, sink)
}

// DeltaSinkFromCtx extracts the delta sink stamped for this turn.
func DeltaSinkFromCtx(ctx context.Context) (DeltaSink, bool) {
	if ctx == nil {
		return nil, false
	}
	s, ok := ctx.Value(deltaSinkKey).(DeltaSink)
	return s, ok
}

type streamsLiveKeyType struct{}

var streamsLiveKey streamsLiveKeyType

// WithStreamsLive reports whether this turn may relay deltas live: the client
// asked for a stream and nothing can veto the text afterwards (D9).
func WithStreamsLive(ctx context.Context, live bool) context.Context {
	return context.WithValue(ctx, streamsLiveKey, live)
}

// StreamsLiveFromCtx reports whether the current turn may relay deltas live.
func StreamsLiveFromCtx(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(streamsLiveKey).(bool)
	return v
}

type turnMessageKeyType struct{}

var turnMessageKey turnMessageKeyType

// WithTurnMessage stamps the inbound message being served. HTTP-shaped ops
// (openai.request, llm.forward) read the request handle from it; it is a
// pointer, so a handler must not retain it past the op.
func WithTurnMessage(ctx context.Context, m *Message) context.Context {
	return context.WithValue(ctx, turnMessageKey, m)
}

// TurnMessageFromCtx returns the inbound message currently being served, or nil.
func TurnMessageFromCtx(ctx context.Context) *Message {
	if ctx == nil {
		return nil
	}
	m, _ := ctx.Value(turnMessageKey).(*Message)
	return m
}

type agentModelKeyType struct{}

var agentModelKey agentModelKeyType

// WithAgentModel stamps the agent's configured model. llm.forward uses it when
// the caller did not name one: "forward to the agent's model" has to mean the
// model the agent runs on, not the manager's "default" entry.
func WithAgentModel(ctx context.Context, model string) context.Context {
	return context.WithValue(ctx, agentModelKey, model)
}

// AgentModelFromCtx returns the agent's configured model ("" when unset).
func AgentModelFromCtx(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(agentModelKey).(string)
	return s
}

// UserResolver resolves a user UUID to a delivery target for proactive push
// (session.push_user). It is satisfied by the identity.Registry; defined
// here (not imported from identity) to avoid a session↔identity import cycle.
type UserResolver interface {
	LookupUser(uuid string) (channel, replyTo string, ok bool)
}

// ProfileSettings resolves a user's per-user overrides — a model and a layer of
// instructions — for the agent they are talking to. Nil, the default, means
// every user gets exactly what the agent is configured with.
//
// It is deliberately a two-string signature rather than a struct: the identity
// package implements it without importing this one, and a lookup that fails
// returns an error the actor ignores, because a profile read must never fail a
// person's turn.
type ProfileSettings interface {
	Settings(ctx context.Context, userID string) (model, instructionsAppend string, err error)
}

// AgentSummary is safe runtime metadata returned by agent.list.
type AgentSummary struct {
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
	ParentID  string `json:"parent_id,omitempty"`
}

// AgentService is implemented by the supervisor. It is deliberately defined
// in the session package so actors can invoke it without importing supervisor
// and creating a package cycle.
type AgentService interface {
	Send(ctx context.Context, source Identity, address string, payload map[string]any) error
	Request(ctx context.Context, source Identity, address string, payload map[string]any, timeout time.Duration) (Message, error)
	Reply(ctx context.Context, source Identity, requestID string, payload map[string]any) error
	Spawn(ctx context.Context, source Identity, profile string, spec map[string]any) (SpawnResult, error)
	List(source Identity) []AgentSummary
}

// SpawnResult mirrors the supervisor's spawn result for the Lua surface.
type SpawnResult struct {
	Address   string `json:"address"`
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
}

// OpHandler executes one op type and returns (response JSON, ok).
// Handlers for driver-backed ops (llm.*) are injected at spawn.
type OpHandler func(ctx context.Context, op Op) (string, bool)

// Info is per-agent data shared by all of the agent's sessions.
type Info struct {
	Name          string
	Model         string
	Instructions  *StringBox // hot-reloadable without a session restart
	HistoryBudget int
	Memory        *memory.AgentMemory
	Shell         map[string]any
	Skills        []string
	Capabilities  []string

	// InstructionsPath is the config value (after configdir rebasing) — the
	// path, not the content, which agent.config() surfaces. Empty when the
	// instructions came from a prompts: registry key instead.
	InstructionsPath string
	// InstructionsPrompt is the prompts: registry key the agent's instructions
	// were sourced from ("" when they came from a file path). A reload that
	// rewrites that key's text updates Instructions in place too.
	InstructionsPrompt string
	// Prompts is the deployment's shared prompt registry, surfaced read-only
	// to the loop by agent.config(). Every agent def holds the same instance,
	// so a file-backed prompt refreshed by the reload watcher is visible to
	// all of them. Nil is a valid empty registry.
	Prompts *PromptRegistry
	// Extras is the agent profile's deployment-specific data, surfaced
	// read-only by agent.config() with secret references rendered as opaque
	// markers.
	Extras map[string]any
	// Credentials is the credential.get allow-list (empty = no access).
	Credentials []string
	// InheritUser is this agent's tenant-inheritance policy across a delegation
	// hop, configured as `inherit_user` (nil = inherit, the default). See
	// InheritsUser.
	InheritUser *bool
}

// InheritsUser reports whether this agent takes the tenant that arrives with a
// delegation hop (an agent message). Opt-out is explicit — `inherit_user:
// false` — and the zero value means inherit, so an Info built without the field
// (tests, builtin loops) keeps the propagation fix on rather than silently
// switching it off. An agent that opts out still sees the person talking to it
// directly: this governs hops, not channel turns.
func (i *Info) InheritsUser() bool {
	if i == nil || i.InheritUser == nil {
		return true
	}
	return *i.InheritUser
}

// StringBox is a race-free string cell (this toolchain's sync/atomic lacks
// the typed String cell).
type StringBox struct{ v atomic.Value }

func (b *StringBox) Load() string {
	s, _ := b.v.Load().(string)
	return s
}

func (b *StringBox) Store(s string) { b.v.Store(s) }

// PromptRegistry is the deployment's resolved prompt text (name -> text),
// shared by every agent: one registry instance is handed to every agent def,
// so a file-backed prompt updated at reload time is visible to all of them at
// once. Reads and writes are serialized — a reload watcher rewrites one key's
// text while sessions snapshot the whole map on every agent.config().
//
// A nil *PromptRegistry is a valid empty registry, so an Info built without
// one (tests, builtin loops) needs no special case.
type PromptRegistry struct {
	mu    sync.RWMutex
	texts map[string]string
}

// NewPromptRegistry returns a registry over a copy of texts.
func NewPromptRegistry(texts map[string]string) *PromptRegistry {
	cp := make(map[string]string, len(texts))
	for k, v := range texts {
		cp[k] = v
	}
	return &PromptRegistry{texts: cp}
}

// Snapshot returns a copy of the current text. Callers get their own map, so
// it is safe to marshal or hold while the watcher writes.
func (r *PromptRegistry) Snapshot() map[string]string {
	out := map[string]string{}
	if r == nil {
		return out
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for k, v := range r.texts {
		out[k] = v
	}
	return out
}

// Set replaces one key's text (the reload watcher's only write).
func (r *PromptRegistry) Set(key, text string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.texts == nil {
		r.texts = map[string]string{}
	}
	r.texts[key] = text
}

// Get returns one key's current text.
func (r *PromptRegistry) Get(key string) (string, bool) {
	if r == nil {
		return "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.texts[key]
	return v, ok
}

// blockingOps run on the worker pool; everything else is inline.
var blockingOps = map[string]bool{
	"send":                 true,
	"reply":                true,
	"send.delta":           true,
	"session.push":         true,
	"session.push_user":    true,
	"llm.chat":             true,
	"llm.forward":          true,
	"llm.embed":            true,
	"llm.rerank":           true,
	"llm.stream.open":      true,
	"llm.stream.next":      true,
	"tools.run":            true,
	"store.put":            true,
	"store.get":            true,
	"store.query":          true,
	"store.delete":         true,
	"store.scopes":         true,
	"session.state.set":    true,
	"session.state.get":    true,
	"session.state.delete": true,
	"session.state.list":   true,
	"user.current":         true,
	"user.usage":           true,
	"user.has_credential":  true,
	"user.get":             true,
	"user.list":            true,
	"shell.spawn":          true,
	"shell.exec":           true,
	"shell.write":          true,
	"shell.destroy":        true,
	"shell.run":            true,
	"shell.start":          true,
	"shell.status":         true,
	"shell.cancel":         true,
	"http.request":         true,
	"mail.imap.fetch":      true,
	"mail.smtp.send":       true,
	"credential.get":       true,
	"agent.send":           true,
	"agent.request":        true,
	"agent.reply":          true,
	"agent.spawn":          true,
	"agent.list":           true,
	"scheduler.every":      true,
	"scheduler.after":      true,
	"scheduler.cron":       true,
	"scheduler.cancel":     true,
	"files.put":            true,
	"files.read":           true,
	"files.list":           true,
	"files.delete":         true,
	"files.commit":         true,
	"files.checkout":       true,
	"files.scratch.put":    true,
	"files.scratch.read":   true,
	"files.scratch.list":   true,
}

// EndReason identifies why an actor left the supervisor.
type EndReason string

const (
	EndShutdown   EndReason = "shutdown"
	EndTerminated EndReason = "terminated"
	// EndUnloaded is an idle conversation the supervisor unloaded. It is not a
	// death: the id is not retired, the lease is released, and the next message
	// recreates the actor with its session.state intact.
	EndUnloaded EndReason = "unloaded"
)

// ErrSafetyBlocked is the end-of-turn error when ingress safety refused the
// message before the loop saw it. A channel maps it to its own "input blocked"
// answer (400 content_filter on the OpenAI endpoint).
var ErrSafetyBlocked = errors.New("input blocked by safety")

// JournalMode controls what an audit journal row keeps for one channel. It is
// per channel because a coding client's traffic is large and private: a
// deployment may keep full audit for chat channels and ids-and-sizes only for
// an API channel.
type JournalMode string

const (
	// JournalFull is the historical behaviour: text and attachment descriptors.
	JournalFull JournalMode = "full"
	// JournalMetadata keeps ids, sizes, user and timings, and never the message
	// text — not even truncated.
	JournalMetadata JournalMode = "metadata"
	// JournalOff writes no journal row at all.
	JournalOff JournalMode = "off"
)

// ParseJournalMode maps the config spelling to a mode. An empty or unknown
// value is full: validation refuses unknown spellings, and an embedder that
// constructs a channel without one gets the behaviour every channel had.
func ParseJournalMode(s string) JournalMode {
	switch JournalMode(strings.ToLower(strings.TrimSpace(s))) {
	case JournalMetadata:
		return JournalMetadata
	case JournalOff:
		return JournalOff
	default:
		return JournalFull
	}
}

// AllowsText reports whether the mode may store message text.
func (m JournalMode) AllowsText() bool { return m == JournalFull }

// AttachmentBytes estimates the wire size of a set of attachment descriptors.
// Inline base64 is counted at its decoded size; a handle or a remote URL has no
// bytes here to count, so it contributes zero — metadata mode is about what the
// engine held, not what a provider fetched later.
func AttachmentBytes(parts []media.Part) int {
	n := 0
	for _, p := range parts {
		if p.Data != "" {
			n += len(p.Data) * 3 / 4
		}
	}
	return n
}

// Actor is a single agent session.
type Actor struct {
	Name        string // session key (agent|route-key), for logs
	Identity    Identity
	Info        *Info
	OnExit      func(Identity, EndReason)
	LoopFile    string   // re-read on restart; empty for builtins
	LoopSrc     string   // source used when LoopFile is empty
	SupportSrc  string   // extra chunk loaded before the plugin (plugin:token_budget)
	SupportSrcs []string // support chunks loaded before the loop plugin
	InstrBudget int64
	// MemBudget caps the live Luau state's total allocation in bytes (0 =
	// uncapped). Set by the supervisor from runtime.vm.memory_limit; with the
	// per-tenant session cap it bounds each tenant's VM footprint (E18).
	MemBudget int64

	Mailbox chan Message

	// Journal, when set, records every egress (delivered, failed, or
	// safety-blocked) to the core-owned message journal. It is set by the
	// supervisor at spawn; loops cannot touch it, which is what makes the
	// journal an audit trail.
	Journal EgressJournalFunc
	// JournalModeOf resolves the journal mode for a channel. Nil is full, the
	// behaviour every channel had before journal modes existed.
	JournalModeOf func(channel string) JournalMode

	// Tracer, when set, receives execution events (turn/op/reply/safety) for
	// an observing console. Set by the supervisor; loops cannot touch it.
	Tracer Tracer
	// TracePayloads includes message, reply and result text in trace events.
	// Metadata (op type, model, duration, tokens, errors) is always emitted.
	TracePayloads bool

	// Media is the centralized media service. When set, inbound attachments
	// receive per-scope grants before Lua sees them, and outbound attachments
	// are normalized and scope-authorized at the Lua→Go border.
	Media *media.Service

	gw     Gateway
	agents AgentService
	sched  SchedulerService
	users  UserResolver
	// profileSettings, when set, is where a per-user model and instruction layer
	// come from. See ProfileSettings.
	profileSettings ProfileSettings
	safety          *safety.Dispatcher
	handlers        map[string]OpHandler
	pool            *pool.Pool

	reload chan struct{}
	log    *slog.Logger

	// exiting is set by the session.exit op (actor goroutine only). Run
	// checks it after runOnce returns so an exited session is not restarted.
	exiting bool

	// Ephemeral marks a one-turn session spawned by DeliverEphemeral. Run does
	// not restart it after a crash, and its second inbox request ends it.
	Ephemeral bool
	// turnServed is true once an ephemeral actor's one inbox request has
	// returned a message. Written and read on the actor goroutine only.
	turnServed bool

	// busy reports whether the actor is actively processing a message (vs.
	// blocked waiting for the next inbox). Read by the supervisor's status
	// snapshot for the TUI; written only by the actor goroutine.
	busy atomic.Bool

	// lastActivity is the unix-nanos of the last turn start or end. The
	// supervisor's idle janitor reads it; the actor writes it.
	lastActivity atomic.Int64
	// unloadReq carries the idle janitor's request to unload. The actor honours
	// it only while parked on an empty mailbox, and only if nothing has happened
	// since the janitor looked — see RequestUnload. unloaded is set when it
	// does, so Run reports EndUnloaded and onActorExit does not retire the id.
	unloadReq chan int64
	unloaded  atomic.Bool

	// turnReplied is set once session.reply finishes a turn. A second reply is
	// an error: the channel has one answer to send.
	turnReplied atomic.Bool
	// turnEndedFor is the message id whose turn has already been closed. It
	// keeps EndTurn single-shot when a crash and a cancellation race the same
	// turn. It is cleared when the next turn starts: two turns may carry the
	// same message id (a retried request is the same message).
	turnEndedFor string
	// turnBlocked is set when egress safety refused text this turn produced.
	// If the turn then ends without a structured reply, closeTurn sends one
	// saying so, so a channel that reports a finish reason can report
	// content_filter instead of an answer that merely looks empty.
	turnBlocked atomic.Bool
	// deltaMu guards the delta accumulators below. Deltas arrive from worker
	// goroutines (llm.forward relays there) while the actor goroutine reads
	// them at end of turn.
	deltaMu sync.Mutex
	// turnDeltaText is every fragment the turn produced, for the audit trail:
	// a live streamed answer still needs one journal row.
	turnDeltaText strings.Builder
	// pendingDeltas holds fragments the channel must not see yet because the
	// safety profile can still drop the answer (D9).
	pendingDeltas strings.Builder

	// Refusals and fallbacks are core-authored replies to blocked traffic.
	// Rate-limit them so a flood of blocked messages cannot turn into an
	// outbound flood.
	refusalMu   sync.Mutex
	lastRefusal time.Time

	// turnDirectives holds core-authored safety directives produced by the
	// ingress safety chain for the active turn; prepended to llm.chat/stream.
	directivesMu   sync.Mutex
	turnDirectives []string

	// pendingDef stages a def snapshot pushed by the runtime agent registry;
	// applied on the actor goroutine at the top of runOnce (applyDefAtRestart).
	pendingDef atomic.Pointer[DefUpdate]
}

func New(name string, identity Identity, info *Info, gw Gateway, agents AgentService, sched SchedulerService, users UserResolver, safe *safety.Dispatcher, handlers map[string]OpHandler, p *pool.Pool, log *slog.Logger) *Actor {
	a := &Actor{
		Name:        name,
		Identity:    identity,
		Info:        info,
		InstrBudget: 5_000_000,
		Mailbox:     make(chan Message, 64),
		gw:          gw,
		agents:      agents,
		sched:       sched,
		users:       users,
		safety:      safe,
		handlers:    handlers,
		pool:        p,
		reload:      make(chan struct{}, 1),
		unloadReq:   make(chan int64, 1),
		log:         log.With("session", name),
	}
	a.touch()
	return a
}

// Busy reports whether the actor is actively processing a message rather than
// blocked waiting for the next inbox. Safe for concurrent reads (TUI snapshot).
func (a *Actor) Busy() bool { return a.busy.Load() }

// touch records activity at the current instant.
func (a *Actor) touch() { a.lastActivity.Store(time.Now().UnixNano()) }

// LastActivity is when this session last started or ended a turn.
func (a *Actor) LastActivity() time.Time {
	ns := a.lastActivity.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// RequestUnload asks the actor to unload itself. seen is the LastActivity the
// caller judged idle. The actor decides: it unloads only if it is parked
// waiting for a message, its mailbox is empty, and its activity stamp is still
// the one the caller saw. Deciding here rather than cancelling from outside is
// what keeps an unload from killing a turn that started in between, or from
// swallowing the message that started it.
func (a *Actor) RequestUnload(seen time.Time) {
	select {
	case a.unloadReq <- seen.UnixNano():
	default: // a request is already pending
	}
}

// DrainMailbox removes and returns whatever is still queued. The supervisor
// calls it after an unloaded actor has exited, to deliver again the messages
// that reached the mailbox too late to be served.
func (a *Actor) DrainMailbox() []Message {
	var out []Message
	for {
		select {
		case m := <-a.Mailbox:
			out = append(out, m)
		default:
			return out
		}
	}
}

// Reload requests a fresh-state restart at the next safe point.
func (a *Actor) Reload() {
	select {
	case a.reload <- struct{}{}:
	default: // already pending
	}
}

// DefUpdate is a full snapshot of the def-derived pieces of a session — what
// the runtime agent registry pushes into live sessions when an agent is
// replaced. The loop source is included for agents whose loop has no file
// behind it: a file-backed loop is re-read from disk at the restart this
// triggers and what the file says wins (hot reload covers policy that has a
// file behind it), but an in-memory loop has no truth beyond the def.
type DefUpdate struct {
	Info         *Info
	Handlers     map[string]OpHandler
	Safety       *safety.Dispatcher
	CanContact   map[string]bool
	Capabilities map[string]bool
	LoopSrc      string
	LoopFile     string
}

// pendingDef stages a DefUpdate for the actor goroutine to apply at the
// restart the reload signal causes.

// Define stages a def snapshot and signals a restart at the next safe point.
// The sequencing is the semantics: the turn in flight finishes on the def it
// resolved with, and the next turn runs the new one — E19's "applies when the
// session is graced or idle", using the actor's park points as the grace.
// The fields themselves are swapped on the actor's own goroutine (see
// applyDefAtRestart), which is what makes them race-free without locking
// every read; Define only stages and signals.
func (a *Actor) Define(u DefUpdate) {
	a.pendingDef.Store(&u)
	a.Reload()
}

// applyDefAtRestart consumes a staged DefUpdate at the top of runOnce — on
// the actor goroutine, before the loop source is loaded, so a restart runs
// the new def end to end and never a mix. Two Defines between parks coalesce
// into one restart; the last snapshot wins, which is sound because a
// snapshot is complete.
func (a *Actor) applyDefAtRestart() {
	p := a.pendingDef.Swap(nil)
	if p == nil {
		return
	}
	u := *p
	a.Info = u.Info
	a.handlers = u.Handlers
	a.safety = u.Safety
	a.Identity.CanContact = u.CanContact
	a.Identity.Capabilities = u.Capabilities
	a.LoopSrc = u.LoopSrc
	a.LoopFile = u.LoopFile
}

// Run is the actor's main loop. It owns the Luau state; call from one
// goroutine only. It restarts the loop on crashes and reload signals until
// ctx is done.
func (a *Actor) Run(ctx context.Context) {
	defer func() {
		if a.OnExit == nil {
			return
		}
		reason := EndTerminated
		if a.unloaded.Load() {
			reason = EndUnloaded
		} else if ctx.Err() != nil {
			reason = EndShutdown
		}
		a.OnExit(a.Identity, reason)
	}()

	for {
		if ctx.Err() != nil {
			return
		}
		crashed := a.runOnce(ctx)
		if a.Ephemeral {
			// A one-turn session is not restarted, on a clean exit or a crash:
			// there is no conversation to recover, and the request that spawned
			// it is waiting on the end-of-turn signal, not on a retry.
			return
		}
		if a.exiting {
			a.log.Info("session exited via session.exit")
			return
		}
		if a.unloaded.Load() {
			return
		}
		if crashed {
			a.log.Warn("loop crashed; restarting in 1s")
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

// loadSource returns the loop source to run and the name a version refusal
// should carry: the path for a loop that lives on disk, so the log line names
// the file to edit, and the session for one handed over in memory, where the
// session key is all an operator has to go on.
//
// The in-memory source is gated where it enters the VM, not here — see
// vm.State.Start — so that a loop a library consumer sets directly is refused
// on the same terms as one read from a path.
func (a *Actor) loadSource() (name, src string, err error) {
	if a.LoopFile != "" {
		// LoopFile may be a directory (a multi-module loop); luacode.Resolve
		// handles both file and directory, concatenating a directory's *.lua
		// members in sorted order. For a plain file it reads the file.
		src, _, err := luacode.Resolve(a.LoopFile)
		return a.LoopFile, src, err
	}
	return "in-memory loop of " + a.Name, a.LoopSrc, nil
}

func (a *Actor) runOnce(ctx context.Context) (crashed bool) {
	a.applyDefAtRestart()
	name, src, err := a.loadSource()
	if err != nil {
		a.log.Warn("cannot read loop plugin", "err", err)
		return true
	}

	// Support chunks are re-read here for the same reason the loop source is
	// resolved here: a restart is where a change on disk is picked up. Reading
	// them only at spawn meant a plugins.dir shadow edited while a session ran
	// could never reach it — the reload watcher's signal arrives as a restart,
	// and the restart re-read the value captured at spawn.
	//
	// The read is version-gated, and a refusal keeps the chunks this session
	// already has rather than failing the restart: a shadow edited into a
	// version this core cannot serve is a bad edit on disk, and the rule for one
	// of those is that it never takes a live session down — the same rule the
	// reload watcher applies when it refuses a loop edit and keeps the running
	// version (see internal/core/reload). The set kept here is itself the
	// product of a gate: a session that exists was built from chunks that
	// passed, at spawn or at an earlier restart, so falling back to them is
	// running a version this core does serve, not silently loading a different
	// one. A refusal with nothing to fall back on is the one case that cannot
	// keep anything — the loop would then run without the support chunks it
	// expects — so that session does not start.
	if srcs, err := luacode.SupportChunks(); err != nil {
		if len(a.SupportSrcs) == 0 && a.SupportSrc == "" {
			a.log.Error("support chunk refused and no earlier version is loaded to keep; not starting without it", "err", err)
			return true
		}
		a.log.Error("support chunk refused; keeping the version already loaded", "err", err)
	} else {
		a.SupportSrcs = srcs
	}

	st := vm.New(a.InstrBudget)
	if a.MemBudget > 0 {
		st.SetMemoryCap(a.MemBudget)
	}
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		a.log.Warn("prelude load failed", "err", err)
		return true
	}
	if a.SupportSrc != "" {
		if err := st.Eval("@plugin:token_budget", a.SupportSrc); err != nil {
			a.log.Warn("support chunk load failed", "err", err)
			return true
		}
	}
	for i, chunk := range a.SupportSrcs {
		if err := st.Eval(fmt.Sprintf("@builtin:support:%d", i), chunk); err != nil {
			a.log.Warn("support chunk load failed", "idx", i, "err", err)
			return true
		}
	}

	status, msg := st.Start("loop", name, src)
	a.log.Info("loop started")
	var current Message // inbound message being processed; replies go here

	for {
		switch status {
		case vm.Finished:
			a.log.Info("loop finished")
			// A loop that returns instead of parking has produced what it is
			// going to produce. Close the turn: the channel is waiting, and the
			// session may restart.
			a.closeTurn(ctx, &current, nil)
			return false
		case vm.Failed:
			a.log.Warn("loop error", "err", msg)
			a.trace(TraceEvent{Kind: "error", Err: truncateTrace(msg)})
			a.closeTurn(ctx, &current, errors.New(msg))
			return true
		}

		var op Op
		if err := json.Unmarshal([]byte(msg), &op); err != nil {
			a.log.Warn("bad op from lua", "err", err, "raw", msg)
			a.trace(TraceEvent{Kind: "error", Err: truncateTrace(err.Error())})
			return true
		}
		a.log.Debug("op dispatch", "op", op.Type, "model", op.Model, "tool", op.Tool, "blocking", blockingOps[op.Type])

		// Loop machinery is not traced as a step: inbox marks the turn
		// boundary itself, and the rest are not observable work.
		traced := op.Type != "inbox" && op.Type != "sleep" && op.Type != "log" && op.Type != "session.exit"
		turn := current.ID
		if traced {
			ev := TraceEvent{Kind: "op.start", Op: op.Type, Tool: op.Tool, Model: op.Model, Turn: turn, Messages: len(op.Messages)}
			if a.TracePayloads {
				ev.Text = truncateTrace(traceOpPayload(op))
			}
			a.trace(ev)
		}
		started := time.Now()

		var resp string
		var ok, proceed bool

		if blockingOps[op.Type] {
			resp, ok, proceed = a.dispatchBlocking(ctx, op, &current)
		} else {
			resp, ok, proceed = a.dispatchInline(ctx, op, &current)
		}
		if traced {
			ev := TraceEvent{
				Op: op.Type, Tool: op.Tool, Model: op.Model, Turn: turn,
				DurationMS: time.Since(started).Milliseconds(), Messages: len(op.Messages),
			}
			traceOpEnd(&ev, resp, ok)
			if a.TracePayloads && resp != "" {
				ev.Text = truncateTrace(resp)
			}
			a.trace(ev)
		}
		if !proceed {
			if op.Type != "inbox" {
				// The turn was interrupted mid-flight: a reload, a shutdown or a
				// cancelled request. The channel is told, or it would wait for an
				// answer that can never come.
				err := errors.New("turn interrupted")
				if ctx.Err() != nil {
					err = ctx.Err()
				}
				a.closeTurn(ctx, &current, err)
			}
			return false // shutdown or reload
		}

		// Confirm interception: when tools.run returns needs_confirm, park the
		// coroutine, send a confirm prompt, block for the user's confirm reply,
		// then re-dispatch with Confirmed=true. The Lua coroutine stays suspended
		// throughout — it yields once, resumes once.
		if op.Type == "tools.run" && ok && isConfirmRequest(resp) {
			resp, ok, proceed = a.doConfirm(ctx, op, &current, resp)
			if !proceed {
				return false
			}
		}

		status, msg = st.Resume(resp, ok)
	}
}

// dispatchInline runs fast ops on the actor goroutine.
func (a *Actor) dispatchInline(ctx context.Context, op Op, current *Message) (resp string, ok, proceed bool) {
	switch op.Type {
	case "inbox":
		// Parking on the inbox is where a turn ends: the loop has finished
		// answering and asked for the next message.
		if current != nil && current.ID != "" {
			a.trace(TraceEvent{Kind: "turn.end", Turn: current.ID})
			a.closeTurn(ctx, current, nil)
		}
		// An ephemeral session serves exactly one message: the second request
		// for the inbox ends it, after the turn.end above so the channel sees
		// the turn finish rather than a session that vanished mid-turn.
		if a.Ephemeral && a.turnServed {
			a.exiting = true
			return "", true, false
		}
		var m Message
		var goOn bool
		for {
			// A safety skip continues this loop rather than ending it; an
			// ephemeral actor has already consumed its one message by then, so
			// the skip ends the turn instead of parking for a second one.
			if a.Ephemeral && a.turnServed {
				a.exiting = true
				return "", true, false
			}
			m, goOn = a.waitInbox(ctx)
			if !goOn {
				return "", false, false
			}
			a.turnServed = true
			// The previous turn is closed; this one starts clean.
			a.turnReplied.Store(false)
			a.turnBlocked.Store(false)
			a.turnEndedFor = ""
			a.resetTurnDeltas()
			// Stamp the turn's identity before safety so a classifier can
			// attribute usage and apply tenant policy. The op context is also
			// what the loop's later handlers see.
			turnCtx := a.opContext(ctx, &m)
			if a.safety == nil {
				break
			}
			// Safety ingress: run the message through the dispatcher before
			// the loop sees it. A classifier block skips the turn entirely;
			// other drops preserve the historical "empty message" behavior.
			res := a.safety.Ingress(turnCtx, safety.IngressInput{
				Message:   m.Text,
				Source:    ingressSource(m.Type),
				SessionID: a.Identity.SessionID,
			})
			if res.Drop || res.Skip {
				a.log.Info("safety ingress dropped message", "reason", res.Reason)
				metrics.Inc("agentflow_safety_drops")
				a.trace(TraceEvent{Kind: "safety", Op: "ingress", Turn: m.ID, Err: res.Reason, Safety: res.Meta})
				if res.Refusal != "" {
					a.sendRefusal(turnCtx, m, res.Refusal, res.Reason)
				}
				if res.Skip {
					// The turn is over before it began. The channel is told:
					// a synchronous client would otherwise wait out its
					// timeout for an answer that can never come.
					a.closeTurn(turnCtx, &m, ErrSafetyBlocked)
					continue // wait for the next message; this turn is over
				}
				m.Text = "" // legacy drop: loop sees an empty message
				break
			}
			a.setTurnDirectives(res.Directives)
			if res.Text != m.Text {
				m.Text = res.Text
				a.trace(TraceEvent{Kind: "safety", Op: "ingress", Turn: m.ID, OK: true, Err: "rewritten", Safety: res.Meta})
			} else if res.Meta != nil || len(res.Directives) > 0 || res.Annotations["support-offer"] != "" {
				a.trace(TraceEvent{Kind: "safety", Op: "ingress", Turn: m.ID, OK: true, Err: res.Reason, Safety: res.Meta})
			}
			break
		}
		if a.Media != nil && len(m.Attachments) > 0 {
			turnCtx := a.opContext(ctx, &m)
			scope := a.mediaScope(turnCtx)
			for _, att := range m.Attachments {
				if att.Handle != "" {
					_ = a.Media.Grant(turnCtx, scope, att.Handle, "channel")
				}
			}
			if norm, err := a.Media.NormalizeBorder(turnCtx, scope, m.Attachments); err == nil {
				m.Attachments = norm
			}
		}
		*current = m
		ev := TraceEvent{Kind: "turn.start", Turn: m.ID, Messages: 1}
		if a.TracePayloads {
			ev.Text = truncateTrace(m.Text)
		}
		a.trace(ev)
		r, _ := jsonString(m)
		return r, true, true

	case "sleep":
		if !sleepOr(ctx, a.reload, time.Duration(op.Seconds*float64(time.Second))) {
			return "", false, false
		}
		return "true", true, true

	case "log":
		a.log.Log(ctx, levelOf(op.Level), op.Msg)
		return "true", true, true

	case "session.exit":
		// The coroutine is never resumed; Run sees the flag and lets the
		// actor terminate instead of restarting the loop. A turn in progress is
		// closed first: the channel is waiting for an answer, and exit means
		// "this is all there is".
		a.closeTurn(ctx, current, nil)
		a.exiting = true
		return "true", true, false

	case "agent.info":
		// Stamped here rather than once before the switch: the inbox case sets
		// `current`, so a hoisted stamp would describe the previous message.
		// agent.info is the case that needs the person, because a profile may
		// override the model and add a layer of instructions.
		return a.infoJSON(a.opContext(ctx, current)), true, true

	case "agent.config":
		return a.configJSON(), true, true

	default:
		// Stamp the tenant user UUID for inline handlers, mirroring execBlocking.
		ctx = a.opContext(ctx, current)
		op.Owner = a.Identity.SessionID
		if h, found := a.handlers[op.Type]; found {
			r, ok := h(ctx, op)
			return r, ok, true
		}
		return fmt.Sprintf("%q", "unknown op "+op.Type), false, true
	}
}

// dispatchBlocking runs an op on the worker pool and waits for the result,
// a reload signal, or shutdown. On abandon the op finishes in the background
// and its result is discarded (the VM is dead by then anyway).
func (a *Actor) dispatchBlocking(ctx context.Context, op Op, current *Message) (resp string, ok, proceed bool) {
	type result struct {
		resp string
		ok   bool
	}
	resCh := pool.CallKeyed(a.pool, a.fairnessKey(current), func() result {
		r, ok := a.execBlocking(ctx, op, current)
		return result{r, ok}
	})
	select {
	case res := <-resCh:
		return res.resp, res.ok, true
	case <-a.reload:
		a.log.Info("hot reload: restarting loop")
		a.trace(TraceEvent{Kind: "reload"})
		return "", false, false
	case <-ctx.Done():
		return "", false, false
	}
}

func (a *Actor) execBlocking(ctx context.Context, op Op, current *Message) (string, bool) {
	ctx = WithOwner(ctx, a.Name)
	ctx = a.stampIdentity(ctx, current)
	ctx = WithSessionKey(ctx, a.Identity.SessionID)
	ctx = WithProvenanceKind(ctx, provenanceKindOf(current))
	ctx = WithTurnMessage(ctx, current)
	if a.Info != nil {
		ctx = WithAgentModel(ctx, a.Info.Model)
	}
	if op.Type == "llm.forward" {
		// "The agent's model" for a forwarded request is the model this
		// person's turns run on: a profile's model override applies here
		// exactly as it does to a loop that reads agent.info().model.
		if a.Info != nil {
			ctx = WithAgentModel(ctx, a.turnModel(ctx))
		}
		// Only llm.forward relays deltas; stamping the sink for every op would
		// let a custom handler stream without the actor deciding whether the
		// answer can still be vetoed.
		ctx = WithDeltaSink(ctx, a.deltaSink(current))
		ctx = WithStreamsLive(ctx, a.streamsLive(current))
	}
	op.Owner = a.Identity.SessionID
	if a.Info != nil {
		ctx = WithShell(ctx, a.Info.Shell)
	}
	if op.Type == "send" {
		if current.Channel == "" {
			return `"no active message to reply to"`, false
		}
		resp, ok := a.egress(ctx, current.Channel, current.ReplyTo, op.Text, op.Attachments, current.ID)
		if !ok && resp == egressBlocked {
			a.turnBlocked.Store(true)
		}
		return resp, ok
	}
	if op.Type == "reply" {
		return a.replyOp(ctx, current, op)
	}
	if op.Type == "send.delta" {
		return a.deltaOp(current, op)
	}
	if op.Type == "session.push" {
		// Proactive egress: send to an explicit channel/recipient without an
		// active inbound message. Capability-gated; same safety egress as send.
		if !a.Identity.Capabilities["channel.push"] {
			return `"agent lacks channel.push capability"`, false
		}
		if op.Channel == "" {
			return `"session.push requires a channel"`, false
		}
		return a.egress(ctx, op.Channel, op.ReplyTo, op.Text, op.Attachments, current.ID)
	}
	if op.Type == "session.push_user" {
		// Proactive egress to a user by identity UUID. Channel-agnostic: the
		// loop only knows the UUID (from msg.from of a prior turn); the
		// resolver maps it back to {channel, reply_to}. Same capability gate
		// and safety egress as session.push.
		if !a.Identity.Capabilities["channel.push"] {
			return `"agent lacks channel.push capability"`, false
		}
		if a.users == nil {
			return `"identity not enabled on this runtime"`, false
		}
		addr, err := address.Parse(op.Address)
		if err != nil || addr.Kind != address.User {
			return `"session.push_user requires a user:<uuid> address"`, false
		}
		if a.Identity.PushSameTenant {
			if me := PersonalUUIDFromCtx(ctx); me != addr.User {
				return `"session.push_user target is outside the current tenant"`, false
			}
		}
		channel, replyTo, ok := a.users.LookupUser(addr.User)
		if !ok {
			return `"unknown user"`, false
		}
		return a.egress(ctx, channel, replyTo, op.Text, op.Attachments, current.ID)
	}
	if a.agents != nil {
		switch op.Type {
		case "agent.send":
			if err := a.agents.Send(ctx, a.Identity, op.Address, op.Payload); err != nil {
				r, _ := jsonString(err.Error())
				return r, false
			}
			return "true", true
		case "agent.request":
			msg, err := a.agents.Request(ctx, a.Identity, op.Address, op.Payload, time.Duration(op.Timeout*float64(time.Second)))
			if err != nil {
				r, _ := jsonString(err.Error())
				return r, false
			}
			r, _ := jsonString(msg)
			return r, true
		case "agent.reply":
			if err := a.agents.Reply(ctx, a.Identity, op.Request, op.Payload); err != nil {
				r, _ := jsonString(err.Error())
				return r, false
			}
			return "true", true
		case "agent.list":
			r, _ := jsonString(a.agents.List(a.Identity))
			return r, true
		case "agent.spawn":
			res, err := a.agents.Spawn(ctx, a.Identity, op.Profile, op.Spec)
			if err != nil {
				r, _ := jsonString(err.Error())
				return r, false
			}
			r, _ := jsonString(res)
			return r, true
		}
	}
	if a.sched != nil {
		scope := schedulerScopeFromCtx(ctx)
		scoped, _ := a.sched.(ScopedSchedulerService)
		switch op.Type {
		case "scheduler.every":
			var id string
			var err error
			if scoped != nil {
				id, err = scoped.EveryScoped(a.Name, op.Interval, scope)
			} else {
				id, err = a.sched.Every(a.Name, op.Interval)
			}
			if err != nil {
				r, _ := jsonString(err.Error())
				return r, false
			}
			r, _ := jsonString(map[string]any{"id": id})
			return r, true
		case "scheduler.after":
			var id string
			var err error
			if scoped != nil {
				id, err = scoped.AfterScoped(a.Name, op.Delay, scope)
			} else {
				id, err = a.sched.After(a.Name, op.Delay)
			}
			if err != nil {
				r, _ := jsonString(err.Error())
				return r, false
			}
			r, _ := jsonString(map[string]any{"id": id})
			return r, true
		case "scheduler.cron":
			var id string
			var err error
			if scoped != nil {
				id, err = scoped.CronScoped(a.Name, op.Cron, scope)
			} else {
				id, err = a.sched.Cron(a.Name, op.Cron)
			}
			if err != nil {
				r, _ := jsonString(err.Error())
				return r, false
			}
			r, _ := jsonString(map[string]any{"id": id})
			return r, true
		case "scheduler.cancel":
			if err := a.sched.Cancel(a.Name, op.TimerID); err != nil {
				r, _ := jsonString(err.Error())
				return r, false
			}
			return "true", true
		}
	}
	if h, found := a.handlers[op.Type]; found {
		if (op.Type == "llm.chat" || op.Type == "llm.stream") && len(op.Messages) > 0 {
			if dirs := a.snapshotTurnDirectives(); len(dirs) > 0 {
				op.Messages = injectSafetyDirectives(op.Messages, dirs)
			}
		}
		return h(ctx, op)
	}
	r, _ := jsonString("unknown op " + op.Type)
	return r, false
}

// EgressRecord is one journaled outbound message: what the session tried to
// send, to whom, and what happened. InReplyTo correlates the reply with the
// journaled inbound message that prompted it (empty for proactive pushes).
type EgressRecord struct {
	SessionID   string
	Agent       string
	Channel     string
	ReplyTo     string
	UserUUID    string // profile behind the reply ("" when the sender is unregistered)
	Text        string
	Attachments []media.Part
	InReplyTo   string // inbound message id, when this is a reply
	Status      string // delivered | failed | blocked_safety
	Err         string
	// Sizes are always filled in, so a metadata-mode row keeps the shape of the
	// traffic it chose not to store.
	TextBytes       int
	AttachmentBytes int
}

// EgressJournalFunc records one egress to the message journal. Implementations
// must not block meaningfully (a fast DB write); errors are logged, never
// fatal — an audit hiccup must not drop user replies.
type EgressJournalFunc func(EgressRecord)

// allowRefusal enforces one core-authored auto-reply per session per 30s.
// Without it, an attacker can make the bot answer every blocked message.
func (a *Actor) allowRefusal() bool {
	const interval = 30 * time.Second
	a.refusalMu.Lock()
	defer a.refusalMu.Unlock()
	if !a.lastRefusal.IsZero() && time.Since(a.lastRefusal) < interval {
		return false
	}
	a.lastRefusal = time.Now()
	return true
}

// sendRefusal delivers a core-authored refusal for a dropped inbound turn.
// It is intentionally outside the safety chain: the text is written by the
// deployment, never by a loop or a model.
func (a *Actor) sendRefusal(ctx context.Context, m Message, text, reason string) {
	if text == "" || a.gw == nil || m.Channel == "" || !a.allowRefusal() {
		return
	}
	if err := a.gw.Send(m.Channel, m.ReplyTo, text, nil); err != nil {
		a.log.Warn("safety refusal send failed", "err", err, "channel", m.Channel)
		return
	}
	a.writeEgressJournal(EgressRecord{
		SessionID: a.Identity.SessionID,
		Agent:     a.Identity.Agent,
		Channel:   m.Channel,
		ReplyTo:   m.ReplyTo,
		UserUUID:  PersonalUUIDFromCtx(ctx),
		Text:      text,
		InReplyTo: m.ID,
		Status:    "blocked_safety",
		Err:       reason,
	})
}

// journalModeOf resolves a channel's journal mode. Nil is full, so an actor
// built without a supervisor lookup behaves exactly as it did before modes
// existed.
func (a *Actor) journalModeOf(channel string) JournalMode {
	if a.JournalModeOf == nil {
		return JournalFull
	}
	return a.JournalModeOf(channel)
}

// writeEgressJournal records one egress under the channel's journal mode. The
// sizes are filled in for every mode; metadata drops the payload after they are
// measured, so even a truncated copy of the text can never reach the journal.
func (a *Actor) writeEgressJournal(rec EgressRecord) {
	if a.Journal == nil {
		return
	}
	mode := a.journalModeOf(rec.Channel)
	if mode == JournalOff {
		return
	}
	rec.TextBytes = len(rec.Text)
	rec.AttachmentBytes = AttachmentBytes(rec.Attachments)
	if !mode.AllowsText() {
		rec.Text = ""
		rec.Attachments = nil
	}
	a.Journal(rec)
}

// egress runs text through the safety dispatcher and delivers it to a
// channel recipient via the gateway. A safety drop means nothing is sent.
// Attachments are not safety-screened (the filters are text-only by design);
// the text (caption) always is. Media allow-lists gate them at ingestion.
// Every outcome is journaled when a Journal is set.
func (a *Actor) mediaScope(ctx context.Context) string {
	if p := ScopePrefixFromCtx(ctx); p != "" {
		return p
	}
	return "agent:" + a.Identity.Agent
}

func (a *Actor) egress(ctx context.Context, channel, replyTo, text string, attachments []media.Part, inReplyTo string) (string, bool) {
	journal := func(status, errStr string) {
		a.writeEgressJournal(EgressRecord{
			SessionID:   a.Identity.SessionID,
			Agent:       a.Identity.Agent,
			Channel:     channel,
			ReplyTo:     replyTo,
			UserUUID:    PersonalUUIDFromCtx(ctx),
			Text:        text,
			Attachments: attachments,
			InReplyTo:   inReplyTo,
			Status:      status,
			Err:         errStr,
		})
	}
	if a.Media != nil && len(attachments) > 0 {
		norm, err := a.Media.NormalizeBorder(ctx, a.mediaScope(ctx), attachments)
		if err != nil {
			a.log.Warn("egress media refused", "err", err)
			journal("failed", err.Error())
			a.trace(TraceEvent{Kind: "reply", Turn: inReplyTo, Err: err.Error()})
			r, _ := jsonString(err.Error())
			return r, false
		}
		attachments = norm
	}
	text, allowed := a.checkEgress(ctx, channel, replyTo, text, inReplyTo, journal)
	if !allowed {
		return egressBlocked, false
	}
	if a.gw != nil {
		if err := a.gw.Send(channel, replyTo, text, attachments); err != nil {
			a.log.Warn("egress failed", "err", err)
			journal("failed", err.Error())
			a.trace(TraceEvent{Kind: "reply", Turn: inReplyTo, Err: err.Error()})
			r, _ := jsonString(err.Error())
			return r, false
		}
	}
	journal("delivered", "")
	ev := TraceEvent{Kind: "reply", Turn: inReplyTo, OK: true}
	if a.TracePayloads {
		ev.Text = truncateTrace(text)
	}
	a.trace(ev)
	return "true", true
}

// egressBlocked is the op result for text the egress safety chain refused.
const egressBlocked = `"reply blocked by safety filter"`

// checkEgress runs the egress safety chain and the refusal/fallback policy.
// It returns the text to send and whether sending is allowed; a drop journals
// through the caller's callback and delivers the profile's fallback, if any.
func (a *Actor) checkEgress(ctx context.Context, channel, replyTo, text, inReplyTo string, journal func(status, errStr string)) (string, bool) {
	if a.safety == nil || text == "" {
		return text, true
	}
	res := a.safety.Egress(ctx, safety.EgressInput{
		Text:      text,
		Source:    "agent",
		SessionID: a.Identity.SessionID,
	})
	if res.Drop {
		a.log.Info("safety egress dropped reply", "reason", res.Reason)
		metrics.Inc("agentflow_safety_drops")
		journal("blocked_safety", res.Reason)
		a.trace(TraceEvent{Kind: "safety", Op: "egress", Turn: inReplyTo, Err: res.Reason, Safety: res.Meta})
		if res.Fallback != "" && a.gw != nil && a.allowRefusal() {
			if err := a.gw.Send(channel, replyTo, res.Fallback, nil); err != nil {
				a.log.Warn("safety fallback send failed", "err", err, "channel", channel)
			} else {
				a.writeEgressJournal(EgressRecord{
					SessionID: a.Identity.SessionID,
					Agent:     a.Identity.Agent,
					Channel:   channel,
					ReplyTo:   replyTo,
					UserUUID:  PersonalUUIDFromCtx(ctx),
					Text:      res.Fallback,
					InReplyTo: inReplyTo,
					Status:    "blocked_safety",
					Err:       res.Reason,
				})
			}
		}
		return "", false
	}
	if res.Text != text || res.Meta != nil {
		a.trace(TraceEvent{Kind: "safety", Op: "egress", Turn: inReplyTo, OK: true, Err: res.Reason, Safety: res.Meta})
	}
	return res.Text, true
}

// closeTurn is the one end-of-turn path: it releases any buffered deltas, then
// tells the channel the turn is over exactly once — success, error, crash or
// cancellation. The channel is what the HTTP/SSE side waits on; without this a
// turn that ends without a reply would park until its timeout.
func (a *Actor) closeTurn(ctx context.Context, m *Message, turnErr error) {
	if m == nil || m.ID == "" {
		return
	}
	if a.turnEndedFor == m.ID {
		return
	}
	a.turnEndedFor = m.ID
	a.touch()
	if a.gw == nil || m.Channel == "" {
		a.resetTurnDeltas()
		return
	}
	a.flushTurnDeltas(ctx, m, turnErr)
	if turnErr == nil && a.turnBlocked.Load() && !a.turnReplied.Load() {
		// Safety refused what this turn tried to say and the loop sent no
		// structured reply. Say so in one: on a channel that reports a finish
		// reason the client sees content_filter, and on any other channel an
		// empty reply is nothing to deliver.
		if err := a.gw.SendReply(m.Channel, m.ReplyTo, gateway.Reply{FinishReason: "content_filter"}); err != nil {
			a.log.Warn("content_filter reply failed", "err", err, "channel", m.Channel)
		}
	}
	a.gw.EndTurn(m.Channel, m.ReplyTo, turnErr)
}

// flushTurnDeltas finishes a turn that produced text without session.reply.
// Live fragments already reached the channel; buffered ones (D9) are released
// here as one fragment after the whole answer is checked. A turn that failed
// releases nothing — partial text is not an answer — but it stays in the audit
// trail with the error.
func (a *Actor) flushTurnDeltas(ctx context.Context, m *Message, turnErr error) {
	a.deltaMu.Lock()
	all := a.turnDeltaText.String()
	pending := a.pendingDeltas.String()
	a.turnDeltaText.Reset()
	a.pendingDeltas.Reset()
	a.deltaMu.Unlock()
	if all == "" {
		return
	}
	if a.turnReplied.Load() {
		// The structured reply carries the answer and was journaled by replyOp;
		// relaying the fragments as well would duplicate it.
		return
	}
	record := EgressRecord{
		SessionID: a.Identity.SessionID,
		Agent:     a.Identity.Agent,
		Channel:   m.Channel,
		ReplyTo:   m.ReplyTo,
		UserUUID:  PersonalUUIDFromCtx(ctx),
		Text:      all,
		InReplyTo: m.ID,
	}
	if turnErr != nil {
		record.Status = "failed"
		record.Err = turnErr.Error()
		a.writeEgressJournal(record)
		return
	}
	text := all
	reason := ""
	blocked := false
	if a.safety != nil {
		res := a.safety.Egress(ctx, safety.EgressInput{
			Text:      text,
			Source:    "agent",
			SessionID: a.Identity.SessionID,
		})
		if res.Drop {
			blocked = true
			reason = res.Reason
			a.turnBlocked.Store(true)
			metrics.Inc("agentflow_safety_drops")
			a.trace(TraceEvent{Kind: "safety", Op: "egress", Turn: m.ID, Err: res.Reason, Safety: res.Meta})
		} else {
			if res.Text != text || res.Meta != nil {
				a.trace(TraceEvent{Kind: "safety", Op: "egress", Turn: m.ID, OK: true, Err: res.Reason, Safety: res.Meta})
			}
			text = res.Text
		}
	}
	if pending != "" && !blocked && text != "" {
		if err := a.gw.SendDelta(m.Channel, m.ReplyTo, text); err != nil {
			a.log.Warn("delta flush failed", "err", err, "channel", m.Channel)
			record.Status = "failed"
			record.Err = err.Error()
			a.writeEgressJournal(record)
			return
		}
	}
	record.Status = "delivered"
	if blocked {
		record.Status = "blocked_safety"
		record.Err = reason
		record.Text = ""
	}
	a.writeEgressJournal(record)
}

// resetTurnDeltas clears the per-turn delta accumulators. Caller starts a turn.
func (a *Actor) resetTurnDeltas() {
	a.deltaMu.Lock()
	a.turnDeltaText.Reset()
	a.pendingDeltas.Reset()
	a.deltaMu.Unlock()
	a.setTurnDirectives(nil)
}

func (a *Actor) setTurnDirectives(dirs []string) {
	a.directivesMu.Lock()
	if len(dirs) == 0 {
		a.turnDirectives = nil
	} else {
		a.turnDirectives = append([]string(nil), dirs...)
	}
	a.directivesMu.Unlock()
}

func (a *Actor) snapshotTurnDirectives() []string {
	a.directivesMu.Lock()
	defer a.directivesMu.Unlock()
	if len(a.turnDirectives) == 0 {
		return nil
	}
	return append([]string(nil), a.turnDirectives...)
}

func injectSafetyDirectives(msgs []ChatMessage, dirs []string) []ChatMessage {
	if len(dirs) == 0 {
		return msgs
	}
	directiveMsg := ChatMessage{
		Role:    "system",
		Content: strings.Join(dirs, "\n"),
	}
	insertAt := 0
	for insertAt < len(msgs) && msgs[insertAt].Role == "system" {
		insertAt++
	}
	out := make([]ChatMessage, 0, len(msgs)+1)
	out = append(out, msgs[:insertAt]...)
	out = append(out, directiveMsg)
	out = append(out, msgs[insertAt:]...)
	return out
}

// streamsLive reports whether deltas may go to the channel as they arrive. D9:
// if the profile can still drop the answer, the engine waits until the whole
// text exists and checks it once; otherwise a stream can be relayed live.
func (a *Actor) streamsLive(current *Message) bool {
	if a.gw == nil || current == nil || current.Channel == "" {
		return false
	}
	return a.safety == nil || !a.safety.CanDropEgress()
}

// deltaSink builds the relay function stamped into the turn's op context.
func (a *Actor) deltaSink(current *Message) DeltaSink {
	if current == nil || current.Channel == "" {
		return nil
	}
	return func(text string) error { return a.emitDelta(current, text) }
}

// emitDelta records one fragment and, when the turn may stream live, sends it.
// A buffered fragment is held until closeTurn, which checks the whole answer.
func (a *Actor) emitDelta(current *Message, text string) error {
	if text == "" {
		return nil
	}
	live := a.streamsLive(current)
	a.deltaMu.Lock()
	a.turnDeltaText.WriteString(text)
	if !live {
		a.pendingDeltas.WriteString(text)
	}
	a.deltaMu.Unlock()
	if !live {
		return nil
	}
	return a.gw.SendDelta(current.Channel, current.ReplyTo, text)
}

// deltaOp is session.send_delta: one fragment of the answer. The actor decides
// whether it can go now (see streamsLive), so the loop never has to know.
func (a *Actor) deltaOp(current *Message, op Op) (string, bool) {
	if current == nil || current.Channel == "" {
		return `"session.send_delta needs an active inbound message"`, false
	}
	if a.turnReplied.Load() {
		return `"session.send_delta: this turn already has a reply"`, false
	}
	if err := a.emitDelta(current, op.Text); err != nil {
		r, _ := jsonString(err.Error())
		return r, false
	}
	return "true", true
}

// replyOp is session.reply: finish the turn with a structured assistant
// message. The text goes through the same safety and journal path as
// session.send; tool calls are not text-screened (the filters are text-only by
// design). A blocked answer is delivered as an empty content_filter reply —
// the HTTP contract's way of saying "the model produced something the safety
// profile refused to show".
func (a *Actor) replyOp(ctx context.Context, current *Message, op Op) (string, bool) {
	if current == nil || current.Channel == "" {
		return `"session.reply needs an active inbound message"`, false
	}
	if !a.turnReplied.CompareAndSwap(false, true) {
		return `"session.reply: a reply was already sent this turn"`, false
	}
	text := op.Text
	journal := func(status, errStr string) {
		a.writeEgressJournal(EgressRecord{
			SessionID: a.Identity.SessionID,
			Agent:     a.Identity.Agent,
			Channel:   current.Channel,
			ReplyTo:   current.ReplyTo,
			UserUUID:  PersonalUUIDFromCtx(ctx),
			Text:      text,
			InReplyTo: current.ID,
			Status:    status,
			Err:       errStr,
		})
	}
	var allowed bool
	text, allowed = a.checkEgress(ctx, current.Channel, current.ReplyTo, text, current.ID, journal)
	// Usage is reported either way: the tokens were spent whether or not the
	// answer may be shown. Reasoning is not: a refused answer's reasoning is
	// the same content by another route.
	rep := gateway.Reply{FinishReason: op.FinishReason}
	if op.Usage != nil {
		rep.Usage = &gateway.Usage{
			Input: op.Usage.Input, Output: op.Usage.Output, Cached: op.Usage.Cached,
			CacheWrite: op.Usage.CacheWrite, Reasoning: op.Usage.Reasoning,
		}
	}
	if allowed {
		rep.Text = text
		rep.Reasoning = op.Reasoning
		for _, tc := range op.ToolCalls {
			args := "{}"
			if raw := strings.TrimSpace(tc.Arguments); raw != "" && json.Valid([]byte(raw)) {
				// The model's own text: what a pass-through must return.
				args = raw
			} else if tc.Args != nil {
				if b, err := json.Marshal(tc.Args); err == nil {
					args = string(b)
				}
			}
			rep.ToolCalls = append(rep.ToolCalls, gateway.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: args})
		}
		if rep.FinishReason == "" {
			if len(rep.ToolCalls) > 0 {
				rep.FinishReason = "tool_calls"
			} else {
				rep.FinishReason = "stop"
			}
		}
	} else {
		rep.FinishReason = "content_filter"
	}
	if a.gw == nil {
		return "true", true
	}
	if err := a.gw.SendReply(current.Channel, current.ReplyTo, rep); err != nil {
		a.log.Warn("reply delivery failed", "err", err)
		journal("failed", err.Error())
		r, _ := jsonString(err.Error())
		return r, false
	}
	if allowed {
		journal("delivered", "")
	}
	ev := TraceEvent{Kind: "reply", Turn: current.ID, OK: true}
	if a.TracePayloads {
		ev.Text = truncateTrace(rep.Text)
	}
	a.trace(ev)
	return "true", true
}

// turnModel is the model this turn runs on: the agent's, unless the person the
// turn acts for has a profile override.
func (a *Actor) turnModel(ctx context.Context) string {
	if u := PersonalUUIDFromCtx(ctx); u != "" && a.profileSettings != nil {
		if m, _, err := a.profileSettings.Settings(ctx, u); err == nil && m != "" {
			return m
		}
	}
	return a.Info.Model
}

// configJSON answers the agent.config op: this agent's own profile as Lua
// data — name, model, the instructions path (or prompt key), the goal block,
// any other profile extras, and the deployment's resolved prompt registry.
// Secrets never appear: every string in the extras tree that is a whole
// secret reference renders as the opaque marker {env="VAR"} or
// {cred="service"}; resolved values never cross the Lua bridge.
func (a *Actor) configJSON() string {
	extras := map[string]any{}
	var goal any
	for k, v := range a.Info.Extras {
		if k == "goal" {
			goal = config.OpaqueValue(v)
			continue
		}
		extras[k] = config.OpaqueValue(v)
	}
	// The prompt registry is always a table, even when empty, so a loop can
	// index cfg.prompts without a nil check. Snapshot copies under the
	// registry's lock: a file-backed prompt may be rewritten by the reload
	// watcher between here and the marshal.
	prompts := a.Info.Prompts.Snapshot()
	out := map[string]any{
		"name":              a.Info.Name,
		"model":             a.Info.Model,
		"instructions_path": a.Info.InstructionsPath,
		"prompts":           prompts,
		"extras":            extras,
	}
	if a.Info.InstructionsPrompt != "" {
		out["instructions_prompt"] = a.Info.InstructionsPrompt
	}
	if goal != nil {
		out["goal"] = goal
	}
	r, _ := jsonString(out)
	return r
}

// infoJSON is what a loop reads for its own identity: agent.info(). It is
// built per call rather than once, because two of its fields depend on *who* is
// being served — see the per-user overrides below.
func (a *Actor) infoJSON(ctx context.Context) string {
	instructions := ""
	if a.Info.Instructions != nil {
		instructions = a.Info.Instructions.Load()
	}
	budget := a.Info.HistoryBudget
	if budget <= 0 {
		budget = 6000
	}
	model := a.Info.Model
	mem := map[string]any{}
	if a.Info.Memory != nil {
		stores := map[string]any{}
		for name, bind := range a.Info.Memory.Stores {
			stores[name] = map[string]any{
				"backend":  bind.Backend,
				"table":    bind.Table,
				"features": bind.Features,
				// Where the store's data lives: deployment, region or instance.
				// A durable conversation belongs on a shared locality; an
				// instance-local store is a cache that dies with the process.
				"locality": string(bind.Locality),
			}
		}
		mem["stores"] = stores
		mem["write"] = a.Info.Memory.Write
		mem["recall"] = a.Info.Memory.Recall
		mem["embed_model"] = a.Info.Memory.EmbedModel
		mem["rerank_model"] = a.Info.Memory.RerankModel
		mem["oversample"] = a.Info.Memory.Oversample
	}

	// Per-user overrides. A profile may name a model and may add a layer of
	// instructions to the agent's own. Merging them *here* is what makes them
	// engine-enforced for every loop that follows the documented idiom — this is
	// the model and the system prompt a loop reads — without a line of Lua
	// changing. It is a default for this person, not a veto: a loop that names a
	// model explicitly on a single call still gets the model it asked for, and
	// the agent's own model stays visible as agent_model.
	var overrideModel, overrideInstructions string
	if u := PersonalUUIDFromCtx(ctx); u != "" && a.profileSettings != nil {
		if m, added, err := a.profileSettings.Settings(ctx, u); err == nil {
			overrideModel, overrideInstructions = m, added
		}
	}

	out := map[string]any{
		"name":           a.Info.Name,
		"session_id":     a.Identity.SessionID,
		"address":        "session:" + a.Identity.SessionID,
		"model":          model,
		"instructions":   instructions,
		"history_budget": budget,
		"memory":         mem,
		"shell":          a.Info.Shell,
		"skills":         a.Info.Skills,
		"capabilities":   a.Info.Capabilities,
	}
	if overrideModel != "" {
		out["agent_model"] = model
		out["model"] = overrideModel
	}
	if overrideInstructions != "" {
		out["instructions_append"] = overrideInstructions
		out["instructions"] = layerInstructions(instructions, overrideInstructions)
	}
	r, _ := jsonString(out)
	return r
}

// layerInstructions appends a profile's instructions to the agent's as their own
// paragraph, so the agent's operating rules stay first and authoritative and the
// person's layer reads as what it is.
func layerInstructions(base, added string) string {
	switch {
	case added == "":
		return base
	case base == "":
		return added
	default:
		return base + "\n\n" + added
	}
}

// opContext stamps the per-turn facts an op handler reads: the tenant the turn
// belongs to, the session key, and the provenance kind. The registered
// handlers get it through the default branch, and the few ops the actor serves
// itself — agent.info — call it directly, so both paths agree on what a handler
// can see.
func (a *Actor) opContext(ctx context.Context, current *Message) context.Context {
	ctx = a.stampIdentity(ctx, current)
	ctx = WithSessionKey(ctx, a.Identity.SessionID)
	ctx = WithTurnMessage(ctx, current)
	if a.Info != nil {
		ctx = WithAgentModel(ctx, a.Info.Model)
	}
	return WithProvenanceKind(ctx, provenanceKindOf(current))
}

// stampIdentity stamps the turn's acting identity, derived from the inbound
// message's core-stamped provenance (never from the payload — see
// actingIdentityOf) and this agent's inherit_user policy. A message whose
// provenance says nothing at all leaves no identity on the context: that is the
// third state, unknown, as opposed to a turn known to have no tenant.
func (a *Actor) stampIdentity(ctx context.Context, current *Message) context.Context {
	id, known := actingIdentityOf(current, a.Info.InheritsUser())
	if !known {
		return ctx
	}
	return WithActingIdentity(ctx, id)
}

// fairnessKey is the op-pool fairness key for a turn: the tenant's personal
// uuid, so every op a person's fleet issues shares one queue and one turn of
// the pool's rotation. Group context still keys on the personal uuid — the
// pool isolates tenants, not scopes, and billing already follows the person.
// "" (no tenant: unknown or known-empty identity) rides the shared queue.
func (a *Actor) fairnessKey(current *Message) string {
	inherit := true
	if a.Info != nil {
		inherit = a.Info.InheritsUser()
	}
	id, _ := actingIdentityOf(current, inherit)
	return id.Personal
}

// SetProfileSettings installs the per-user override lookup. Called by the
// supervisor right after the actor is built, alongside the other late-bound
// dependencies; nil leaves every user on the agent's own model and prompts.
func (a *Actor) SetProfileSettings(p ProfileSettings) { a.profileSettings = p }

// waitInbox blocks for the next message, a reload signal, or shutdown.
func (a *Actor) waitInbox(ctx context.Context) (Message, bool) {
	a.busy.Store(false) // idle while parked on the mailbox
	for {
		select {
		case m := <-a.Mailbox:
			a.busy.Store(true)
			a.touch()
			return m, true
		case seen := <-a.unloadReq:
			// Stale if anything happened since the janitor looked, or if a
			// message is already waiting (select picks at random among ready
			// cases, so both can be ready at once).
			if seen != a.lastActivity.Load() || len(a.Mailbox) > 0 {
				continue
			}
			a.unloaded.Store(true)
			return Message{}, false
		case <-a.reload:
			a.log.Info("hot reload: restarting loop")
			return Message{}, false
		case <-ctx.Done():
			return Message{}, false
		}
	}
}

// waitInboxOrTimeout is waitInbox with an extra escape: a deadline, reported as
// expired rather than as a stop. It exists for the confirmation wait — an
// unanswered prompt used to park the actor, and the suspended Luau coroutine
// with it, until the process shut down. A confirm-gated tool invoked on a
// scheduler turn hits that every time, because a timer message carries no
// channel to reply on and nobody is there to answer.
func (a *Actor) waitInboxOrTimeout(ctx context.Context, d time.Duration) (Message, bool, bool) {
	t := time.NewTimer(d)
	defer t.Stop()
	a.busy.Store(false) // idle while parked on the mailbox
	select {
	case m := <-a.Mailbox:
		a.busy.Store(true)
		a.touch()
		return m, true, false
	case <-t.C:
		return Message{}, false, true
	case <-a.reload:
		a.log.Info("hot reload: restarting loop")
		return Message{}, false, false
	case <-ctx.Done():
		return Message{}, false, false
	}
}

func sleepOr(ctx context.Context, reload chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-reload:
		return false
	case <-ctx.Done():
		return false
	}
}

// ingressSource maps the internal message type to the safety chain's source
// vocabulary. Channel drivers stamp Type:"user"; timers stamp Type:"timer".
func ingressSource(typ string) string {
	switch typ {
	case "user":
		return "channel"
	case "timer":
		return "scheduler"
	case "shell.result":
		// A background job's result is engine-made, like a system message; its
		// payload is the job's output, which the loop asked for.
		return "system"
	default:
		return typ
	}
}

func levelOf(l string) slog.Level {
	switch l {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func jsonString(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

type ownerKeyType struct{}

var ownerKey ownerKeyType

// WithOwner returns a context carrying the session key. Shell and confirm
// handlers extract this to enforce per-session ownership.
func WithOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, ownerKey, owner)
}

// OwnerFromCtx extracts the session key injected by WithOwner.
func OwnerFromCtx(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v := ctx.Value(ownerKey)
	s, _ := v.(string)
	return s
}

type shellKeyType struct{}

var shellKey shellKeyType

// WithShell returns a context carrying the session agent's bound shell
// profile (nil when unbound). The registry shell tools (builtin:shell.*)
// resolve it to lazily spawn or reuse the session's handle.
func WithShell(ctx context.Context, profile map[string]any) context.Context {
	return context.WithValue(ctx, shellKey, profile)
}

// ShellFromCtx extracts the bound shell profile injected by WithShell, or nil.
func ShellFromCtx(ctx context.Context) map[string]any {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(shellKey).(map[string]any)
	return v
}

// ActingIdentity is the tenant identity of one turn, stamped by the actor at
// dispatch from the inbound message's core-stamped provenance — never from Lua
// (§7). One string used to do three jobs; in group context they part company,
// so a consumer must name the one it means and the accessors below are the only
// way to read it.
type ActingIdentity struct {
	// Personal is the person's own uuid — the profile behind the turn. It keys
	// the credential store and the usage ledger, both of which are per person.
	Personal string
	// Membership is the group-context uuid, membership_uuid(U, G), when the turn
	// acts in a group; "" in personal context. It is the scope uuid there and is
	// recorded for attribution, but is never billed on.
	Membership string
	// ScopePrefix is the resolved scope prefix for this turn ("user:<uuid>"), or
	// "" when the turn has no user scope at all. Scope sites use it verbatim
	// rather than rebuilding it, so group context reaches them unchanged.
	ScopePrefix string
}

// ScopeUUID is the uuid scope resolution keys on: the person's own uuid
// normally, the membership uuid in group context. The two spaces are disjoint
// by construction (§5.6 consequence 1), which is what removes the need for a
// separate group: scope tier.
func (id ActingIdentity) ScopeUUID() string {
	if id.Membership != "" {
		return id.Membership
	}
	return id.Personal
}

type actingKeyType struct{}

var actingKey actingKeyType

// WithActingIdentity returns a context carrying the turn's acting identity. It
// is stamped by the actor from the core-owned inbound message, so a handler
// resolves a scope or a credential from something a loop cannot mint.
func WithActingIdentity(ctx context.Context, id ActingIdentity) context.Context {
	return context.WithValue(ctx, actingKey, id)
}

// WithPersonalIdentity returns a context carrying a personal-context acting
// identity for one user uuid: that person is the scope, the billing identity
// and the credential identity at once. It is the shorthand for the case that
// exists today (no group context yet).
func WithPersonalIdentity(ctx context.Context, userUUID string) context.Context {
	return WithActingIdentity(ctx, PersonalIdentity(userUUID))
}

// PersonalIdentity builds the personal-context acting identity of one user.
func PersonalIdentity(userUUID string) ActingIdentity {
	if userUUID == "" {
		return ActingIdentity{}
	}
	return ActingIdentity{Personal: userUUID, ScopePrefix: "user:" + userUUID}
}

// ActingFromCtx returns the acting identity stamped at dispatch. ok is false
// when none was stamped at all — unknown, which is a different thing from a
// stamped identity that carries no uuid (known-empty: an unlinked handle).
func ActingFromCtx(ctx context.Context) (ActingIdentity, bool) {
	if ctx == nil {
		return ActingIdentity{}, false
	}
	id, ok := ctx.Value(actingKey).(ActingIdentity)
	return id, ok
}

// ScopeUUIDFromCtx returns the uuid the memory, file and shell scopes key on.
// A scope site must name this one and not the person: in group context they are
// different uuids, and only the scope uuid is isolated per (person, group).
func ScopeUUIDFromCtx(ctx context.Context) string {
	id, _ := ActingFromCtx(ctx)
	return id.ScopeUUID()
}

// BillingUUIDFromCtx returns the uuid the usage ledger and the quota are keyed
// by. It is always the personal uuid: group context must not fragment the
// ledger, because the ledger is the billing mechanism (F23).
func BillingUUIDFromCtx(ctx context.Context) string {
	id, _ := ActingFromCtx(ctx)
	return id.Personal
}

// CredentialUUIDFromCtx returns the uuid the credential store is keyed by, and
// the person the user.* surface reports on. Always the personal uuid: a
// tenant's API keys are per person, not per (person, group).
func CredentialUUIDFromCtx(ctx context.Context) string {
	id, _ := ActingFromCtx(ctx)
	return id.Personal
}

// PersonalUUIDFromCtx returns the person behind the turn. It is the identity
// for person-level surfaces that are neither billing nor credentials: the
// per-user profile overrides, and the tenant attribution on an audit record.
func PersonalUUIDFromCtx(ctx context.Context) string {
	id, _ := ActingFromCtx(ctx)
	return id.Personal
}

// ScopePrefixFromCtx returns the resolved scope prefix ("user:<uuid>"), or ""
// when the turn has no user scope.
func ScopePrefixFromCtx(ctx context.Context) string {
	id, _ := ActingFromCtx(ctx)
	return id.ScopePrefix
}

type sessionKeyType struct{}

var sessionKey sessionKeyType

// WithSessionKey returns a context carrying the owning session key (the actor
// stamps it from Identity.SessionID at dispatch). Tools that need per-session
// state — scratch saves from builtin:fetch / builtin:browser — read it here;
// it is finer-grained than the agent-name owner stamp.
func WithSessionKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, sessionKey, key)
}

// SessionKeyFromCtx extracts the session key injected by WithSessionKey.
func SessionKeyFromCtx(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(sessionKey).(string)
	return s
}

// ProvenanceKindFromCtx extracts the provenance kind injected by
// WithProvenanceKind. It is core-assigned (never Lua-settable) — the memory
// scope layer trusts it to admit maintenance reads for engine-fired contexts.
func ProvenanceKindFromCtx(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(provenanceKey).(string)
	return s
}

// MaintenanceFromCtx reports whether the current dispatch came from an
// engine-fired context (system or scheduler provenance) — the agent's own
// maintenance loops, as opposed to a user's turn or an agent hop. The kind is
// core-assigned at dispatch, so a loop cannot forge it.
func MaintenanceFromCtx(ctx context.Context) bool {
	switch ProvenanceKindFromCtx(ctx) {
	case "system", "scheduler":
		return true
	}
	return false
}

// schedulerScopeFromCtx captures the turn's identity at timer-registration
// time. A timer created by a tenant runs as that tenant; a timer created by a
// tenantless service turn stays known-empty. It never becomes a maintenance
// context just because it fired later.
func schedulerScopeFromCtx(ctx context.Context) SchedulerScope {
	scope := SchedulerScope{Principal: "system:scheduler"}
	id, ok := ActingFromCtx(ctx)
	if !ok {
		return scope
	}
	u := id.Personal
	scope.UserUUID = &u
	if id.Membership != "" {
		m := id.Membership
		scope.MembershipUUID = &m
	}
	if key := SessionKeyFromCtx(ctx); key != "" {
		scope.Principal = "session:" + key
	}
	return scope
}

type provenanceKeyType struct{}

var provenanceKey provenanceKeyType

// WithProvenanceKind returns a context carrying the inbound message's
// provenance kind ("system", "scheduler", "agent", or "" for channel turns
// that carry none). Stamped by the actor at dispatch.
func WithProvenanceKind(ctx context.Context, kind string) context.Context {
	return context.WithValue(ctx, provenanceKey, kind)
}

// provenanceKindOf reads the core-assigned provenance kind of the current
// inbound message; channel-originated messages carry none ("").
func provenanceKindOf(m *Message) string {
	if m == nil || m.Provenance == nil {
		return ""
	}
	return m.Provenance.Kind
}

// actingIdentityOf derives a turn's acting identity from its inbound message.
//
// The message's core-stamped provenance is the ONLY authority. The payload is
// rebuilt from the caller's Lua table at every hop, so resolving identity from
// it would let a loop mint another tenant's scope — the forgery hole §4.2 #4
// names. The sender address is consulted only when the provenance says nothing
// at all: that is a message that never passed the identity layer (an
// identity-disabled runtime, whose drivers address a person as "user:<handle>").
//
// An explicit empty stamp means known-empty and never falls back: an unlinked
// handle's stable identity id lives in that same "user:" address form, and
// treating it as a tenant would give every anonymous handle a scope.
//
// inheritUser false makes an agent refuse a tenant that arrived over an agent
// hop (`inherit_user: false`). The person talking to that agent directly still
// reaches it; a delegating agent's tenant does not.
func actingIdentityOf(m *Message, inheritUser bool) (ActingIdentity, bool) {
	if m == nil {
		return ActingIdentity{}, false
	}
	if p := m.Provenance; p != nil && p.UserUUID != nil {
		if p.Kind == "agent" && !inheritUser {
			return ActingIdentity{}, true
		}
		id := PersonalIdentity(*p.UserUUID)
		// Group context: the engine stamped a validated membership uuid. The
		// scope uuid becomes the membership uuid and the scope prefix follows
		// it — the personal uuid stays billing and credentials (§7). An empty
		// stamp is known-none and is ignored, like the UserUUID "" state.
		if p.MembershipUUID != nil && *p.MembershipUUID != "" {
			id.Membership = *p.MembershipUUID
			id.ScopePrefix = "user:" + *p.MembershipUUID
		}
		return id, true
	}
	if from := m.From; from != "" {
		if u, ok := strings.CutPrefix(from, "user:"); ok && u != "" {
			return PersonalIdentity(u), true
		}
	}
	return ActingIdentity{}, false
}

// isConfirmRequest checks whether a response JSON encodes a needs_confirm=true result.
func isConfirmRequest(respJSON string) bool {
	var m map[string]any
	if err := json.Unmarshal([]byte(respJSON), &m); err != nil {
		return false
	}
	v, ok := m["needs_confirm"]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// doConfirm sends a confirm prompt to the user, blocks for a confirm reply,
// then re-dispatches the tools.run op with Confirmed=true if approved.
func (a *Actor) doConfirm(ctx context.Context, op Op, current *Message, respJSON string) (resp string, ok, proceed bool) {
	var ci struct {
		ConfirmID string `json:"confirm_id"`
		Tool      string `json:"tool"`
	}
	if err := json.Unmarshal([]byte(respJSON), &ci); err != nil {
		return `"confirm parse error"`, false, true
	}

	prompt := fmt.Sprintf(
		"⚠️ Confirm: run tool %q? Reply YES to proceed or NO to cancel [ref:%s]",
		ci.Tool, ci.ConfirmID,
	)
	if a.gw != nil && current != nil && current.Channel != "" {
		if err := a.gw.Send(current.Channel, current.ReplyTo, prompt, nil); err != nil {
			a.log.Warn("confirm prompt send failed", "err", err)
		}
	}
	a.log.Info("confirm prompt sent", "tool", ci.Tool, "confirm_id", ci.ConfirmID)

	// A prompt nobody answers must not park the session forever. Ten minutes is
	// long enough for a person to notice and short enough that a turn with no
	// one to answer it recovers on its own.
	const confirmWait = 10 * time.Minute

	var confirmed bool
	for {
		msg, goOn, expired := a.waitInboxOrTimeout(ctx, confirmWait)
		if expired {
			a.log.Warn("confirmation timed out; treating it as denied",
				"tool", ci.Tool, "confirm_id", ci.ConfirmID, "waited", confirmWait)
			r, _ := jsonString("tool invocation denied: no confirmation within " + confirmWait.String())
			return r, false, true
		}
		if !goOn {
			return "", false, false
		}
		if msg.Text == "" {
			continue
		}
		ref := fmt.Sprintf("[ref:%s]", ci.ConfirmID)
		if !strings.Contains(msg.Text, ref) {
			continue
		}
		upper := strings.ToUpper(msg.Text)
		confirmed = strings.Contains(upper, "YES")
		break
	}

	if !confirmed {
		r, _ := jsonString("tool invocation denied by user")
		a.log.Info("confirm denied", "tool", ci.Tool, "confirm_id", ci.ConfirmID)
		return r, false, true
	}

	a.log.Info("confirm approved; re-dispatching", "tool", ci.Tool)
	op.Confirmed = true
	op.ConfirmID = ci.ConfirmID

	type result struct {
		resp string
		ok   bool
	}
	resCh := pool.CallKeyed(a.pool, a.fairnessKey(current), func() result {
		r, o := a.execBlocking(ctx, op, current)
		return result{r, o}
	})
	select {
	case res := <-resCh:
		return res.resp, res.ok, true
	case <-a.reload:
		return "", false, false
	case <-ctx.Done():
		return "", false, false
	}
}
