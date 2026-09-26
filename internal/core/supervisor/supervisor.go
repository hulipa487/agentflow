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
//
// In a fleet each daemon is booted by one instance. A daemon is one session,
// and one conversation must not exist twice: the instance that claims the
// session owns it — it renews the claim like any other session, receives its
// traffic, and hands it over when it dies. The others skip it, which is why a
// daemon's boot turn runs once per fleet boot rather than once per instance; a
// takeover resumes the session without re-running it, because the boot turn is
// a startup instruction and the conversation is in the memory backend.
func (s *Supervisor) BootPersistent(ctx context.Context) {
	for name, def := range s.defs {
		if !def.Persistent || def.SpawnTemplate != nil {
			continue
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
				continue
			}
			if !claimed {
				s.log.Info("persistent agent runs on another instance", "agent", name)
				continue
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
			continue
		}
		s.log.Info("persistent agent booted", "agent", name)
	}
}

// daemonKey is the session key a daemon agent's boot turn is delivered to.
const daemonKey = "boot"

// Agents returns the agent definitions (the reload watcher reads these).
func (s *Supervisor) Agents() map[string]*AgentDef { return s.defs }

// SessionRouter decides where a session's messages are delivered. In a
// single-instance deployment there is none and every message is delivered here.
// In a fleet the hub implements it: a session runs on one instance, and a
// message that arrives on another is queued for the owner rather than starting
// a second copy of the conversation.
type SessionRouter interface {
	Route(ctx context.Context, agent, key string, msg session.Message) error
	// Claim takes ownership of a session for this instance, reporting whether
	// it now owns it, and keeps owning it until it is released. It is asked
	// before work that must happen exactly once per session in the deployment —
	// see BootPersistent — so a router that cannot answer must return an error
	// rather than a hopeful true.
	Claim(ctx context.Context, sessKey string) (bool, error)
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

	a, err := s.ensureSession(agent, skey, def)
	if err != nil {
		return err
	}

	select {
	case a.Mailbox <- msg:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

// ensureSession returns the live session for skey, starting one from def if
// there is none yet.
//
// The support chunks are read and version-gated here, before the session
// exists, and a refusal refuses the session: it has no earlier version of the
// chunks to keep, because it has never run, so the only other thing a refusal
// could mean is a session whose loop is missing the support chunks it was
// written against — the silent downgrade the gate exists to prevent. The error
// goes back to whoever delivered: at boot that is the boot push, which logs it
// (BootPersistent) while the sessions that already exist keep serving, and at
// first contact it is the channel's delivery, which fails rather than running
// half a runtime.
//
// The read happens outside the lock and the map is re-checked under it, so two
// first contacts racing for the same session start one actor and both deliver
// to it.
func (s *Supervisor) ensureSession(agent, skey string, def *AgentDef) (*session.Actor, error) {
	s.mu.Lock()
	if a, ok := s.sessions[skey]; ok {
		s.mu.Unlock()
		return a, nil
	}
	s.mu.Unlock()

	support, err := builtins.SupportChunks()
	if err != nil {
		return nil, err
	}

	identity := session.Identity{
		SessionID:    skey,
		Agent:        agent,
		CanContact:   def.CanContact,
		Capabilities: def.Capabilities,
	}
	a := session.New(skey, identity, def.Info, s.gw, s, s.sched, s.users, def.Safety, def.Handlers, s.pool, s.log)
	a.LoopFile = def.LoopFile
	a.LoopSrc = def.LoopSrc
	a.SupportSrcs = support
	a.OnExit = s.onActorExit
	a.SetProfileSettings(s.profileSettings)
	a.Journal = s.EgressJournal
	actorCtx, cancel := context.WithCancel(s.ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.sessions[skey]; ok {
		// Lost the race: the actor built above was never started, so its
		// context is the only thing to release.
		cancel()
		return existing, nil
	}
	s.sessions[skey] = a
	s.cancels[skey] = cancel
	go a.Run(actorCtx)
	metrics.Inc("agentflow_sessions_active")
	s.log.Info("session spawned", "session", skey)
	return a, nil
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

// ReloadAll restarts every live session of every agent. This is what a
// deployment-wide change uses: the support chunks are loaded into every
// session's Lua state, so there is no per-agent reference to key a reload by.
// Like ReloadAgent it signals rather than waits — each session rebuilds at its
// next safe point, so one that is mid-turn finishes it first.
func (s *Supervisor) ReloadAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.sessions {
		a.Reload()
	}
}

// UnknownAgentError is returned when the router names an agent that isn't
// configured — a broken route plugin, not a runtime failure.
type UnknownAgentError struct{ Agent string }

func (e *UnknownAgentError) Error() string { return "unknown agent " + e.Agent }
