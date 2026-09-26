// The runtime agent registry: UpsertAgent and RemoveAgent, the agent-side
// mirror of the model manager's Upsert/Remove. Agents were boot-time config —
// onboarding a tenant was a restart. The management API (PUT/DELETE
// /admin/api/agents/{name}) calls these, so a fleet definition is a config
// write (§6.2).
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"agentflow/internal/core/session"
)

// spawnNamePrefixes are the namespaces spawn profiles occupy in the defs map.
// The registry refuses names in them: profiles are config-owned templates,
// not agents, and an upsert must not shadow one.
var spawnNamePrefixes = []string{"__spawn__", "spawn:"}

// LiveSessionsError reports a removal refused because sessions are live (E20:
// refuse, with an explicit force).
type LiveSessionsError struct {
	Agent    string
	Sessions int
}

func (e *LiveSessionsError) Error() string {
	return fmt.Sprintf("agent %q still has %d live session(s); stop them first or pass force", e.Agent, e.Sessions)
}

// UpsertAgent installs def under name. The map is swapped copy-on-write, not
// mutated: Agents() hands the map to readers (the reload watcher polls it
// live) on the understanding that it is immutable between changes.
//
// An agent that already exists keeps its live sessions: the def is pushed
// into each of them and each restarts at its next safe point, so the turn in
// flight finishes on the def it resolved with and the next turn runs the new
// one (E19). A new daemon agent boots immediately — the same claim-and-boot
// BootPersistent runs, so it does not wait for the next fleet restart.
func (s *Supervisor) UpsertAgent(name string, def *AgentDef) error {
	if def == nil || def.Info == nil || def.Handlers == nil {
		return errors.New("supervisor: agent definition is incomplete")
	}
	if def.Info.Name != name {
		return fmt.Errorf("supervisor: definition names agent %q, not %q", def.Info.Name, name)
	}
	if def.SpawnTemplate != nil {
		return errors.New("supervisor: a spawn template is not an agent definition")
	}
	for _, prefix := range spawnNamePrefixes {
		if strings.HasPrefix(name, prefix) {
			return fmt.Errorf("supervisor: agent name %q is in the spawn-profile namespace", name)
		}
	}

	s.mu.Lock()
	_, replacing := s.defs[name]
	next := make(map[string]*AgentDef, len(s.defs)+1)
	for k, v := range s.defs {
		next[k] = v
	}
	next[name] = def
	s.defs = next
	s.mu.Unlock()

	if replacing {
		s.reloadAgentFrom(name, def)
		s.log.Info("agent replaced; live sessions reload at their next safe point", "agent", name)
	} else if def.Persistent {
		// s.ctx is the runtime context Start fixed; a boot before Start would
		// deliver on a nil ctx, so upsert of a daemon is refused until then.
		if s.ctx == nil {
			return errors.New("supervisor: not started; cannot boot a persistent agent")
		}
		s.bootPersistentOne(s.ctx, name, def)
	}
	return nil
}

// SetMembershipResolver installs the group-identity resolution the router's
// group proposals go through: given a personal uuid and a group uuid it
// reports whether the person is an active member and, if so, the derived
// membership uuid. Wired from main with the tenancy registry; nil (the
// default) refuses every group proposal — a runtime without tenancy has no
// group context.
func (s *Supervisor) SetMembershipResolver(fn func(personalUUID, groupUUID string) (string, error)) {
	s.membershipResolver = fn
}

// DeliverAs delivers a message whose turn acts in a group: the router Lua
// PROPOSES the group (from the chat's durable mode, B10), and the engine
// VALIDATES it — the proposal is resolved through the membership resolver and
// stamped onto the message's core-owned provenance, never taken from Lua
// state. That is what core-stamped means here: validated here, not merely
// trusted (A1, §5.7). The session key carries the raw group uuid, so a chat
// that toggles moves to a different session rather than mixing scopes.
func (s *Supervisor) DeliverAs(agent, key string, msg session.Message, groupUUID string) error {
	if groupUUID == "" {
		return s.DeliverLocal(agent, key, msg)
	}
	if s.membershipResolver == nil {
		return fmt.Errorf("group context is not available on this runtime")
	}
	p := msg.Provenance
	if p == nil || p.UserUUID == nil || *p.UserUUID == "" {
		return fmt.Errorf("a group-proposed message carries no tenant stamp to validate against")
	}
	membership, err := s.membershipResolver(*p.UserUUID, groupUUID)
	if err != nil {
		// A proposal naming a group the sender does not belong to is refused
		// at the boundary — the router bug or forgery fails here, before any
		// scope resolves.
		return fmt.Errorf("group identity refused: %w", err)
	}
	stamped := *p
	stamped.MembershipUUID = &membership
	msg.Provenance = &stamped
	return s.DeliverLocal(agent, key, msg)
}

// RemoveAgent removes an agent at runtime. Live sessions hold the agent's
// handlers, memory handles and a mailbox mid-turn, so removal refuses while
// any exist unless force is set (E20). Returns how many sessions were
// stopped. OnExit reaps each stopped session's resources — shell records,
// scheduler timers, requests — exactly as a lifecycle stop does.
//
// A daemon's session lease is not released here; it expires at the session
// TTL like any abandoned claim, and until then a routed message for the
// removed agent fails with UnknownAgentError at delivery.
func (s *Supervisor) RemoveAgent(name string, force bool) (stopped int, err error) {
	for _, prefix := range spawnNamePrefixes {
		if strings.HasPrefix(name, prefix) {
			return 0, fmt.Errorf("supervisor: agent name %q is in the spawn-profile namespace", name)
		}
	}

	prefix := name + "|"
	s.mu.Lock()
	if _, ok := s.defs[name]; !ok {
		s.mu.Unlock()
		return 0, &UnknownAgentError{Agent: name}
	}
	var live []string
	for key := range s.sessions {
		if strings.HasPrefix(key, prefix) {
			live = append(live, key)
		}
	}
	if len(live) > 0 && !force {
		n := len(live)
		s.mu.Unlock()
		return 0, &LiveSessionsError{Agent: name, Sessions: n}
	}
	next := make(map[string]*AgentDef, len(s.defs))
	for k, v := range s.defs {
		if k != name {
			next[k] = v
		}
	}
	s.defs = next
	for _, key := range live {
		if cancel, ok := s.cancels[key]; ok {
			cancel()
			stopped++
		}
	}
	s.mu.Unlock()

	if stopped > 0 {
		s.log.Info("agent removed; sessions stopping", "agent", name, "sessions", stopped)
	}
	return stopped, nil
}

// reloadAgentFrom pushes def into every live session of name and signals a
// restart. It is the def-carrying counterpart of ReloadAgent, which only
// covers loop files: a live session holds copies of the def-derived state it
// was spawned with, so a replaced def reaches existing sessions by being
// applied, not by being re-read.
func (s *Supervisor) reloadAgentFrom(name string, def *AgentDef) {
	prefix := name + "|"
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, a := range s.sessions {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			a.Define(session.DefUpdate{
				Info:         def.Info,
				Handlers:     def.Handlers,
				Safety:       def.Safety,
				CanContact:   def.CanContact,
				Capabilities: def.Capabilities,
				LoopSrc:      def.LoopSrc,
				LoopFile:     def.LoopFile,
			})
		}
	}
}

// bootPersistentOne boots a single daemon agent — the body of
// BootPersistent's loop, so a daemon added at runtime boots by the same
// claim-and-deliver path as one present at startup. Reports whether the boot
// turn was delivered here.
func (s *Supervisor) bootPersistentOne(ctx context.Context, name string, def *AgentDef) bool {
	if !def.Persistent || def.SpawnTemplate != nil {
		return false
	}
	if s.hub != nil {
		// The session key format is the one DeliverLocal builds below, and
		// the one the hub keys its leases by: sessionhub.SessionKey.
		claimed, err := s.hub.Claim(ctx, name+"|"+daemonKey)
		if err != nil {
			// A store that cannot answer is a store that cannot arbitrate.
			// Starting the daemon anyway would risk a second copy of the
			// conversation, so it is left to whoever can claim it.
			s.log.Warn("persistent agent not booted: session claim failed", "agent", name, "err", err)
			return false
		}
		if !claimed {
			s.log.Info("persistent agent runs on another instance", "agent", name)
			return false
		}
	}
	msg := session.Message{
		ID:   "boot:" + name,
		Type: "boot",
		From: "system:supervisor",
		Ts:   time.Now().Unix(),
		Provenance: &session.Provenance{
			Kind:      "system",
			Principal: "system:supervisor",
		},
	}
	if err := s.DeliverLocal(name, daemonKey, msg); err != nil {
		s.log.Warn("persistent agent boot failed", "agent", name, "err", err)
		return false
	}
	s.log.Info("persistent agent booted", "agent", name)
	return true
}
