package supervisor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"agentflow/internal/core/address"
	"agentflow/internal/core/session"
)

// Send delivers a core-stamped asynchronous agent message after resolving and
// authorizing the requested address. Source identity comes from the actor, not
// from any Lua-controlled payload.
func (s *Supervisor) Send(ctx context.Context, source session.Identity, raw string, payload map[string]any) error {
	if !source.Capabilities["agent.send"] {
		return fmt.Errorf("agent %q lacks agent.send capability", source.Agent)
	}
	dst, err := address.Parse(raw)
	if err != nil {
		return err
	}
	if dst.New {
		return fmt.Errorf("agent.send does not accept spawn-on-address")
	}
	return s.deliverAgentMessage(ctx, source, dst, payload, "")
}

// Request sends an agent message with a runtime-generated correlation ID and
// parks until agent.reply resolves that exact ID or the deadline expires.
func (s *Supervisor) Request(ctx context.Context, source session.Identity, raw string, payload map[string]any, timeout time.Duration) (session.Message, error) {
	if !source.Capabilities["agent.request"] {
		return session.Message{}, fmt.Errorf("agent %q lacks agent.request capability", source.Agent)
	}
	if timeout <= 0 {
		return session.Message{}, fmt.Errorf("agent request timeout must be positive")
	}
	dst, err := address.Parse(raw)
	if err != nil {
		return session.Message{}, err
	}
	if dst.New {
		return session.Message{}, fmt.Errorf("agent.request does not accept spawn-on-address")
	}

	recipient, err := s.recipientAgent(dst)
	if err != nil {
		return session.Message{}, err
	}
	requestID, wait, cancel := s.requests.Open(source.SessionID, recipient)
	if err := s.deliverAgentMessage(ctx, source, dst, payload, requestID); err != nil {
		cancel()
		return session.Message{}, err
	}
	waitCtx, stop := context.WithTimeout(ctx, timeout)
	defer stop()
	return s.requests.Wait(waitCtx, requestID, wait)
}

// Reply resolves an outstanding request owned by a trusted receiver. The
// reply's sender and provenance are stamped here; the request ID itself is an
// opaque runtime token, never a Lua-provided destination identity.
func (s *Supervisor) Reply(ctx context.Context, source session.Identity, requestID string, payload map[string]any) error {
	if requestID == "" {
		return fmt.Errorf("missing request id")
	}
	msg := session.Message{
		ID:      requestID,
		Type:    "agent",
		From:    "agent:" + source.Agent,
		Payload: clonePayload(payload),
		Ts:      time.Now().Unix(),
		Provenance: &session.Provenance{
			Kind:      "agent",
			Principal: "agent:" + source.Agent,
			Parent:    source.ParentID,
			RequestID: requestID,
		},
	}
	if !s.requests.Resolve(requestID, source.Agent, msg) {
		return fmt.Errorf("unknown, expired, or unauthorized request %q", requestID)
	}
	return nil
}

// List returns only address metadata visible to the caller. It intentionally
// omits message content, internal state, grants, and channel correlation data.
func (s *Supervisor) List(source session.Identity) []session.AgentSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]session.AgentSummary, 0, len(s.sessions))
	for id, actor := range s.sessions {
		if id == source.SessionID || source.CanContact[actor.Identity.Agent] || actor.Identity.ParentID == source.SessionID {
			out = append(out, session.AgentSummary{
				SessionID: id,
				Agent:     actor.Identity.Agent,
				ParentID:  actor.Identity.ParentID,
			})
		}
	}
	return out
}

// SessionStatus is one live session's row in the TUI snapshot.
type SessionStatus struct {
	SessionID string
	Agent     string
	ParentID  string
	Busy      bool
}

// Snapshot returns a point-in-time view of every live session for the TUI:
// per-session status plus active/idle/total counts. Unlike List, it is not
// scoped to a caller — it powers the operator-facing dashboard.
func (s *Supervisor) Snapshot() (rows []SessionStatus, active, idle int) {
	s.mu.Lock()
	actors := make([]*session.Actor, 0, len(s.sessions))
	for _, a := range s.sessions {
		actors = append(actors, a)
	}
	s.mu.Unlock()

	// Read Busy outside the lock: it is an atomic per-actor flag, and the
	// actor set was captured under the lock.
	for _, a := range actors {
		busy := a.Busy()
		rows = append(rows, SessionStatus{
			SessionID: a.Identity.SessionID,
			Agent:     a.Identity.Agent,
			ParentID:  a.Identity.ParentID,
			Busy:      busy,
		})
		if busy {
			active++
		} else {
			idle++
		}
	}
	return rows, active, idle
}

func (s *Supervisor) recipientAgent(dst address.Address) (string, error) {
	switch dst.Kind {
	case address.Agent:
		return dst.Agent, nil
	case address.Session:
		s.mu.Lock()
		target, ok := s.sessions[dst.Session]
		s.mu.Unlock()
		if !ok {
			return "", fmt.Errorf("unknown session %q", dst.Session)
		}
		return target.Identity.Agent, nil
	default:
		return "", fmt.Errorf("unsupported address kind %q", dst.Kind)
	}
}

func (s *Supervisor) deliverAgentMessage(ctx context.Context, source session.Identity, dst address.Address, payload map[string]any, requestID string) error {
	var targetAgent, targetKey string
	switch dst.Kind {
	case address.Agent:
		targetAgent = dst.Agent
		if !source.CanContact[targetAgent] && targetAgent != source.Agent {
			return fmt.Errorf("agent %q may not contact %q", source.Agent, targetAgent)
		}
		// A stable per-sender route key keeps a normal agent address on one
		// recipient session while preventing one caller from impersonating another.
		targetKey = "agent:" + source.SessionID
	case address.Session:
		s.mu.Lock()
		target, ok := s.sessions[dst.Session]
		s.mu.Unlock()
		if !ok {
			// Unknown session: find-or-create it as a routed session of a static
			// agent, keyed by the id's key segment (the same semantics the router
			// path uses via Deliver). This is gated by the sender's can_contact
			// exactly like contacting a live session, so a sender can only create
			// sessions of agents it is already authorized to reach. Without this,
			// chat->PM kickoff to a not-yet-running PM session would fail.
			agentName, key, found := strings.Cut(dst.Session, "|")
			if !found || key == "" {
				return fmt.Errorf("unknown session %q", dst.Session)
			}
			if _, defined := s.defs[agentName]; !defined {
				return fmt.Errorf("unknown session %q", dst.Session)
			}
			if !source.CanContact[agentName] && agentName != source.Agent {
				return fmt.Errorf("agent %q may not contact %q", source.Agent, agentName)
			}
			msg := newAgentMessage(source, agentName, payload, requestID)
			return s.Deliver(agentName, key, msg)
		}
		if dst.Session != source.SessionID && !source.CanContact[target.Identity.Agent] && target.Identity.ParentID != source.SessionID {
			return fmt.Errorf("agent %q may not contact session %q", source.Agent, dst.Session)
		}
		targetAgent = target.Identity.Agent
		targetKey = strings.TrimPrefix(dst.Session, targetAgent+"|")
	default:
		return fmt.Errorf("unsupported address kind %q", dst.Kind)
	}

	msg := newAgentMessage(source, targetAgent, payload, requestID)
	return s.Deliver(targetAgent, targetKey, msg)
}

// newAgentMessage builds the core-stamped agent message delivered to a target
// agent. Source identity comes from the actor, never from Lua payloads.
func newAgentMessage(source session.Identity, targetAgent string, payload map[string]any, requestID string) session.Message {
	return session.Message{
		ID:      newMessageID(),
		Type:    "agent",
		From:    "agent:" + source.Agent,
		To:      "agent:" + targetAgent,
		Payload: clonePayload(payload),
		ReplyTo: requestID,
		Ts:      time.Now().Unix(),
		Provenance: &session.Provenance{
			Kind:      "agent",
			Principal: "agent:" + source.Agent,
			Parent:    source.ParentID,
			RequestID: requestID,
		},
	}
}

func newMessageID() string { return "ag-" + uuid.NewString() }

func clonePayload(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
