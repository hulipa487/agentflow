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
)

// Message is a single chat turn. Content is plain text. ToolCalls is set on
// an assistant turn that requested tools; ToolCallID and ToolResult are set
// on a "tool" role turn that carries a tool's result back to the model.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolResult any        `json:"tool_result,omitempty"`
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
	Tools       []ToolDef
	ToolChoice  string // "" | "auto" | "none"
}

// Usage counts tokens as reported by the provider (0 when unknown).
type Usage struct {
	Input  int `json:"input"`
	Output int `json:"output"`
}

// event is one normalized provider event: a text delta, terminal usage, an
// error, or the assembled tool_calls at stream end. Exactly one of the fields
// is meaningful per event; a closed channel means the stream is over.
type event struct {
	delta     string
	usage     Usage
	err       error
	toolCalls []ToolCall
}

// Manager resolves model names to provider clients and owns live streams.
type Manager struct {
	models map[string]config.Model
	http   *http.Client
	log    *slog.Logger

	seq     atomic.Uint64
	mu      sync.Mutex
	streams map[string]*Stream
}

func NewManager(models map[string]config.Model, log *slog.Logger) *Manager {
	return &Manager{
		models:  models,
		http:    &http.Client{}, // per-request ctx carries the timeout
		log:     log.With("driver", "llm"),
		streams: map[string]*Stream{},
	}
}

func (m *Manager) resolve(name string) (config.Model, error) {
	if name == "" {
		name = "default"
	}
	cfg, ok := m.models[name]
	if !ok {
		return config.Model{}, fmt.Errorf("unknown model %q", name)
	}
	return cfg, nil
}

// Chat performs a full completion and returns the buffered text plus any
// tool_calls the model requested at stream end.
func (m *Manager) Chat(ctx context.Context, model string, msgs []Message, opts Opts) (string, []ToolCall, Usage, error) {
	cfg, err := m.resolve(model)
	if err != nil {
		return "", nil, Usage{}, err
	}
	events, err := m.openWithRetry(ctx, cfg, msgs, opts)
	if err != nil {
		return "", nil, Usage{}, err
	}
	var text string
	var usage Usage
	var toolCalls []ToolCall
	for ev := range events {
		if ev.err != nil {
			return "", nil, usage, ev.err
		}
		text += ev.delta
		usage = ev.usage
		if len(ev.toolCalls) > 0 {
			toolCalls = ev.toolCalls
		}
	}
	return text, toolCalls, usage, nil
}

// Stream is a live completion; Next blocks for the next delta.
type Stream struct {
	ch     chan event
	cancel context.CancelFunc
}

// StreamOpen starts a completion and returns a stream id for StreamNext.
func (m *Manager) StreamOpen(ctx context.Context, model string, msgs []Message, opts Opts) (string, error) {
	cfg, err := m.resolve(model)
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

// StreamNext returns the next delta; done=true means the stream finished
// (usage and toolCalls are valid then). A finished stream unregisters itself.
func (m *Manager) StreamNext(ctx context.Context, id string) (delta string, done bool, usage Usage, toolCalls []ToolCall, err error) {
	m.mu.Lock()
	st, ok := m.streams[id]
	m.mu.Unlock()
	if !ok {
		return "", false, Usage{}, nil, fmt.Errorf("unknown stream %q", id)
	}
	select {
	case ev, open := <-st.ch:
		if !open {
			m.StreamClose(id)
			return "", true, Usage{}, nil, nil
		}
		if ev.err != nil {
			m.StreamClose(id)
			return "", true, ev.usage, nil, ev.err
		}
		if len(ev.toolCalls) > 0 {
			// Tool calls arrive once at stream end; treat as done so the
			// loop picks them up without waiting for the channel close.
			return "", true, ev.usage, ev.toolCalls, nil
		}
		return ev.delta, false, ev.usage, nil, nil
	case <-ctx.Done():
		return "", false, Usage{}, nil, ctx.Err()
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
