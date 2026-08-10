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
	log      *slog.Logger

	mu        sync.Mutex
	sessions  map[string]*session.Actor
	cancels   map[string]context.CancelFunc
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

// Start fixes the context used for lazily spawned actors.
func (s *Supervisor) Start(ctx context.Context) { s.ctx = ctx }

// Agents returns the agent definitions (the reload watcher reads these).
func (s *Supervisor) Agents() map[string]*AgentDef { return s.defs }

// Deliver routes a message to the session for (agent, key), spawning it on
// first contact.
func (s *Supervisor) Deliver(agent, key string, msg session.Message) error {
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
		actorCtx, cancel := context.WithCancel(s.ctx)
		s.sessions[skey] = a
		s.cancels[skey] = cancel
		go a.Run(actorCtx)
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
		s.shellMgr.ReapSession(id.SessionID)
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
