// Package gateway is the channel registry: session replies are routed back
// to the channel the inbound message arrived on.
package gateway

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"agentflow/internal/core/media"
	"agentflow/internal/core/metrics"
)

// Driver is a live channel's delivery half (webhook, telegram, ...).
type Driver interface {
	Name() string
	// Deliver sends text (plus optional media attachments) to a
	// channel-specific target (chat id, pending webhook request id, ...).
	// A channel that cannot deliver media returns an error, never a silent
	// drop.
	Deliver(replyTo string, text string, attachments []media.Part) error
}

// ToolCall is one tool invocation in a structured reply. Arguments is the JSON
// encoding of the call's arguments — the wire form every OpenAI-shaped client
// concatenates.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Usage is a reply's token accounting, translated from llm.Usage. It is a
// wire-shaped copy rather than an import so the gateway stays free of the LLM
// driver.
type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	Cached     int `json:"cached"`
	CacheWrite int `json:"cache_write"`
	Reasoning  int `json:"reasoning"`
}

// Reply is a finished assistant message. Text is the answer; ToolCalls are the
// invocations the model requested; FinishReason is "stop", "tool_calls",
// "length" or "content_filter". Usage is nil when the provider reported none.
type Reply struct {
	Text         string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        *Usage
	Reasoning    string
}

// ReplyDriver is the optional structured half of a channel. A channel that does
// not implement it gets Deliver for the text, and an error for a reply that
// carries tool calls — a silent drop would look like a model that answered
// nothing.
type ReplyDriver interface {
	DeliverReply(replyTo string, r Reply) error
}

// DeltaDriver is the optional streaming half. A channel that does not implement
// it gets its deltas buffered in the registry and flushed on EndTurn, so a
// streaming loop still produces one complete text reply on every channel.
type DeltaDriver interface {
	DeliverDelta(replyTo string, text string) error
}

// TurnDriver is the optional end-of-turn signal. EndTurn is called exactly once
// per turn: success, error, crash or cancellation. A channel that does not
// implement it needs nothing from it (the fallbacks above finish on Deliver).
type TurnDriver interface {
	EndTurn(replyTo string, err error)
}

// Registry maps channel names to drivers.
type Registry struct {
	mu      sync.RWMutex
	drivers map[string]Driver
	log     *slog.Logger

	// deltaMu guards deltas, the fallback buffer for a channel without a
	// DeltaDriver: fragments accumulate here and are flushed as one text on
	// EndTurn. The key is channel + "\x00" + replyTo, which cannot collide
	// because a channel name never contains NUL.
	deltaMu sync.Mutex
	deltas  map[string]*strings.Builder
}

func NewRegistry(log *slog.Logger) *Registry {
	return &Registry{
		drivers: map[string]Driver{},
		deltas:  map[string]*strings.Builder{},
		log:     log.With("module", "gateway"),
	}
}

func (r *Registry) Register(d Driver) {
	r.mu.Lock()
	r.drivers[d.Name()] = d
	r.mu.Unlock()
	r.log.Info("channel registered", "channel", d.Name())
}

// Send delivers text to the message's origin channel. A reply to an unknown
// channel is an error the session should hear about (op failure), not a
// silent drop.
//
// It is also where egress is counted: every reply the engine delivers goes
// through here, so one pair of increments covers the whole egress path, and a
// failure is counted whether it is an unknown channel or a delivery error.
func (r *Registry) Send(channel, replyTo, text string, attachments []media.Part) error {
	metrics.Inc("agentflow_egress_total")
	r.mu.RLock()
	d, ok := r.drivers[channel]
	r.mu.RUnlock()
	if !ok {
		metrics.Inc("agentflow_egress_failed")
		metrics.Inc("agentflow_channel_errors")
		return fmt.Errorf("unknown channel %q", channel)
	}
	if err := d.Deliver(replyTo, text, attachments); err != nil {
		metrics.Inc("agentflow_egress_failed")
		metrics.Inc("agentflow_channel_errors")
		return err
	}
	return nil
}

// driver resolves a registered driver, counting the failure the way Send does.
func (r *Registry) driver(channel string) (Driver, error) {
	r.mu.RLock()
	d, ok := r.drivers[channel]
	r.mu.RUnlock()
	if !ok {
		metrics.Inc("agentflow_egress_failed")
		metrics.Inc("agentflow_channel_errors")
		return nil, fmt.Errorf("unknown channel %q", channel)
	}
	return d, nil
}

// SendReply delivers a finished assistant message. A driver that implements
// ReplyDriver gets the structure. A driver that does not gets the text through
// Deliver — unless the reply carries tool calls, which cannot be expressed as
// text and are refused rather than silently dropped.
func (r *Registry) SendReply(channel, replyTo string, rep Reply) error {
	metrics.Inc("agentflow_egress_total")
	d, err := r.driver(channel)
	if err != nil {
		return err
	}
	if rd, ok := d.(ReplyDriver); ok {
		if err := rd.DeliverReply(replyTo, rep); err != nil {
			metrics.Inc("agentflow_egress_failed")
			metrics.Inc("agentflow_channel_errors")
			return err
		}
		return nil
	}
	if len(rep.ToolCalls) > 0 {
		metrics.Inc("agentflow_egress_failed")
		metrics.Inc("agentflow_channel_errors")
		return fmt.Errorf("channel %q does not support structured replies (the reply carried %d tool calls)", channel, len(rep.ToolCalls))
	}
	if rep.Text == "" {
		// A reply with no text and no tool calls has nothing a text channel
		// can show (a refused answer is the usual case); an empty message
		// would only confuse the person on the other end.
		return nil
	}
	if err := d.Deliver(replyTo, rep.Text, nil); err != nil {
		metrics.Inc("agentflow_egress_failed")
		metrics.Inc("agentflow_channel_errors")
		return err
	}
	return nil
}

// SendDelta delivers one text fragment. A driver that implements DeltaDriver
// streams it; any other driver gets the fragments buffered and flushed as one
// text on EndTurn, which is what keeps a streaming loop correct on a channel
// that never learned about streams.
func (r *Registry) SendDelta(channel, replyTo, text string) error {
	d, err := r.driver(channel)
	if err != nil {
		return err
	}
	if dd, ok := d.(DeltaDriver); ok {
		if err := dd.DeliverDelta(replyTo, text); err != nil {
			metrics.Inc("agentflow_egress_failed")
			metrics.Inc("agentflow_channel_errors")
			return err
		}
		return nil
	}
	key := channel + "\x00" + replyTo
	r.deltaMu.Lock()
	b, ok := r.deltas[key]
	if !ok {
		b = &strings.Builder{}
		r.deltas[key] = b
	}
	b.WriteString(text)
	r.deltaMu.Unlock()
	return nil
}

// EndTurn ends one turn. Any buffered deltas are delivered first — they are the
// answer for a channel with no streaming support — and then a TurnDriver is
// told the turn is over, with the error that ended it, if any.
func (r *Registry) EndTurn(channel, replyTo string, turnErr error) {
	key := channel + "\x00" + replyTo
	r.deltaMu.Lock()
	b, ok := r.deltas[key]
	delete(r.deltas, key)
	r.deltaMu.Unlock()
	if ok && b.Len() > 0 {
		if err := r.Send(channel, replyTo, b.String(), nil); err != nil {
			r.log.Warn("buffered delta flush failed", "channel", channel, "reply_to", replyTo, "err", err)
		}
	}
	d, err := r.driver(channel)
	if err != nil {
		return
	}
	if td, ok := d.(TurnDriver); ok {
		td.EndTurn(replyTo, turnErr)
	}
}
