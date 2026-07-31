package identity

import (
	"log/slog"

	"agentflow/internal/core/router"
)

// Sink is a router.Sink that mints a user identity for each inbound event
// before forwarding it to the inner router sink. It rewrites the envelope:
//
//   - From: the channel-native sender ("user:telegram:123") → "user:<uuid>"
//   - To:   "" → "agent:<bound agent>" (the agent the channel is configured for)
//   - Payload: adds "user_uuid" and "native_from" so a loop can recover them
//
// The native Channel/ReplyTo are left intact so session.send (reply to the
// current inbound) still works without any change. Failures to resolve are
// fail-open: the message is forwarded with its original From, so the runtime
// never blocks inbound traffic on the identity layer.
type Sink struct {
	inner router.Sink
	reg   *Registry
	log   *slog.Logger
}

// NewSink wraps an inner router sink with identity minting.
func NewSink(inner router.Sink, reg *Registry, log *slog.Logger) *Sink {
	return &Sink{inner: inner, reg: reg, log: log.With("module", "identity")}
}

// Submit implements router.Sink.
func (s *Sink) Submit(in router.Inbound) {
	native := in.Message.From
	uuid, err := s.reg.Resolve(in.Channel, native, in.Message.ReplyTo, in.Message.Payload)
	if err != nil {
		s.log.Warn("identity resolve failed; forwarding native From", "err", err, "native", native, "channel", in.Channel)
		s.inner.Submit(in)
		return
	}
	in.Message.From = "user:" + uuid
	in.Message.To = "agent:" + in.Agent
	if in.Message.Payload == nil {
		in.Message.Payload = map[string]any{}
	}
	// Copy to avoid mutating a payload map the driver may still hold; the
	// driver's payload is small and this is the inbound hot path, so a shallow
	// copy is enough. We only add keys, never overwrite existing ones.
	p := make(map[string]any, len(in.Message.Payload)+2)
	for k, v := range in.Message.Payload {
		p[k] = v
	}
	p["user_uuid"] = uuid
	p["native_from"] = native
	in.Message.Payload = p
	s.inner.Submit(in)
}
