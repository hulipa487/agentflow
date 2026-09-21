// Package supervisor owns the live session map: one actor per
// (agent, route-key), spawned lazily on first message. It also holds the
// per-agent shared state (instructions, model) that sessions read via
// agent.info.
package supervisor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"agentflow/internal/builtins"
	"agentflow/internal/core/gateway"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/request"
	"agentflow/internal/core/safety"
	"agentflow/internal/core/scheduler"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/shell"
)

// AgentDef is a configured agent with its resolved loop source and shared
// per-agent info.
type AgentDef struct {
	Info             *session.Info
	LoopFile         string // hot-reload watched path; "" for builtins
	LoopSrc          string
	InstructionsPath string // watched; content lands in Info.Instructions
	Handlers         map[string]session.OpHandler
	CanContact       map[string]bool
	Capabilities     map[string]bool
	Safety           *safety.Dispatcher
	// Persistent marks a daemon agent: the supervisor delivers a synthetic
	// boot message at startup (BootPersistent) so its session spawns without
	// waiting for external traffic.
	Persistent bool
	// SpawnTemplate marks this def as produced by a spawn profile.
	SpawnTemplate *SpawnTemplate
}

// SpawnTemplate is a resolved, in-memory spawn profile. The supervisor holds
// one per profiles.agent entry; Spawn attenuates it against the parent.
type SpawnTemplate struct {
	Name         string
	LoopFile     string
	LoopSrc      string
	Model        string
	Instructions string
	Shell        map[string]any
	Memory       *session.Info // partial Info used as a template; cloned per spawn
	CanContact   map[string]bool
	Capabilities map[string]bool
	Skills       []string
	Handlers     map[string]session.OpHandler
	Safety       *safety.Dispatcher
}

// Supervisor resolves (agent, key) → session actor.
type Supervisor struct {
	defs     map[string]*AgentDef
	gw       *gateway.Registry
	pool     *pool.Pool
	shellMgr *shell.Manager
	sched    *schedulerAdapter
	users    session.UserResolver
	// profileSettings, when set, supplies each session with the per-user model
	// and instruction overrides its person has. See session.ProfileSettings.
	profileSettings session.ProfileSettings
	log             *slog.Logger

	// EgressJournal, when set, is attached to every spawned actor so all
	// session egress lands in the core-owned message journal.
	EgressJournal session.EgressJournalFunc

	// hub, when set, decides whether a delivered message is this instance's to
	// handle or belongs to the instance that owns the session. Nil — the
	// default, and every single-instance deployment — delivers everything here.
	hub SessionRouter

	mu       sync.Mutex
	sessions map[string]*session.Actor
	cancels  map[string]context.CancelFunc
	// retired tracks session ids that ran and exited. A send to a retired
	// session address must fail (the recipient is gone); find-or-create is only
	// for session addresses that have never existed (e.g. a PM session that has
	// not been started yet), never for resurrecting a dead one.
	retired   map[string]struct{}
	templates map[string]*SpawnTemplate
	ctx       context.Context
	requests  *request.Registry
}

func New(defs map[string]*AgentDef, gw *gateway.Registry, p *pool.Pool, shellMgr *shell.Manager, log *slog.Logger) *Supervisor {
	templates := map[string]*SpawnTemplate{}
	for _, def := range defs {
		if def.SpawnTemplate != nil {
			templates[def.SpawnTemplate.Name] = def.SpawnTemplate
		}
	}
	return &Supervisor{
		defs:      defs,
		gw:        gw,
		pool:      p,
		shellMgr:  shellMgr,
		sessions:  map[string]*session.Actor{},
		cancels:   map[string]context.CancelFunc{},
		retired:   map[string]struct{}{},
		templates: templates,
		requests:  request.New(),
		log:       log.With("module", "supervisor"),
	}
}

// SetScheduler installs the scheduler adapter. Called by main after the
// supervisor and scheduler service are both constructed.
func (s *Supervisor) SetScheduler(svc *scheduler.Service) {
	s.sched = newSchedulerAdapter(svc, s, s.log)
}

// SetUserResolver installs the identity resolver used by session.push_user.
// Called by main; nil (the default) leaves push_user returning "identity not
// enabled" — the runtime works unchanged without the identity layer.
func (s *Supervisor) SetUserResolver(r session.UserResolver) { s.users = r }

// SetProfileSettings installs the per-user override lookup every session gets.
// Called by main; nil (the default) leaves every user on the agent's own model
// and prompts.
func (s *Supervisor) SetProfileSettings(p session.ProfileSettings) { s.profileSettings = p }

// Start fixes the context used for lazily spawned actors.
func (s *Supervisor) Start(ctx context.Context) { s.ctx = ctx }

// BootPersistent delivers a synthetic boot message to every agent definition
// marked persistent, so daemon agents (cron schedulers, queue consumers)
// spawn at boot without waiting for external traffic. Call after Start, once
// channels are registered so a boot-turn reply has somewhere to go. Delivery
// errors are logged, never fatal.
func (s *Supervisor) BootPersistent() {
	for name, def := range s.defs {
		if !def.Persistent || def.SpawnTemplate != nil {
			continue
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
		if err := s.DeliverLocal(name, "boot", msg); err != nil {
			s.log.Warn("persistent agent boot failed", "agent", name, "err", err)
			continue
		}
		s.log.Info("persistent agent booted", "agent", name)
	}
}

// Agents returns the agent definitions (the reload watcher reads these).
func (s *Supervisor) Agents() map[string]*AgentDef { return s.defs }

// SessionRouter decides where a session's messages are delivered. In a
// single-instance deployment there is none and every message is delivered here.
// In a fleet the hub implements it: a session runs on one instance, and a
// message that arrives on another is queued for the owner rather than starting
// a second copy of the conversation.
type SessionRouter interface {
	Route(ctx context.Context, agent, key string, msg session.Message) error
}

// SetHub installs the router that decides whether a delivered message is this
// instance's to handle. Nil — the default — delivers everything locally.
func (s *Supervisor) SetHub(h SessionRouter) { s.hub = h }

// Deliver routes a message to the session for (agent, key), spawning it on
// first contact. With a hub installed the message may instead be queued for
// the instance that owns the session; see SetHub.
func (s *Supervisor) Deliver(agent, key string, msg session.Message) error {
	if h := s.hub; h != nil {
		return h.Route(context.Background(), agent, key, msg)
	}
	return s.DeliverLocal(agent, key, msg)
}

// DeliverLocal delivers to this instance's session unconditionally, bypassing
// the hub. It is what the hub itself calls once it has established that this
// instance owns the session, and what a boot push uses: "spawn my daemons" is a
// local instruction, not a message to be routed.
func (s *Supervisor) DeliverLocal(agent, key string, msg session.Message) error {
	def, ok := s.defs[agent]
	if !ok {
		return &UnknownAgentError{Agent: agent}
	}
	skey := agent + "|" + key

	s.mu.Lock()
	a, ok := s.sessions[skey]
	if !ok {
		identity := session.Identity{
			SessionID:    skey,
			Agent:        agent,
			CanContact:   def.CanContact,
			Capabilities: def.Capabilities,
		}
		a = session.New(skey, identity, def.Info, s.gw, s, s.sched, s.users, def.Safety, def.Handlers, s.pool, s.log)
		a.LoopFile = def.LoopFile
		a.LoopSrc = def.LoopSrc
		a.SupportSrcs = builtins.SupportChunks()
		a.OnExit = s.onActorExit
		a.SetProfileSettings(s.profileSettings)
		a.Journal = s.EgressJournal
		actorCtx, cancel := context.WithCancel(s.ctx)
		s.sessions[skey] = a
		s.cancels[skey] = cancel
		go a.Run(actorCtx)
		metrics.Inc("agentflow_sessions_active")
		s.log.Info("session spawned", "session", skey)
	}
	s.mu.Unlock()

	select {
	case a.Mailbox <- msg:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

// StopSession terminates one actor without stopping the runtime. It is safe to
// call repeatedly and is used by lifecycle policies and child supervision.
func (s *Supervisor) StopSession(sessionID string) {
	s.mu.Lock()
	cancel, ok := s.cancels[sessionID]
	s.mu.Unlock()
	if ok {
		cancel()
	}
}

func (s *Supervisor) onActorExit(id session.Identity, reason session.EndReason) {
	var parent *session.Actor

	s.mu.Lock()
	current, ok := s.sessions[id.SessionID]
	if ok && current.Identity.SessionID == id.SessionID {
		delete(s.sessions, id.SessionID)
		delete(s.cancels, id.SessionID)
		metrics.Add("agentflow_sessions_active", -1)
		metrics.Inc("agentflow_children_died")
		if s.retired != nil {
			s.retired[id.SessionID] = struct{}{}
		}
	}
	if id.ParentID != "" {
		parent = s.sessions[id.ParentID]
	}
	s.mu.Unlock()

	// Reap only resources owned by the exited session; never use the agent name.
	s.requests.CancelOwner(id.SessionID)
	if s.sched != nil {
		s.sched.CancelOwner(id.SessionID)
	}
	if s.shellMgr != nil {
		// The context is detached on purpose: the session is already gone, and
		// the reap must not be cancelled by whatever cancelled it.
		s.shellMgr.ReapSession(context.Background(), id.SessionID)
	}

	if parent != nil {
		msg := session.Message{
			ID:   "system:agent.died:" + id.SessionID,
			Type: "system",
			From: "system:lifecycle",
			To:   "session:" + id.ParentID,
			Payload: map[string]any{
				"event":      "agent.died",
				"session_id": id.SessionID,
				"agent":      id.Agent,
				"reason":     string(reason),
			},
			Ts: time.Now().Unix(),
			Provenance: &session.Provenance{
				Kind:      "system",
				Principal: "system:lifecycle",
				Parent:    id.ParentID,
			},
		}
		select {
		case parent.Mailbox <- msg:
		default:
			s.log.Warn("parent mailbox full; dropping agent.died", "parent", id.ParentID, "child", id.SessionID)
		}
	}

	s.log.Info("session exited", "session", id.SessionID, "agent", id.Agent, "reason", reason)
}

// ReloadAgent restarts every live session of an agent (loop file changed).
func (s *Supervisor) ReloadAgent(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := name + "|"
	for key, a := range s.sessions {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			a.Reload()
		}
	}
}

// UnknownAgentError is returned when the router names an agent that isn't
// configured — a broken route plugin, not a runtime failure.
type UnknownAgentError struct{ Agent string }

func (e *UnknownAgentError) Error() string { return "unknown agent " + e.Agent }
