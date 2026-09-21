package identity

import (
	"log/slog"
	"strings"
	"sync"

	"agentflow/internal/core/metrics"
	"agentflow/internal/core/router"
)

// linkCommand is the in-channel command that carries a link code. Only
// linkable channels (those whose sender identity the platform authenticates)
// have it interpreted.
const linkCommand = "/link"

// Sink is a router.Sink that resolves an identity (and its profile) for each
// inbound event before forwarding it to the inner router sink. It rewrites the
// envelope:
//
//   - From: the channel-native sender ("user:telegram:123") → "user:<profile>"
//     when the handle is linked, else "user:<identity>" — a stable handle id,
//     never the raw channel string, so the audit trail is uniform.
//   - To:   "" → "agent:<bound agent>" (the agent the channel is configured for)
//   - Payload: adds "user_uuid" (the scope stamp; empty for an unlinked
//     handle), "identity_id", "registered", "trust" and "native_from".
//
// The scope stamp is the *profile* id, so a person's memory, files and
// credentials follow them across every linked channel. For an unlinked handle
// it is deliberately empty: the actor then stamps no user on the context and
// the turn runs in the shared service stratum, which is what keeps an
// unclaimed handle from accumulating personal history.
//
// The native Channel/ReplyTo are left intact so session.send (reply to the
// current inbound) still works without any change. Failures to resolve are
// fail-open: the message is forwarded with its original From, so the runtime
// never blocks inbound traffic on the identity layer.
type Sink struct {
	inner router.Sink
	reg   *Registry
	log   *slog.Logger

	mu    sync.Mutex
	reply func(channel, replyTo, text string)
}

// NewSink wraps an inner router sink with identity resolution.
func NewSink(inner router.Sink, reg *Registry, log *slog.Logger) *Sink {
	return &Sink{inner: inner, reg: reg, log: log.With("module", "identity")}
}

// SetReply wires an in-channel reply path (the gateway), so a link attempt can
// be confirmed — or its failure explained — where the user is looking. It is
// optional: without it a link still completes, silently.
func (s *Sink) SetReply(fn func(channel, replyTo, text string)) {
	s.mu.Lock()
	s.reply = fn
	s.mu.Unlock()
}

func (s *Sink) say(in router.Inbound, text string) {
	s.mu.Lock()
	fn := s.reply
	s.mu.Unlock()
	if fn == nil {
		return
	}
	fn(in.Channel, in.Message.ReplyTo, text)
}

// Submit implements router.Sink.
func (s *Sink) Submit(in router.Inbound) {
	native := in.Message.From
	res, err := s.reg.Resolve(in.Channel, native, in.Message.ReplyTo, in.Message.Payload)
	if err != nil {
		s.log.Warn("identity resolve failed; forwarding native From", "err", err, "native", native, "channel", in.Channel)
		s.inner.Submit(in)
		return
	}

	// A link confirmation is an identity operation, not a chat message: it is
	// completed here and never reaches the router, the journal or a loop, so a
	// link code cannot leak into a prompt or a model's context.
	if res.Linkable {
		if code, ok := linkCode(in.Message.Text); ok {
			s.completeLink(in, res, native, code)
			return
		}
	}

	from := res.IdentityID
	if res.Registered() {
		from = res.UserID
	}
	in.Message.From = "user:" + from
	in.Message.To = "agent:" + in.Agent
	if in.Message.Payload == nil {
		in.Message.Payload = map[string]any{}
	}
	// Copy to avoid mutating a payload map the driver may still hold; the
	// driver's payload is small and this is the inbound hot path, so a shallow
	// copy is enough. We only add keys, never overwrite existing ones.
	p := make(map[string]any, len(in.Message.Payload)+6)
	for k, v := range in.Message.Payload {
		p[k] = v
	}
	p["user_uuid"] = res.UserID // "" = unregistered: no personal scope
	p["identity_id"] = res.IdentityID
	p["registered"] = res.Registered()
	p["trust"] = res.Trust
	p["native_from"] = native
	in.Message.Payload = p
	s.inner.Submit(in)
}

// linkCode extracts a code from an in-channel link command. It reports ok for
// "/link CODE" — and for a bare "/link", which is answered with guidance
// rather than passed to a loop.
func linkCode(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if t == linkCommand {
		return "", true
	}
	rest, ok := strings.CutPrefix(t, linkCommand+" ")
	if !ok {
		return "", false
	}
	code := strings.TrimSpace(rest)
	if code == "" || strings.ContainsAny(code, " \t\r\n") {
		return "", false
	}
	return code, true
}

// completeLink consumes a challenge and reports the outcome in-channel. The
// message is always swallowed: "/link ..." is never chat.
func (s *Sink) completeLink(in router.Inbound, res Resolution, native, code string) {
	if code == "" {
		s.say(in, "To link this account, get a code from the users API (POST /v1/users/me/links), then send: /link YOURCODE")
		return
	}
	userID, err := s.reg.ConsumeLink(in.Channel, native, code)
	if err != nil {
		s.log.Warn("link challenge failed", "err", err, "channel", in.Channel, "identity", res.IdentityID)
		s.say(in, "Linking failed: "+err.Error())
		return
	}
	metrics.Inc("agentflow_user_links")
	s.log.Info("identity linked by challenge", "user_id", userID, "identity", res.IdentityID, "channel", in.Channel)
	s.say(in, "Linked. This account is now connected to your profile.")
}
