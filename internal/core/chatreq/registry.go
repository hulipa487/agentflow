// Package chatreq holds the parsed request behind an OpenAI-compatible turn.
//
// The whole body must not cross the router's Lua bridge: a coding client sends
// megabytes of history, and D6 keeps it in Go. The channel stores the parsed
// request here under a reference; the message carries only that reference, and
// the two Lua ops that need the request (llm.forward, openai.request) resolve it
// from the current turn's payload.
//
// A registry is per driver (per channel) and per process. A keyed session that
// moves to another instance between turns has no entry for its next request
// until that request itself lands here — the channel is the only writer, and
// the request that arrives with a turn is always registered before the message
// is delivered.
package chatreq

import (
	"sync"

	"github.com/google/uuid"

	"agentflow/internal/core/session"
)

// Request is one parsed Chat Completions request. Messages and Tools are the
// provider-neutral shapes the runtime already uses, so llm.forward hands them
// straight to the LLM driver without a second conversion.
type Request struct {
	Ref    string // registry key; the message payload's request_ref
	ID     string // request id; also the message id (idempotency may make it stable)
	Model  string
	Stream bool
	// IncludeUsage is stream_options.include_usage: the client wants a usage
	// chunk at the end of the stream.
	IncludeUsage bool

	Messages []session.ChatMessage
	Tools    []session.ToolSpec

	ToolChoice        string // "" | auto | none | required | function:<name>
	ParallelToolCalls *bool
	Temperature       *float64
	TopP              *float64
	MaxTokens         int
	Stop              []string
	Seed              *int64
	ResponseFormat    map[string]any

	// User and Metadata are the client's own fields, untrusted by design.
	User     string
	Metadata map[string]any
	// ConversationID is metadata.conversation_id or X-AgentFlow-Conversation.
	ConversationID string

	// Bytes is the request body's size, for the payload's metadata.
	Bytes int
}

// Params is the sampling-parameter view openai.request returns to Lua.
func (r *Request) Params() map[string]any {
	p := map[string]any{}
	if r.Temperature != nil {
		p["temperature"] = *r.Temperature
	}
	if r.TopP != nil {
		p["top_p"] = *r.TopP
	}
	if r.MaxTokens > 0 {
		p["max_tokens"] = r.MaxTokens
	}
	if len(r.Stop) > 0 {
		p["stop"] = r.Stop
	}
	if r.Seed != nil {
		p["seed"] = *r.Seed
	}
	if r.ResponseFormat != nil {
		p["response_format"] = r.ResponseFormat
	}
	if r.ParallelToolCalls != nil {
		p["parallel_tool_calls"] = *r.ParallelToolCalls
	}
	return p
}

// LuaView is the table openai.request returns: the full request, minus its
// handle. It is deliberately the client's own shape so a loop can inspect what
// was sent without a translation table in the docs.
func (r *Request) LuaView() map[string]any {
	out := map[string]any{
		"model":    r.Model,
		"stream":   r.Stream,
		"messages": r.Messages,
		"params":   r.Params(),
	}
	if len(r.Tools) > 0 {
		out["tools"] = r.Tools
	}
	if r.ToolChoice != "" {
		out["tool_choice"] = r.ToolChoice
	}
	if r.User != "" {
		out["user"] = r.User
	}
	if r.Metadata != nil {
		out["metadata"] = r.Metadata
	}
	return out
}

// Registry maps request refs to parsed requests for one channel driver. The
// driver removes an entry when its turn ends.
type Registry struct {
	mu    sync.Mutex
	byRef map[string]*Request
}

func NewRegistry() *Registry {
	return &Registry{byRef: map[string]*Request{}}
}

// Add stores a request and returns its ref. A request that already has one
// keeps it (an idempotent retry joins the running request).
func (r *Registry) Add(req *Request) string {
	if req.Ref == "" {
		req.Ref = "rq-" + uuid.NewString()
	}
	r.mu.Lock()
	r.byRef[req.Ref] = req
	r.mu.Unlock()
	return req.Ref
}

// Get resolves a ref.
func (r *Registry) Get(ref string) (*Request, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	req, ok := r.byRef[ref]
	return req, ok
}

// Remove drops a finished request.
func (r *Registry) Remove(ref string) {
	r.mu.Lock()
	delete(r.byRef, ref)
	r.mu.Unlock()
}

// Len reports how many requests are registered (tests and metrics).
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byRef)
}

// Reset clears the registry (the admin/refresh path, tests).
func (r *Registry) Reset() {
	r.mu.Lock()
	r.byRef = map[string]*Request{}
	r.mu.Unlock()
}
