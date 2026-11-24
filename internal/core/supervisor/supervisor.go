// Package supervisor owns the live session map: one actor per
// (agent, route-key), spawned lazily on first message. It also holds the
// per-agent shared state (instructions, model) that sessions read via
// agent.info.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"agentflow/internal/core/gateway"
	"agentflow/internal/core/media"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/request"
	"agentflow/internal/core/safety"
	"agentflow/internal/core/scheduler"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/shell"
	"agentflow/internal/luacode"
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
	SSH          map[string]any
	Memory       *session.Info // partial Info used as a template; cloned per spawn
	CanContact   map[string]bool
	Capabilities map[string]bool
	Skills       []string
	Handlers     map[string]session.OpHandler
	Safety       *safety.Dispatcher
	// InheritSafety means the profile did not name a safety profile; a child
	// inherits the spawning session's dispatcher instead of silently running
	// with none.
	InheritSafety bool
}

// Supervisor resolves (agent, key) → session actor.
type Supervisor struct {
	defs     map[string]*AgentDef
	gw       *gateway.Registry
	pool     *pool.Pool
	shellMgr *shell.Manager
	// jobs are the session's background shell jobs, when the sandbox is wired.
	jobs  ShellJobs
	sched *schedulerAdapter
	users session.UserResolver
	// profileSettings, when set, supplies each session with the per-user model
	// and instruction overrides its person has. See session.ProfileSettings.
	profileSettings session.ProfileSettings
	log             *slog.Logger

	// EgressJournal, when set, is attached to every spawned actor so all
	// session egress lands in the core-owned message journal.
	EgressJournal session.EgressJournalFunc

	// media is the centralized media service attached to every spawned actor.
	media *media.Service

	// hub, when set, decides whether a delivered message is this instance's to
	// handle or belongs to the instance that owns the session. Nil — the
	// default, and every single-instance deployment — delivers everything here.
	hub SessionRouter

	// membershipResolver validates a router-proposed group identity and
	// returns the derived membership uuid. Nil (the default) refuses every
	// group proposal. See SetMembershipResolver / DeliverAs.
	membershipResolver func(personalUUID, groupUUID string) (string, error)

	// sessionCap is the per-tenant bound on live sessions (E18): 0 disables
	// it. tenants maps a session key to the tenant its first message carried;
	// a session serves the tenant it was spawned for.
	sessionCap     int
	tenants        map[string]string
	pushSameTenant bool

	// ephemeralCap bounds one tenant's concurrent ephemeral sessions. It is
	// separate from sessionCap: an ephemeral session costs a VM and a goroutine
	// for the length of one request, and counting it against the conversation
	// cap would let a burst of stateless traffic lock a tenant out of its own
	// chats. 0 disables the bound. ephemeralTenants counts the live ones;
	// ephemeralTenant remembers each one's tenant so the count can be undone
	// when it exits.
	ephemeralCap     int
	ephemeralTenants map[string]int
	ephemeralTenant  map[string]string

	// journalMode resolves a channel's journal mode for every spawned actor.
	// Nil is full.
	journalMode func(channel string) session.JournalMode

	// idleUnload is how long a conversation may sit idle before the janitor
	// unloads it. 0 disables the janitor. janitorEvery is the sweep period
	// (a minute), lowered by tests.
	idleUnload   time.Duration
	janitorEvery time.Duration
	// vmMemory is the per-state allocation cap new sessions get (0 =
	// uncapped). See SetVMMemory.
	vmMemory int64

	// tracer, when set, receives every session's execution events (the
	// standalone Studio app). tracePayloads additionally captures text.
	tracer        session.Tracer
	tracePayloads bool

	mu       sync.Mutex
	sessions map[string]*session.Actor
	cancels  map[string]context.CancelFunc
	// retired tracks session ids that ran and exited. A send to a retired
	// session address must fail (the recipient is gone); find-or-create is only
	// for session addresses that have never existed (e.g. a PM session that has
	// not been started yet), never for resurrecting a dead one.
	retired map[string]struct{}
	// leaseLost marks sessions this instance stopped because the hub lost their
	// lease to a peer. Such a session must NOT be retired: this instance may win
	// the lease back later, and the same conversation then has to be able to
	// run here again.
	leaseLost map[string]struct{}
	templates map[string]*SpawnTemplate
	// safetyBySession is the dispatcher each live session runs under, so a
	// spawned child can inherit its parent's chain when the profile does not
	// name one.
	safetyBySession map[string]*safety.Dispatcher
	ctx             context.Context
	requests        *request.Registry
}

func New(defs map[string]*AgentDef, gw *gateway.Registry, p *pool.Pool, shellMgr *shell.Manager, log *slog.Logger) *Supervisor {
	templates := map[string]*SpawnTemplate{}
	for _, def := range defs {
		if def.SpawnTemplate != nil {
			templates[def.SpawnTemplate.Name] = def.SpawnTemplate
		}
	}
	return &Supervisor{
		defs:             defs,
		gw:               gw,
		pool:             p,
		shellMgr:         shellMgr,
		sessions:         map[string]*session.Actor{},
		cancels:          map[string]context.CancelFunc{},
		retired:          map[string]struct{}{},
		leaseLost:        map[string]struct{}{},
		templates:        templates,
		safetyBySession:  map[string]*safety.Dispatcher{},
		tenants:          map[string]string{},
		ephemeralTenants: map[string]int{},
		ephemeralTenant:  map[string]string{},
		requests:         request.New(),
		janitorEvery:     time.Minute,
		log:              log.With("module", "supervisor"),
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

// SetMedia installs the centralized media service used by session actors to
// normalize inbound/outbound attachments and enforce per-scope handle grants.
func (s *Supervisor) SetMedia(svc *media.Service) { s.media = svc }

// SetSessionCap sets the per-tenant bound on live sessions (E18): a tenant
// whose sessions are all busy with it cannot spawn actors without bound. 0
// (the default) disables the bound. Called by main from runtime.limits.
func (s *Supervisor) SetSessionCap(n int) { s.sessionCap = n }

// SetEphemeralCap sets the per-tenant bound on concurrent ephemeral sessions.
// 0 disables it. It is a separate budget from SetSessionCap: a stateless
// request's session lives for one turn and must not consume a conversation
// slot. Called by main from runtime.limits.
func (s *Supervisor) SetEphemeralCap(n int) { s.ephemeralCap = n }

// SetJournalMode installs the channel→journal-mode lookup every spawned actor
// uses. Nil (the default) journals every channel in full.
func (s *Supervisor) SetJournalMode(fn func(channel string) session.JournalMode) {
	s.journalMode = fn
}

// SetIdleUnload sets how long a conversation may sit idle before it is
// unloaded. 0 (the default at this layer) disables the janitor; main passes
// runtime.sessions.idle_unload, whose own default is 30m. Call before Start.
func (s *Supervisor) SetIdleUnload(d time.Duration) { s.idleUnload = d }

// SetVMMemory sets the per-Luau-state allocation cap every new session gets
// (runtime.vm.memory_limit, now live). 0 = uncapped. With the session cap it
// bounds each tenant's VM footprint: cap × sessions_per_tenant.
func (s *Supervisor) SetVMMemory(n int64) { s.vmMemory = n }

// SetTracer installs the execution-event observer every session gets. payloads
// additionally captures message and reply text. Nil (the default) disables
// tracing; it is observability only and never changes execution.
func (s *Supervisor) SetTracer(t session.Tracer, payloads bool) {
	s.tracer = t
	s.tracePayloads = payloads
}

// tenantOf is the tenant a message's turn acts for, from the core-stamped
// provenance — the only authority, as everywhere. "" (unprovenanced) is
// engine-internal traffic and is exempt from tenant bounds.
func tenantOf(msg session.Message) string {
	if p := msg.Provenance; p != nil && p.UserUUID != nil {
		return *p.UserUUID
	}
	return ""
}

// Start fixes the context used for lazily spawned actors.
func (s *Supervisor) Start(ctx context.Context) {
	s.ctx = ctx
	if s.idleUnload > 0 {
		go s.idleJanitor(ctx)
	}
}

// idleJanitor unloads conversations that have been idle past the configured
// window. One goroutine per process, once a minute: unloading is housekeeping,
// not a per-message cost.
func (s *Supervisor) idleJanitor(ctx context.Context) {
	tick := time.NewTicker(s.janitorEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.SweepIdle()
		}
	}
}

// SweepIdle unloads every conversation that qualifies right now. It is the
// janitor's whole pass, exported so tests (and an operator tool) can run it
// without waiting for the ticker.
//
// A session is unloaded only when all hold: idle longer than the setting, not
// busy, mailbox empty, not a persistent daemon, not ephemeral, not a spawned
// child, and owning no live resource — a timer, a shell, a live child, or a
// parked agent.request. The last check is what keeps an unload from severing
// work the session is waiting on: those four are the only things that outlive
// a parked actor. A child is excluded because it cannot come back: what makes
// it a child (its parent, the task it was spawned with) is not something the
// next message could recreate.
//
// The sweep only asks. The actor unloads itself, and only if it is still
// parked on an empty mailbox with the activity stamp seen here — so a message
// that arrives while the sweep is deciding is served, not lost.
func (s *Supervisor) SweepIdle() {
	if s.idleUnload <= 0 {
		return
	}
	now := time.Now()
	s.mu.Lock()
	candidates := make([]string, 0, len(s.sessions))
	for key, a := range s.sessions {
		if a.Ephemeral || a.Identity.ParentID != "" || a.Busy() || len(a.Mailbox) > 0 {
			continue
		}
		if def := s.defs[a.Identity.Agent]; def != nil && def.Persistent {
			continue
		}
		if now.Sub(a.LastActivity()) < s.idleUnload {
			continue
		}
		candidates = append(candidates, key)
	}
	s.mu.Unlock()

	for _, key := range candidates {
		if s.ownsLiveResource(key) {
			continue
		}
		s.mu.Lock()
		a := s.sessions[key]
		s.mu.Unlock()
		if a == nil {
			continue
		}
		// Re-read after the resource checks: the session may have taken a
		// message since the snapshot above.
		seen := a.LastActivity()
		if a.Busy() || len(a.Mailbox) > 0 || time.Since(seen) < s.idleUnload {
			continue
		}
		a.RequestUnload(seen)
	}
}

// ShellJobs is what the supervisor needs to know about background shell jobs:
// whether a session is waiting on one, and how to stop them when it ends.
type ShellJobs interface {
	Running(owner string) int
	CancelOwner(owner string)
}

// SetShellJobs installs the background shell job service. main calls it once,
// before any session runs.
func (s *Supervisor) SetShellJobs(j ShellJobs) { s.jobs = j }

// ownsLiveResource reports whether a session holds anything that outlives its
// parked actor. Call without s.mu held.
func (s *Supervisor) ownsLiveResource(skey string) bool {
	if s.sched != nil && s.sched.OwnerPending(skey) > 0 {
		return true
	}
	if s.shellMgr != nil && s.shellMgr.Owned(skey) > 0 {
		return true
	}
	// A session waiting for a background job's result is not idle. Unloading
	// it would end the session's resources, the job among them.
	if s.jobs != nil && s.jobs.Running(skey) > 0 {
		return true
	}
	if s.requests.PendingFor(skey) > 0 {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.sessions {
		if a.Identity.ParentID == skey {
			return true
		}
	}
	return false
}

// LiveLocal reports whether this instance holds a live actor for sessKey. It
// is the liveness function for shell reclaim in a single topology, where there
// is no lease table to ask: a handle whose session is not here belongs to a
// process that is gone.
func (s *Supervisor) LiveLocal(sessKey string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.sessions[sessKey]
	return ok
}

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
		s.bootPersistentOne(ctx, name, def)
	}
}

// daemonKey is the session key a daemon agent's boot turn is delivered to.
const daemonKey = "boot"

// FailoverPersistent boots every persistent agent that no live instance owns.
//
// BootPersistent only runs at startup, so a daemon whose instance is killed
// stays down until some instance restarts — the one piece of fleet state that
// was not self-healing. The loop calls this every half session TTL: `Live` reads
// the lease, and `bootPersistentOne` claims it, so two instances noticing the
// same dead daemon still boot it once.
func (s *Supervisor) FailoverPersistent(ctx context.Context) {
	if s.hub == nil {
		return
	}
	for name, def := range s.defs {
		if !def.Persistent || def.SpawnTemplate != nil {
			continue
		}
		sessKey := name + "|" + daemonKey
		live, err := s.hub.Live(ctx, sessKey)
		if err != nil {
			s.log.Warn("daemon failover check failed; leaving the daemon alone", "agent", name, "err", err)
			continue
		}
		if live {
			continue
		}
		s.log.Info("daemon has no live owner; attempting to boot it here", "agent", name)
		s.bootPersistentOne(ctx, name, def)
	}
}

// BootPersistentAgent boots one daemon by name — the runtime-registry path,
// where an agent is installed after Start and the definition is already in the
// map. Reports whether the boot turn was delivered here.
func (s *Supervisor) BootPersistentAgent(ctx context.Context, name string) bool {
	s.mu.Lock()
	def := s.defs[name]
	s.mu.Unlock()
	if def == nil {
		return false
	}
	return s.bootPersistentOne(ctx, name, def)
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
	// Claim takes ownership of a session for this instance, reporting whether
	// it now owns it, and keeps owning it until it is released. It is asked
	// before work that must happen exactly once per session in the deployment —
	// see BootPersistent — so a router that cannot answer must return an error
	// rather than a hopeful true.
	Claim(ctx context.Context, sessKey string) (bool, error)
	// Live reports whether any instance still owns the session. The daemon
	// failover loop asks it before re-booting a daemon; a store that cannot
	// answer makes the loop skip rather than risk a second copy.
	Live(ctx context.Context, sessKey string) (bool, error)
	// ReleaseSession gives up this instance's claim on one session, so the next
	// owner does not wait out the lease. The idle janitor uses it after
	// unloading a conversation.
	ReleaseSession(ctx context.Context, sessKey string) error
}

// SetHub installs the router that decides whether a delivered message is this
// instance's to handle. Nil — the default — delivers everything locally.
func (s *Supervisor) SetHub(h SessionRouter) { s.hub = h }

// SetPushSameTenant makes session.push_user refuse targets outside the current
// tenant. Call before Start; the default keeps operator-service notifications
// working as before.
func (s *Supervisor) SetPushSameTenant(v bool) { s.pushSameTenant = v }

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

	a, err := s.ensureSession(agent, skey, def, tenantOf(msg), false)
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

// ErrTenantBusy is what DeliverEphemeral returns when a tenant is at its
// concurrent ephemeral-session bound. The channel maps it to HTTP 429, so the
// error is a refusal rather than a runtime failure.
var ErrTenantBusy = errors.New("ephemeral session cap reached")

// ErrSessionCap is the keyed-session cap's refusal. The channel maps it to 429
// like ErrTenantBusy; both are "retry shortly", not "something broke".
var ErrSessionCap = errors.New("live-session cap reached")

// DeliverEphemeral delivers one message to a one-turn session for (agent, key).
//
// It never calls the hub: an ephemeral key is unique per request, so two
// instances cannot both own it, and a lease row would exist only to say so.
// The actor serves exactly one message, asks for the inbox again, and exits;
// nothing of it survives the request.
//
// A key that already has a session attaches to it rather than starting a second
// one — the Idempotency-Key retry path relies on that: a repeat while the first
// request is still running joins it.
func (s *Supervisor) DeliverEphemeral(agent, key string, msg session.Message) error {
	def, ok := s.defs[agent]
	if !ok {
		return &UnknownAgentError{Agent: agent}
	}
	skey := agent + "|" + key
	a, err := s.ensureSession(agent, skey, def, tenantOf(msg), true)
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
//
// tenant is the first message's tenant, for the per-tenant session cap (E18):
// the one place actors are created is where the bound applies.
func (s *Supervisor) ensureSession(agent, skey string, def *AgentDef, tenant string, ephemeral bool) (*session.Actor, error) {
	s.mu.Lock()
	if a, ok := s.sessions[skey]; ok {
		// An ephemeral key is unique per request, so a hit is an idempotent
		// retry joining the running request; it does not re-bind or re-count.
		if ephemeral {
			s.mu.Unlock()
			return a, nil
		}
		err := s.bindTenantLocked(skey, tenant)
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return a, nil
	}
	if ephemeral {
		if tenant != "" && s.ephemeralCap > 0 && s.ephemeralTenants[tenant] >= s.ephemeralCap {
			s.mu.Unlock()
			metrics.Inc("agentflow_ephemeral_cap_drops")
			return nil, fmt.Errorf("%w: tenant %s has %d ephemeral sessions in flight (limit %d); retry shortly or raise runtime.limits.ephemeral_per_tenant",
				ErrTenantBusy, tenant, s.ephemeralTenants[tenant], s.ephemeralCap)
		}
	} else if tenant != "" && s.sessionCap > 0 && s.countTenantLocked(tenant) >= s.sessionCap {
		n := s.countTenantLocked(tenant)
		s.mu.Unlock()
		metrics.Inc("agentflow_session_cap_drops")
		return nil, fmt.Errorf("%w: tenant %s has %d live sessions (limit %d); wait for one to end or raise runtime.limits.sessions_per_tenant",
			ErrSessionCap, tenant, n, s.sessionCap)
	}
	s.mu.Unlock()

	support, err := luacode.SupportChunks()
	if err != nil {
		return nil, err
	}

	identity := session.Identity{
		SessionID:      skey,
		Agent:          agent,
		CanContact:     def.CanContact,
		Capabilities:   def.Capabilities,
		PushSameTenant: s.pushSameTenant,
		Ephemeral:      ephemeral,
	}
	a := session.New(skey, identity, def.Info, s.gw, s, s.sched, s.users, def.Safety, def.Handlers, s.pool, s.log)
	a.LoopFile = def.LoopFile
	a.LoopSrc = def.LoopSrc
	a.SupportSrcs = support
	a.OnExit = s.onActorExit
	a.SetProfileSettings(s.profileSettings)
	a.Journal = s.EgressJournal
	a.JournalModeOf = s.journalMode
	a.Ephemeral = ephemeral
	a.MemBudget = s.vmMemory
	a.Tracer = s.tracer
	a.TracePayloads = s.tracePayloads
	a.Media = s.media
	actorCtx, cancel := context.WithCancel(s.ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.sessions[skey]; ok {
		// Lost the race: the actor built above was never started, so its
		// context is the only thing to release.
		cancel()
		if ephemeral {
			return existing, nil
		}
		if err := s.bindTenantLocked(skey, tenant); err != nil {
			return nil, err
		}
		return existing, nil
	}
	s.sessions[skey] = a
	s.cancels[skey] = cancel
	s.safetyBySession[skey] = def.Safety
	if ephemeral {
		if tenant != "" {
			s.ephemeralTenant[skey] = tenant
			s.ephemeralTenants[tenant]++
		}
		metrics.Inc("agentflow_ephemeral_sessions")
	} else {
		if tenant != "" {
			s.tenants[skey] = tenant
		}
		metrics.Inc("agentflow_sessions_active")
	}
	go a.Run(actorCtx)
	s.log.Info("session spawned", "session", skey, "ephemeral", ephemeral)
	return a, nil
}

// bindTenantLocked enforces the session-to-tenant binding. A session serves
// the tenant it was spawned for; a message stamped with another tenant must
// never be appended to it, because session state, scratch and shell handles are
// keyed by the session, not re-resolved per message. An incoming tenantless
// message (engine work, boot pushes) is allowed and does not adopt the session.
// Caller holds s.mu.
func (s *Supervisor) bindTenantLocked(skey, tenant string) error {
	if tenant == "" {
		return nil
	}
	stored, ok := s.tenants[skey]
	if !ok {
		s.tenants[skey] = tenant
		return nil
	}
	if stored != "" && stored != tenant {
		metrics.Inc("agentflow_session_tenant_mismatch")
		// Do not name the other tenant in the error: the caller already knows
		// its own identity and does not need the other side's uuid.
		return fmt.Errorf("session %q belongs to another tenant", skey)
	}
	if stored == "" {
		s.tenants[skey] = tenant
	}
	return nil
}

// countTenantLocked counts the live sessions attributed to one tenant.
// Linear scan: the cap bounds the count, so the map never holds more rows
// than sessions-per-tenant times tenants, and the scan runs once per spawn,
// never per message. Caller holds mu.
func (s *Supervisor) countTenantLocked(tenant string) int {
	n := 0
	for _, t := range s.tenants {
		if t == tenant {
			n++
		}
	}
	return n
}

// SessionBusy reports whether a live session is processing a message. The
// second value is false when the session does not exist (never started, or
// exited) — an observer uses it to finish a turn whose loop ended instead of
// parking on the inbox.
func (s *Supervisor) SessionBusy(sessionID string) (busy, live bool) {
	s.mu.Lock()
	a, ok := s.sessions[sessionID]
	s.mu.Unlock()
	if !ok {
		return false, false
	}
	return a.Busy(), true
}

// Wait blocks until every session actor has exited, or ctx ends, and reports
// whether they all did. Shutdown calls it between cancelling the actors and
// releasing their leases: a peer may claim a session the moment its lease is
// given up, and it must not run a turn this instance is still finishing.
func (s *Supervisor) Wait(ctx context.Context) bool {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		s.mu.Lock()
		n := len(s.sessions)
		s.mu.Unlock()
		if n == 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
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

// StopSessionForLease stops the local actor for a session whose lease moved to
// another instance. Unlike an ordinary stop, the session is not retired: this
// instance may take the lease back later, and the conversation must then be
// able to run here again. If the actor is already gone the call does nothing.
func (s *Supervisor) StopSessionForLease(sessionID string) {
	s.mu.Lock()
	cancel, ok := s.cancels[sessionID]
	if ok {
		s.leaseLost[sessionID] = struct{}{}
	}
	s.mu.Unlock()
	if ok {
		cancel()
	}
}

func (s *Supervisor) onActorExit(id session.Identity, reason session.EndReason) {
	var parent *session.Actor
	var unloaded *session.Actor

	// An unloaded conversation gives its lease back, so another instance can
	// serve the next message instead of waiting out the TTL. The release comes
	// BEFORE the session leaves the map: while the dead actor is still
	// registered, a message that arrives is parked in its mailbox and delivered
	// again below. Releasing after the removal would leave a window in which a
	// message spawns a fresh actor under the old lease, and the release then
	// takes the lease away from a session that is running — two instances
	// could end up serving one conversation. The release gets its own context:
	// the actor's may already be cancelled.
	if reason == session.EndUnloaded && s.hub != nil {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := s.hub.ReleaseSession(releaseCtx, id.SessionID); err != nil {
			s.log.Warn("release of an unloaded session's lease failed", "session", id.SessionID, "err", err)
		}
		cancel()
	}

	s.mu.Lock()
	current, ok := s.sessions[id.SessionID]
	if ok && current.Identity.SessionID == id.SessionID {
		if reason == session.EndUnloaded {
			unloaded = current
		}
		delete(s.sessions, id.SessionID)
		delete(s.cancels, id.SessionID)
		delete(s.tenants, id.SessionID)
		delete(s.safetyBySession, id.SessionID)
		if id.Ephemeral {
			// An ephemeral id is unique per request and never comes back, so
			// retiring it would grow the map by one entry per request forever.
			// It cannot have lost a lease (it never took one), and it is not a
			// child whose death a parent should hear about.
			if t, had := s.ephemeralTenant[id.SessionID]; had {
				if s.ephemeralTenants[t] <= 1 {
					delete(s.ephemeralTenants, t)
				} else {
					s.ephemeralTenants[t]--
				}
				delete(s.ephemeralTenant, id.SessionID)
			}
			metrics.Add("agentflow_ephemeral_sessions", -1)
		} else {
			metrics.Add("agentflow_sessions_active", -1)
			if reason != session.EndUnloaded {
				metrics.Inc("agentflow_children_died")
			}
			if _, lost := s.leaseLost[id.SessionID]; lost {
				// The lease moved to a peer. Retiring the id here would make every
				// later send to this conversation fail if this instance wins it
				// back, so the loss marker is consumed without retiring.
				delete(s.leaseLost, id.SessionID)
			} else if reason != session.EndUnloaded && s.retired != nil {
				// An unloaded conversation is not dead: the next message
				// recreates it, so its id must stay resolvable.
				s.retired[id.SessionID] = struct{}{}
			}
		}
	}
	if id.ParentID != "" && reason != session.EndUnloaded {
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
	if s.jobs != nil {
		// A job whose session is gone has no inbox to report to.
		s.jobs.CancelOwner(id.SessionID)
	}
	if unloaded != nil {
		metrics.Inc("agentflow_sessions_unloaded")
		// Messages that reached the mailbox after the actor decided to unload
		// were never served. Deliver them again: the session is gone from the
		// map now, so this recreates it (or queues for whichever instance owns
		// the conversation by then). A sender that picked up the old actor just
		// before the removal and writes after this drain is the one case left
		// uncovered; every session exit has that window.
		if left := unloaded.DrainMailbox(); len(left) > 0 {
			agent, key := splitSessionKey(id.SessionID)
			go func() {
				for _, m := range left {
					if err := s.Deliver(agent, key, m); err != nil {
						s.log.Warn("redelivery after unload failed", "session", id.SessionID, "id", m.ID, "err", err)
					}
				}
			}()
		}
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

// splitSessionKey undoes agent + "|" + key. Agent names cannot contain "|", so
// the first one is the separator.
func splitSessionKey(skey string) (agent, key string) {
	for i := 0; i < len(skey); i++ {
		if skey[i] == '|' {
			return skey[:i], skey[i+1:]
		}
	}
	return skey, ""
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
