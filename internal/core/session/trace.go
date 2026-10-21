package session

import (
	"encoding/json"
	"time"

	"agentflow/internal/core/safety"
)

// TraceEvent is one step of a session's execution, emitted for an observing
// console (the standalone Studio app). It is observability data, not
// authority: nothing reads it back into the runtime.
//
// Kinds:
//
//	turn.start  an inbound message reached the loop (after safety ingress)
//	turn.end    the loop parked on the inbox, finishing the previous turn
//	op.start    an op was dispatched
//	op.end      the op returned (duration, ok/err, usage/tool-call summary)
//	reply       the session egressed a message
//	safety      a filter dropped or rewrote a message
//	error       the loop failed and will restart
//	reload      a hot reload restarted the loop
type TraceEvent struct {
	Seq             int64     `json:"seq"`
	Ts              time.Time `json:"ts"`
	Agent           string    `json:"agent"`
	SessionID       string    `json:"session"`
	Thread          string    `json:"thread,omitempty"`
	Turn            string    `json:"turn,omitempty"`
	Kind            string    `json:"kind"`
	Op              string    `json:"op,omitempty"`
	Tool            string    `json:"tool,omitempty"`
	Model           string    `json:"model,omitempty"`
	OK              bool      `json:"ok"`
	Err             string    `json:"err,omitempty"`
	DurationMS      int64     `json:"duration_ms,omitempty"`
	TokensIn        int       `json:"tokens_in,omitempty"`
	TokensOut       int       `json:"tokens_out,omitempty"`
	TokensReasoning int       `json:"tokens_reasoning,omitempty"`
	ToolCalls       int       `json:"tool_calls,omitempty"`
	Messages        int       `json:"messages,omitempty"`
	// Text carries the inbound/reply text and the request/result payloads.
	// It is populated only when the deployment opts into payload capture
	// (runtime.studio.trace_payloads); metadata is always safe to emit.
	Text string `json:"text,omitempty"`
	// Safety carries a classifier verdict. It never contains message text, so
	// it is emitted regardless of payload capture.
	Safety *safety.SafetyMeta `json:"safety,omitempty"`
}

// Tracer receives execution events. Implementations must be fast and
// non-blocking: they run on the session goroutine (or the op worker), between
// Lua steps.
type Tracer func(TraceEvent)

// maxTraceText bounds any payload captured in a trace event. The ring that
// stores events is bounded too; this keeps one large completion from crowding
// out the metadata of every other step.
const maxTraceText = 16 << 10

func truncateTrace(s string) string {
	if len(s) <= maxTraceText {
		return s
	}
	return s[:maxTraceText] + "…"
}

// trace emits one event. It never lets an observer panic take down a session.
func (a *Actor) trace(ev TraceEvent) {
	if a.Tracer == nil {
		return
	}
	ev.Agent = a.Identity.Agent
	ev.SessionID = a.Identity.SessionID
	if ev.Ts.IsZero() {
		ev.Ts = time.Now()
	}
	defer func() { _ = recover() }()
	a.Tracer(ev)
}

// traceOpEnd fills the token/tool-call summary every provider reply carries
// under the same JSON keys (caps/llm.go marshals Reply verbatim). Op errors
// are not JSON, so a parse failure is simply "no summary".
func traceOpEnd(ev *TraceEvent, resp string, ok bool) {
	ev.Kind = "op.end"
	ev.OK = ok
	if !ok {
		if resp != "" {
			// Handlers return failures as JSON strings; quote-stripping makes
			// the trace readable without losing a bare error.
			var s string
			if json.Unmarshal([]byte(resp), &s) == nil {
				ev.Err = truncateTrace(s)
			} else {
				ev.Err = truncateTrace(resp)
			}
		}
		return
	}
	switch ev.Op {
	case "llm.chat":
		var r struct {
			Usage struct {
				Input     int `json:"input"`
				Output    int `json:"output"`
				Reasoning int `json:"reasoning"`
			} `json:"usage"`
			ToolCalls []json.RawMessage `json:"tool_calls"`
		}
		if err := json.Unmarshal([]byte(resp), &r); err == nil {
			ev.TokensIn = r.Usage.Input
			ev.TokensOut = r.Usage.Output
			ev.TokensReasoning = r.Usage.Reasoning
			ev.ToolCalls = len(r.ToolCalls)
		}
	}
}

// traceOpPayload renders the observable input of an op for payload capture.
// It prefers a plain text argument, then the chat messages, then tool args —
// whichever the op actually carries.
func traceOpPayload(op Op) string {
	switch {
	case op.Text != "":
		return op.Text
	case len(op.Messages) > 0:
		if b, err := json.Marshal(op.Messages); err == nil {
			return string(b)
		}
	case len(op.Args) > 0:
		if b, err := json.Marshal(op.Args); err == nil {
			return string(b)
		}
	}
	return ""
}
