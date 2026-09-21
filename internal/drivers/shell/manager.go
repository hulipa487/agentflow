// Package shell provides the Docker and SSH shell execution providers.
// Shell handles created by a session are owned by that session and reaped
// when the session dies.
package shell

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// HandleState tracks the lifecycle of a shell handle.
type HandleState int

const (
	// HandleState values are pinned: Handle carries State as a numeric JSON
	// field, so the numbers are a wire contract even for values Go no longer
	// names. 0 (pending) and 2 (stopped) are reserved.
	HandleRunning   HandleState = 1 // container/connection is live
	HandleDestroyed HandleState = 3 // reclaimed
)

// SpawnOpts are the parameters for creating a new shell handle.
type SpawnOpts struct {
	Image    string            // Docker image name (ignored for SSH)
	WorkDir  string            // working directory
	Env      map[string]string // environment variables
	Network  string            // "none" | "bridge" | "host"
	MemLimit string            // e.g. "512m"
	CPULimit float64           // e.g. 1.0

	// Volumes are docker bind specs ("host_dir:/container_dir[:ro]"), one per
	// entry, passed to `docker run -v`. They are how checked-out project files
	// and per-session scratch reach a container's filesystem.
	Volumes []string

	// SSH provider fields.
	Host     string // host:port
	User     string
	Password string
	KeyFile  string

	// ShellOpts is the generic escape hatch for provider-specific options that
	// don't map to a typed field above (e.g. a per-spawn docker image override).
	// Each provider reads the keys it knows.
	ShellOpts map[string]any
}

// ExecResult is the output of a command executed inside a shell handle.
type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Duration int64  `json:"duration_ms"`
}

// Handle is a managed shell handle (Docker container, SSH session).
type Handle struct {
	ID       string         `json:"id"`
	Provider string         `json:"provider"` // "docker" | "ssh"
	Image    string         `json:"image"`    // empty for SSH
	State    HandleState    `json:"state"`
	Meta     map[string]any `json:"meta,omitempty"`

	owner    string // the session key this handle belongs to
	internal any    // provider-specific state (e.g. container ID, *ssh.Client)
	mu       sync.RWMutex
}

// ShellProvider is the interface for a shell backend (Docker, SSH).
type ShellProvider interface {
	Name() string
	Spawn(ctx context.Context, opts SpawnOpts) (*Handle, error)
	Exec(ctx context.Context, handle *Handle, cmd string) (*ExecResult, error)
	Read(ctx context.Context, handle *Handle, path string) ([]byte, error)
	Write(ctx context.Context, handle *Handle, path string, content []byte) error
	Destroy(ctx context.Context, handle *Handle) error
	Alive(handle *Handle) bool
}

// Manager owns all shell handles across the runtime and provides per-session
// reaping when a session dies.
//
// In a fleet a handle outlives the process that created it, and the manager is
// where that is handled: every handle it creates is recorded in a Registry
// (when one is installed), a handle this instance does not hold is adopted from
// its record when a loop asks for it, and Sweep reclaims what no live session
// can still be using. With no registry — the single-instance default, and every
// unit test that does not install one — the maps below are the whole story and
// the behaviour is what it always was.
type Manager struct {
	mu        sync.Mutex
	handles   map[string]*Handle  // handle ID → handle
	byOwner   map[string][]string // session key → handle IDs, in creation order
	providers map[string]ShellProvider
	reg       Registry
	instance  string // this process's id, written into every record
	log       *slog.Logger
}

// NewManager creates the shell manager with the registered providers. A nil
// logger discards: the manager is usable without one, like the rest of the
// engine's constructors.
func NewManager(providers []ShellProvider, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	m := &Manager{
		handles:   map[string]*Handle{},
		byOwner:   map[string][]string{},
		providers: map[string]ShellProvider{},
		log:       log.With("module", "shell.manager"),
	}
	for _, p := range providers {
		m.providers[p.Name()] = p
	}
	return m
}

// SetRegistry installs the durable handle registry and the instance id written
// into every record. main calls it once the shared store is open; a manager
// without one is process-local, which is what a single-instance deployment
// wants and costs nothing.
func (m *Manager) SetRegistry(reg Registry, instance string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reg = reg
	m.instance = instance
}

// Spawn creates a new shell handle owned by the given session key.
func (m *Manager) Spawn(ctx context.Context, owner string, providerName string, opts SpawnOpts) (*Handle, error) {
	p, ok := m.providers[providerName]
	if !ok {
		return nil, fmt.Errorf("unknown shell provider %q", providerName)
	}
	h, err := p.Spawn(ctx, opts)
	if err != nil {
		return nil, err
	}
	h.Provider = providerName
	h.owner = owner
	m.remember(h)

	m.log.Info("shell handle spawned", "handle_id", h.ID, "provider", providerName, "owner", owner)
	m.record(ctx, recordOf(h, owner, m.instanceID()))
	return h, nil
}

// Exec runs a command inside a shell handle owned by a session.
func (m *Manager) Exec(ctx context.Context, owner string, handleID string, cmd string) (*ExecResult, error) {
	h, p, err := m.resolve(ctx, owner, handleID)
	if err != nil {
		return nil, err
	}
	return p.Exec(ctx, h, cmd)
}

// Read reads content from a path inside a shell handle.
func (m *Manager) Read(ctx context.Context, owner string, handleID string, path string) ([]byte, error) {
	h, p, err := m.resolve(ctx, owner, handleID)
	if err != nil {
		return nil, err
	}
	return p.Read(ctx, h, path)
}

// Write copies content to a path inside a shell handle.
func (m *Manager) Write(ctx context.Context, owner string, handleID string, path string, content []byte) error {
	h, p, err := m.resolve(ctx, owner, handleID)
	if err != nil {
		return err
	}
	return p.Write(ctx, h, path, content)
}

// Destroy tears down a shell handle owned by a session, and its record.
//
// The handle need not be one this instance holds: after a failover the session
// is here and the container was created elsewhere, and tearing it down is
// exactly what the loop asked for. The resource is released from the record in
// that case, and the record goes only once the release succeeded, so a Docker
// host that is briefly unreachable leaves the work for the reclaim pass rather
// than losing the container.
func (m *Manager) Destroy(ctx context.Context, owner string, handleID string) error {
	if handleID == "" {
		return fmt.Errorf("missing shell handle id")
	}
	if h, ok := m.remembered(handleID); ok {
		if h.owner != owner {
			return fmt.Errorf("shell handle %q not owned by %q", handleID, owner)
		}
		p, ok := m.providers[h.Provider]
		if !ok {
			return fmt.Errorf("unknown shell provider %q", h.Provider)
		}
		if err := p.Destroy(ctx, h); err != nil {
			return err
		}
		m.forget(ctx, h.ID)
		return nil
	}

	reg := m.registry()
	if reg == nil {
		return fmt.Errorf("shell handle %q not found", handleID)
	}
	rec, ok, err := reg.Load(ctx, handleID)
	if err != nil {
		return fmt.Errorf("shell handle %q: %w", handleID, err)
	}
	if !ok {
		// Already gone: a second destroy is a destroy.
		return nil
	}
	if rec.Owner != owner {
		return fmt.Errorf("shell handle %q not owned by %q", handleID, owner)
	}
	if err := m.release(ctx, rec); err != nil {
		return err
	}
	m.log.Info("shell handle destroyed from its record", "handle_id", rec.ID, "instance", rec.Instance, "owner", owner)
	return nil
}

// Current returns the owner's live handle, or nil when the session has none.
//
// "Current" means the session's shell rather than this process's: a handle
// created by an instance that has since died is adopted, because from the
// session's point of view nothing changed.
func (m *Manager) Current(ctx context.Context, owner string) (*Handle, error) {
	if h := m.live(owner); h != nil {
		return h, nil
	}
	reg := m.registry()
	if reg == nil {
		return nil, nil
	}
	recs, err := reg.List(ctx, owner)
	if err != nil {
		return nil, err
	}
	// A session that spawned a second container after its first became
	// unreachable means the newer one, so the newest running record wins.
	newest := -1
	for i, rec := range recs {
		if rec.State != HandleRunning {
			continue
		}
		if newest < 0 || rec.CreatedAt >= recs[newest].CreatedAt {
			newest = i
		}
	}
	if newest < 0 {
		return nil, nil
	}
	return m.adopted(ctx, recs[newest])
}

// Ensure returns the owner's live handle, spawning one from the given provider
// and opts when none exists. Owners are session keys (one loop coroutine
// each), so the check-then-spawn window cannot double-spawn within a session.
//
// A handle that exists but cannot be reached from here — an SSH connection
// whose instance died, a container that is gone — is not insisted on: its
// record is released and a fresh handle is spawned. Anything else would leave
// the session unable to run a command ever again because of a shell it cannot
// use.
func (m *Manager) Ensure(ctx context.Context, owner, providerName string, opts SpawnOpts) (*Handle, error) {
	if h := m.live(owner); h != nil {
		return h, nil
	}
	if reg := m.registry(); reg != nil {
		recs, err := reg.List(ctx, owner)
		if err != nil {
			// Not a spawn-with-a-shrug: an unreadable registry may well hold a
			// live handle, and a second container for one session is worse than
			// an error the loop can retry.
			return nil, fmt.Errorf("shell ensure %s: %w", owner, err)
		}
		for _, rec := range recs {
			if rec.State != HandleRunning {
				continue
			}
			h, err := m.adopted(ctx, rec)
			if err == nil {
				return h, nil
			}
			m.log.Warn("shell handle not reachable from here; releasing it and spawning a new one",
				"handle_id", rec.ID, "provider", rec.Provider, "err", err)
			if err := m.release(ctx, rec); err != nil {
				m.log.Warn("shell handle release failed", "handle_id", rec.ID, "err", err)
			}
		}
	}
	return m.Spawn(ctx, owner, providerName, opts)
}

// ReapSession destroys every shell handle belonging to a session key: the ones
// this instance holds, and the records it does not.
//
// The second part is the fleet's cleanup path. A session that ends takes its
// shells with it, and after a failover the session ends on an instance that
// never created them — so the instance that owns the session at the end is the
// one that removes its containers.
func (m *Manager) ReapSession(ctx context.Context, owner string) {
	for _, id := range m.owned(owner) {
		h, ok := m.remembered(id)
		if !ok {
			continue
		}
		p, ok := m.providers[h.Provider]
		if !ok {
			continue
		}
		// Ignore errors during reap — best-effort cleanup.
		if err := p.Destroy(ctx, h); err != nil {
			m.log.Warn("reap shell handle failed", "handle_id", id, "err", err)
		}
		m.forget(ctx, id)
	}

	if reg := m.registry(); reg != nil {
		recs, err := reg.List(ctx, owner)
		if err != nil {
			m.log.Warn("reap of recorded shell handles failed", "owner", owner, "err", err)
		}
		for _, rec := range recs {
			if _, held := m.remembered(rec.ID); held {
				continue // handled above
			}
			if err := m.release(ctx, rec); err != nil {
				m.log.Warn("reap of a remote shell handle failed", "handle_id", rec.ID, "err", err)
			}
		}
	}
	m.log.Info("session shell handles reaped", "owner", owner)
}

// Sweep reclaims handles no live session can still be using: the shells of
// sessions that ended while their instance was gone. Nothing else would ever
// remove them — a killed instance never runs its own reap, so without this its
// containers stay on the host forever.
//
// live answers "is this session alive anywhere in the deployment", which is the
// session hub's lease table; it is asked rather than assumed, so a handle whose
// session is alive but idle is kept, and one whose liveness cannot be
// established is kept too (an unanswered question is not a licence to destroy).
//
// A handle whose session is alive has its clock refreshed instead. The window
// is measured from the last time the handle was known to belong to a live
// session, not from when it was created: a container created last week in a
// session that was alive until a minute ago must survive its instance's death,
// because that is the failover it exists for.
func (m *Manager) Sweep(ctx context.Context, grace time.Duration, live func(ctx context.Context, owner string) (bool, error)) (int, error) {
	reg := m.registry()
	if reg == nil {
		return 0, nil
	}
	recs, err := reg.List(ctx, "")
	if err != nil {
		return 0, fmt.Errorf("shell reclaim: %w", err)
	}
	now := time.Now()
	reclaimed := 0
	for _, rec := range recs {
		alive, err := live(ctx, rec.Owner)
		if err != nil {
			m.log.Warn("shell reclaim skipped: session liveness unknown",
				"handle_id", rec.ID, "owner", rec.Owner, "err", err)
			continue
		}
		if alive {
			// The clock only has to be accurate to within the grace period, so
			// it is refreshed at a fraction of it: a write per handle per pass
			// would buy nothing.
			if now.Sub(time.Unix(rec.LiveAt, 0)) >= grace/4 {
				rec.LiveAt = now.Unix()
				m.record(ctx, rec)
			}
			continue
		}
		last := rec.LiveAt
		if rec.CreatedAt > last {
			last = rec.CreatedAt // a record with no clock at all still has an age
		}
		if now.Sub(time.Unix(last, 0)) < grace {
			continue
		}
		if err := m.release(ctx, rec); err != nil {
			m.log.Warn("shell reclaim failed; the handle survives for the next pass",
				"handle_id", rec.ID, "err", err)
			continue
		}
		m.log.Info("shell handle reclaimed", "handle_id", rec.ID, "provider", rec.Provider,
			"owner", rec.Owner, "instance", rec.Instance, "unused_for", now.Sub(time.Unix(last, 0)).String())
		reclaimed++
	}
	return reclaimed, nil
}

// resolve finds a handle and its provider, adopting from the registry when this
// instance does not hold it.
func (m *Manager) resolve(ctx context.Context, owner, handleID string) (*Handle, ShellProvider, error) {
	h, err := m.lookup(ctx, owner, handleID)
	if err != nil {
		return nil, nil, err
	}
	p, ok := m.providers[h.Provider]
	if !ok {
		return nil, nil, fmt.Errorf("unknown shell provider %q", h.Provider)
	}
	return h, p, nil
}

// lookup resolves a handle for an owner: from memory when this instance holds
// it, otherwise from the registry.
func (m *Manager) lookup(ctx context.Context, owner, handleID string) (*Handle, error) {
	if handleID == "" {
		return nil, fmt.Errorf("missing shell handle id")
	}
	if h, ok := m.remembered(handleID); ok {
		// Ownership check: the handle must belong to this session.
		if h.owner != owner {
			return nil, fmt.Errorf("shell handle %q not owned by %q", handleID, owner)
		}
		return h, nil
	}
	return m.adopt(ctx, owner, handleID)
}

// adopt rebuilds a handle for this owner from its record. It is how a session
// that moved to this instance — or an instance that restarted — reaches a
// resource that already exists.
func (m *Manager) adopt(ctx context.Context, owner, handleID string) (*Handle, error) {
	reg := m.registry()
	if reg == nil {
		return nil, fmt.Errorf("shell handle %q not found", handleID)
	}
	rec, ok, err := reg.Load(ctx, handleID)
	if err != nil {
		return nil, fmt.Errorf("shell handle %q: %w", handleID, err)
	}
	if !ok {
		// Not "not found": an id that was never registered and one whose record
		// the reclaim pass removed are the same answer to a loop — spawn a new
		// shell — and saying which is which helps nobody.
		return nil, fmt.Errorf("shell handle %q is not registered: it was never created here, or it was reclaimed after going unused", handleID)
	}
	if rec.Owner != owner {
		return nil, fmt.Errorf("shell handle %q not owned by %q", handleID, owner)
	}
	if rec.State != HandleRunning {
		return nil, fmt.Errorf("shell handle %q is no longer running", handleID)
	}
	return m.adopted(ctx, rec)
}

// adopted remembers a handle rebuilt from a record, and marks the record live:
// the session is here now, so the reclaim pass must not read the handle as
// abandoned while it is in use.
func (m *Manager) adopted(ctx context.Context, rec Record) (*Handle, error) {
	h, err := m.attach(rec)
	if err != nil {
		return nil, err
	}
	h = m.remember(h)
	m.log.Info("shell handle adopted", "handle_id", h.ID, "provider", h.Provider,
		"owner", rec.Owner, "from_instance", rec.Instance)
	rec.LiveAt = time.Now().Unix()
	m.record(ctx, rec)
	return h, nil
}

// attach asks a provider to rebuild a handle for one of its records.
func (m *Manager) attach(rec Record) (*Handle, error) {
	p, ok := m.providers[rec.Provider]
	if !ok {
		return nil, fmt.Errorf("unknown shell provider %q", rec.Provider)
	}
	att, ok := p.(Attacher)
	if !ok {
		return nil, fmt.Errorf("shell handle %q is a live %s connection, which cannot be re-established on another instance",
			rec.ID, rec.Provider)
	}
	h, err := att.Attach(rec)
	if err != nil {
		return nil, err
	}
	h.owner = rec.Owner
	return h, nil
}

// release destroys the resource a record describes without a live handle for it.
// A provider with nothing left to release (an SSH connection dies with its
// process) is not an error: the record goes either way.
func (m *Manager) release(ctx context.Context, rec Record) error {
	if p, ok := m.providers[rec.Provider]; ok {
		if f, ok := p.(Forgetter); ok {
			if err := f.Forget(ctx, rec); err != nil {
				return fmt.Errorf("release shell handle %q: %w", rec.ID, err)
			}
		}
	}
	reg := m.registry()
	if reg == nil {
		return nil
	}
	if err := reg.Delete(ctx, rec.ID); err != nil {
		return fmt.Errorf("shell handle %q was released but its record remains: %w", rec.ID, err)
	}
	return nil
}

// record writes a handle's record. A failure is reported and not propagated:
// the shell that just ran is not undone by a store that could not note it. The
// warning names the handle, because a handle whose record never landed is one
// that nothing can reclaim if this instance is killed.
func (m *Manager) record(ctx context.Context, rec Record) {
	reg := m.registry()
	if reg == nil {
		return
	}
	if err := reg.Save(ctx, rec); err != nil {
		m.log.Warn("shell handle not recorded: no instance can adopt or reclaim it",
			"handle_id", rec.ID, "err", err)
	}
}

// forget drops a handle from memory and deletes its record.
func (m *Manager) forget(ctx context.Context, id string) {
	m.mu.Lock()
	h, ok := m.handles[id]
	if ok {
		delete(m.handles, id)
		m.byOwner[h.owner] = removeID(m.byOwner[h.owner], id)
	}
	m.mu.Unlock()

	reg := m.registry()
	if reg == nil {
		return
	}
	if err := reg.Delete(ctx, id); err != nil {
		m.log.Warn("shell handle record not deleted: a reclaim pass will retry", "handle_id", id, "err", err)
	}
}

// remember adds a handle to this process's view of the world. A handle already
// remembered wins: two goroutines adopting the same record is one handle.
func (m *Manager) remember(h *Handle) *Handle {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.handles[h.ID]; ok {
		return existing
	}
	m.handles[h.ID] = h
	m.byOwner[h.owner] = append(m.byOwner[h.owner], h.ID)
	return h
}

// remembered returns the in-memory handle for an id, owned by anyone.
func (m *Manager) remembered(id string) (*Handle, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.handles[id]
	return h, ok
}

// live returns the owner's running in-memory handle, or nil.
func (m *Manager) live(owner string) *Handle {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.byOwner[owner] {
		if h, ok := m.handles[id]; ok && h.State == HandleRunning {
			return h
		}
	}
	return nil
}

// owned lists the ids this process holds for an owner, in creation order.
func (m *Manager) owned(owner string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string{}, m.byOwner[owner]...)
}

// registry reports the installed registry, if any.
func (m *Manager) registry() Registry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reg
}

// instanceID reports this process's id, for records.
func (m *Manager) instanceID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.instance
}

// removeID removes one id from a slice, preserving order.
func removeID(ids []string, id string) []string {
	for i, v := range ids {
		if v == id {
			return append(ids[:i:i], ids[i+1:]...)
		}
	}
	return ids
}
