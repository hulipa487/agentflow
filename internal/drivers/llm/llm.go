// Package llm is the LLM driver: named model configurations behind a single
// streaming-first API. Providers: anthropic, openai, and openai-responses —
// each targeting Anthropic/OpenAI-compatible endpoints via base_url. All
// providers normalize into one event stream; Chat is just a buffered stream.
package llm

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/media"
)

// Message is a single chat turn. Content is plain text; Parts carries
// multimodal content (text + media descriptors) when set — the plain-text
// path is untouched for existing callers. ToolCalls is set on an assistant
// turn that requested tools; ToolCallID and ToolResult are set on a "tool"
// role turn that carries a tool's result back to the model. ThinkingBlocks
// on an assistant turn replays reasoning content captured from an earlier
// reply (Anthropic requires it on continuations when thinking is enabled);
// only the Anthropic serializer consumes it — providers without replay
// semantics ignore the field.
type Message struct {
	Role           string           `json:"role"`
	Content        string           `json:"content,omitempty"`
	Parts          []media.Part     `json:"parts,omitempty"`
	ToolCalls      []ToolCall       `json:"tool_calls,omitempty"`
	ToolCallID     string           `json:"tool_call_id,omitempty"`
	ToolResult     any              `json:"tool_result,omitempty"`
	ThinkingBlocks []map[string]any `json:"thinking_blocks,omitempty"`
}

// hasMedia reports whether any message in the list carries non-text parts.
func hasMedia(msgs []Message) bool {
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type != "text" {
				return true
			}
		}
	}
	return false
}

// dataURI renders a media part as a data: URL. Requires Data (base64) to be
// resolved; MIME defaults to application/octet-stream.
func dataURI(p media.Part) string {
	mime := p.MIME
	if mime == "" {
		mime = "application/octet-stream"
	}
	return "data:" + mime + ";base64," + p.Data
}

// ToolCall is one tool invocation the model requested. Args is the parsed
// arguments object.
type ToolCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// ToolDef is the provider-agnostic shape a loop passes in opts.Tools. Each
// provider reshapes it to its native request format in its open func.
type ToolDef struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// Opts are per-call overrides from Lua (llm.chat opts).
type Opts struct {
	Temperature *float64
	MaxTokens   int
	Thinking    string // "" | off | low | medium | high | xhigh | max (see Thinking)
	Tools       []ToolDef
	ToolChoice  string // "" | "auto" | "none"
}

// Usage counts tokens as reported by the provider (0 when unknown).
// Reasoning is the provider's separate count of thinking tokens where it
// reports one (OpenAI-shaped completion/output token details); Anthropic and
// Gemini fold thinking into Output, so it stays 0 there.
//
// Cached is prompt tokens served from the provider's prompt cache, billed at a
// discount; CacheWrite is tokens written into it. Only Anthropic bills cache
// creation separately (and is the only provider reporting it here) — elsewhere
// CacheWrite stays 0.
type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	Cached     int `json:"cached,omitempty"`
	CacheWrite int `json:"cache_write,omitempty"`
	Reasoning  int `json:"reasoning,omitempty"`
}

// Reply is one buffered completion: the assistant text plus everything the
// provider reported alongside it. Thinking is the reasoning content joined
// for reading ("" when the provider sent none); ThinkingBlocks are the raw
// provider blocks — defined only where the provider defines them (Anthropic
// thinking/redacted_thinking, Responses reasoning items, Gemini thought
// parts), nil on Chat Completions, where reasoning is a bare string. The
// blocks round-trip: passing them back as thinking_blocks on an assistant
// message replays them (Anthropic requires this for multi-round tool use).
type Reply struct {
	Text           string
	Thinking       string
	ThinkingBlocks []map[string]any
	ToolCalls      []ToolCall
	Usage          Usage
}

// Frame is one llm.stream.next step. Delta is a text fragment; on Done the
// terminal fields carry the full reply payload (Usage, ToolCalls, Thinking,
// ThinkingBlocks — the same values Chat would have returned).
type Frame struct {
	Delta          string
	Done           bool
	Usage          Usage
	ToolCalls      []ToolCall
	Thinking       string
	ThinkingBlocks []map[string]any
}

// event is one normalized provider event: a text delta, terminal usage, an
// error, or the assembled tool_calls at stream end. Exactly one of the fields
// is meaningful per event; a closed channel means the stream is over.
// thinking/thinkingBlocks are terminal-only: providers assemble reasoning
// internally and attach the full payload to their end-of-stream events, so
// consumers see it exactly once, with toolCalls and/or usage.
type event struct {
	delta          string
	usage          Usage
	err            error
	toolCalls      []ToolCall
	thinking       string
	thinkingBlocks []map[string]any
}

// Manager resolves model names to provider clients and owns live streams.
// The model set is hot-swappable (Upsert/Remove/SetAll, e.g. from the admin
// web UI): resolution happens per call, so an edit takes effect on the next
// request without touching live sessions.
type Manager struct {
	mmu    sync.RWMutex // guards models
	models map[string]config.Model
	http   *http.Client
	log    *slog.Logger

	seq     atomic.Uint64
	mu      sync.Mutex
	streams map[string]*Stream

	// resolveSecret lazily resolves a model api_key that is still a raw
	// reference (${VAR} / cred:<service>) — i.e. loaded via the configdir
	// path. Wired from main with a config.Resolver; nil means "treat every
	// key as a literal" (the legacy path expands at load, so keys arrive
	// literal). A failed resolution is NOT cached: the next call retries.
	resolveSecret func(raw string) (string, bool)
}

// SetSecretResolver installs the lazy api_key resolver (see the field).
func (m *Manager) SetSecretResolver(fn func(string) (string, bool)) {
	m.resolveSecret = fn
}

// resolveModel is resolve plus lazy api_key resolution. A model whose key
// cannot be resolved stays configured — the first LLM call fails here with
// an error naming the model and the unresolved credential, rather than the
// boot failing or a bare 401 reaching the provider.
func (m *Manager) resolveModel(name string) (config.Model, error) {
	cfg, err := m.resolve(name)
	if err != nil {
		return config.Model{}, err
	}
	if m.resolveSecret != nil && cfg.APIKey != "" {
		v, ok := m.resolveSecret(cfg.APIKey)
		if !ok {
			return config.Model{}, fmt.Errorf("model %q: cannot resolve api_key %q (set the env var or store the credential)", name, cfg.APIKey)
		}
		cfg.APIKey = v
	}
	return cfg, nil
}

func NewManager(models map[string]config.Model, log *slog.Logger) *Manager {
	return &Manager{
		models:  cloneModels(models),
		http:    &http.Client{}, // per-request ctx carries the timeout
		log:     log.With("driver", "llm"),
		streams: map[string]*Stream{},
	}
}

// cloneModels copies a model set, always returning a non-nil map so runtime
// Upsert can never assign into a nil map (a config with no models: key yields
// a nil map at boot).
func cloneModels(models map[string]config.Model) map[string]config.Model {
	out := make(map[string]config.Model, len(models))
	for k, v := range models {
		out[k] = v
	}
	return out
}

func (m *Manager) resolve(name string) (config.Model, error) {
	if name == "" {
		name = "default"
	}
	m.mmu.RLock()
	cfg, ok := m.models[name]
	m.mmu.RUnlock()
	if !ok {
		return config.Model{}, fmt.Errorf("unknown model %q", name)
	}
	return cfg, nil
}

// Upsert adds or replaces a named model at runtime. In-flight requests keep
// the config they resolved with; the next call sees the new one.
func (m *Manager) Upsert(name string, cfg config.Model) {
	m.mmu.Lock()
	m.models[name] = cfg
	m.mmu.Unlock()
}

// Remove deletes a named model. In-flight requests are unaffected.
func (m *Manager) Remove(name string) {
	m.mmu.Lock()
	delete(m.models, name)
	m.mmu.Unlock()
}

// SetAll replaces the entire model set (e.g. reverting to the config file's
// models: section).
func (m *Manager) SetAll(models map[string]config.Model) {
	m.mmu.Lock()
	m.models = cloneModels(models)
	m.mmu.Unlock()
}

// List returns a copy of the current model set, keyed by name.
func (m *Manager) List() map[string]config.Model {
	m.mmu.RLock()
	defer m.mmu.RUnlock()
	return cloneModels(m.models)
}

// Get returns the live config for a named model ("" resolves to "default").
func (m *Manager) Get(name string) (config.Model, error) { return m.resolve(name) }

// Chat performs a full completion and returns the buffered reply: text,
// tool_calls requested at stream end, reported usage, and whatever thinking
// content the provider sent (passive capture — surfaced, never requested
// beyond what the thinking level already asks for).
func (m *Manager) Chat(ctx context.Context, model string, msgs []Message, opts Opts) (*Reply, error) {
	cfg, err := m.resolveModel(model)
	if err != nil {
		return nil, err
	}
	events, err := m.openWithRetry(ctx, cfg, msgs, opts)
	if err != nil {
		return nil, err
	}
	r := &Reply{}
	for ev := range events {
		if ev.err != nil {
			return nil, ev.err
		}
		r.Text += ev.delta
		r.Usage = ev.usage
		if ev.thinking != "" {
			r.Thinking = ev.thinking
		}
		if ev.thinkingBlocks != nil {
			r.ThinkingBlocks = ev.thinkingBlocks
		}
		if len(ev.toolCalls) > 0 {
			r.ToolCalls = ev.toolCalls
		}
	}
	return r, nil
}

// Stream is a live completion; Next blocks for the next delta.
type Stream struct {
	ch     chan event
	cancel context.CancelFunc

	// Terminal payload captured from events as they pass through (usage and
	// tool calls may arrive on separate events before the close). Only
	// StreamNext touches these, and one loop calls it sequentially.
	usage    Usage
	calls    []ToolCall
	thinking string
	blocks   []map[string]any
}

// StreamOpen starts a completion and returns a stream id for StreamNext.
func (m *Manager) StreamOpen(ctx context.Context, model string, msgs []Message, opts Opts) (string, error) {
	cfg, err := m.resolveModel(model)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithCancel(ctx)
	events, err := m.openWithRetry(ctx, cfg, msgs, opts)
	if err != nil {
		cancel()
		return "", err
	}
	id := fmt.Sprintf("st-%d", m.seq.Add(1))
	st := &Stream{ch: make(chan event, 16), cancel: cancel}
	m.mu.Lock()
	m.streams[id] = st
	m.mu.Unlock()
	go func() {
		for ev := range events {
			st.ch <- ev
		}
		close(st.ch)
	}()
	return id, nil
}

// StreamNext returns the next frame; done=true means the stream finished
// (usage, toolCalls, and the terminal thinking payload are valid then). A
// finished stream unregisters itself.
//
// A tool-call event ends the turn but is not the last event on the stream:
// every provider emits it immediately *before* the usage frame. Returning Done
// there — which this used to do — reported the call as costing nothing, and
// usage feeds the per-user ledger and the agent budget. It also left the stream
// registered, because that path never called StreamClose, so the entry, its
// cancel func, the response body and the provider goroutine survived until the
// process exited. So: keep receiving, accumulate, and return Done from the
// close. A usage-only frame is swallowed for the same reason — it carries no
// delta, and an empty delta reads to a Lua `for` as end-of-stream.
func (m *Manager) StreamNext(ctx context.Context, id string) (Frame, error) {
	m.mu.Lock()
	st, ok := m.streams[id]
	m.mu.Unlock()
	if !ok {
		return Frame{}, fmt.Errorf("unknown stream %q", id)
	}
	for {
		select {
		case ev, open := <-st.ch:
			if !open {
				done := Frame{Done: true, Usage: st.usage, ToolCalls: st.calls, Thinking: st.thinking, ThinkingBlocks: st.blocks}
				m.StreamClose(id)
				return done, nil
			}
			if ev.usage.Input != 0 || ev.usage.Output != 0 {
				st.usage = ev.usage
			}
			if len(ev.toolCalls) > 0 {
				st.calls = ev.toolCalls
			}
			if ev.thinking != "" {
				st.thinking = ev.thinking
			}
			if ev.thinkingBlocks != nil {
				st.blocks = ev.thinkingBlocks
			}
			if ev.err != nil {
				done := Frame{Done: true, Usage: st.usage, ToolCalls: st.calls, Thinking: st.thinking, ThinkingBlocks: st.blocks}
				m.StreamClose(id)
				return done, ev.err
			}
			if ev.delta != "" {
				return Frame{Delta: ev.delta, Usage: st.usage}, nil
			}
			// A tool call, or a usage-only frame: no delta to hand back, so keep
			// draining rather than reporting an empty one.
		case <-ctx.Done():
			m.StreamClose(id)
			return Frame{}, ctx.Err()
		}
	}
}

// StreamClose cancels and unregisters a stream (idempotent).
func (m *Manager) StreamClose(id string) {
	m.mu.Lock()
	st, ok := m.streams[id]
	delete(m.streams, id)
	m.mu.Unlock()
	if ok {
		st.cancel()
	}
}

// openWithRetry establishes the event stream, retrying establishment
// failures (transport, 429, 5xx). Once events flow, errors are terminal.
func (m *Manager) openWithRetry(ctx context.Context, cfg config.Model, msgs []Message, opts Opts) (<-chan event, error) {
	retry := cfg.Retry
	if retry < 0 {
		retry = 0
	}
	var last error
	for attempt := 0; attempt <= retry; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(500<<uint(attempt-1)) * time.Millisecond
			m.log.Warn("llm: retrying", "model", cfg.Model, "attempt", attempt, "backoff", backoff, "err", last)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		m.log.Debug("llm: chat request", "provider", cfg.Provider, "model", cfg.Model, "messages", len(msgs), "media", hasMedia(msgs), "attempt", attempt)
		events, retryable, err := m.open(ctx, cfg, msgs, opts)
		if err == nil {
			return events, nil
		}
		last = err
		if !retryable {
			return nil, err
		}
	}
	return nil, last
}

// open dispatches to the provider. The bool marks the error retryable.
func (m *Manager) open(ctx context.Context, cfg config.Model, msgs []Message, opts Opts) (<-chan event, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.TimeoutD())
	events, retryable, err := openProvider(ctx, m.http, cfg, msgs, opts)
	if err != nil {
		cancel()
		return nil, retryable, err
	}
	// the event channel outlives this function; cancel when it closes
	out := make(chan event, 16)
	go func() {
		defer cancel()
		for ev := range events {
			out <- ev
		}
		close(out)
	}()
	return out, false, nil
}
