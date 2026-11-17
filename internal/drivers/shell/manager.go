// Package shell holds connections to real remote machines: the SSH shell
// provider and the manager that owns its handles.
//
// It used to hold a Docker provider too, and with it a durable handle
// registry, adoption after failover and a reclaim sweep — the machinery a
// long-lived container needs to survive the instance that started it. Shell
// commands on the engine's own host now run as one-off sandbox jobs
// (internal/core/shelljobs), which leave nothing to track. What remains here
// is the one kind of shell that really is a thing with a lifetime: an SSH
// connection, which lives in the process that dialled it and ends with the
// session that asked for it.
package shell

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
)

// HandleState tracks the lifecycle of a shell handle.
type HandleState int

const (
	// HandleState values are pinned: Handle carries State as a numeric JSON
	// field, so the numbers are a wire contract even for values Go no longer
	// names. 0 (pending) and 2 (stopped) are reserved.
	HandleRunning   HandleState = 1 // the connection is live
	HandleDestroyed HandleState = 3 // closed
)

// SpawnOpts are the parameters for creating a new shell handle.
type SpawnOpts struct {
	WorkDir string            // working directory
	Env     map[string]string // environment variables

	Host     string // host:port
	User     string
	Password string
	KeyFile  string

	// ShellOpts is the escape hatch for provider-specific options that do not
	// map to a typed field above. Each provider reads the keys it knows.
	ShellOpts map[string]any
}

// ExecResult is the output of a command executed inside a shell handle.
type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Duration int64  `json:"duration_ms"`
}

// Handle is a managed shell handle.
type Handle struct {
	ID       string         `json:"id"`
	Provider string         `json:"provider"`
	State    HandleState    `json:"state"`
	Meta     map[string]any `json:"meta,omitempty"`

	owner    string // the session key this handle belongs to
	internal any    // provider-specific state (e.g. *ssh.Client)
	mu       sync.RWMutex
}

// ShellProvider is the interface for a shell backend.
type ShellProvider interface {
	Name() string
	Spawn(ctx context.Context, opts SpawnOpts) (*Handle, error)
	Exec(ctx context.Context, handle *Handle, cmd string) (*ExecResult, error)
	Read(ctx context.Context, handle *Handle, path string) ([]byte, error)
	Write(ctx context.Context, handle *Handle, path string, content []byte) error
	Destroy(ctx context.Context, handle *Handle) error
	Alive(handle *Handle) bool
}

// Manager owns the shell handles of this process and reaps a session's
// handles when the session ends.
type Manager struct {
	mu        sync.Mutex
	handles   map[string]*Handle  // handle ID → handle
	byOwner   map[string][]string // session key → handle IDs, in creation order
	providers map[string]ShellProvider
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
	m.mu.Lock()
	m.handles[h.ID] = h
	m.byOwner[owner] = append(m.byOwner[owner], h.ID)
	m.mu.Unlock()

	m.log.Info("shell handle spawned", "handle_id", h.ID, "provider", providerName, "owner", owner)
	return h, nil
}

// Exec runs a command inside a shell handle owned by a session.
func (m *Manager) Exec(ctx context.Context, owner string, handleID string, cmd string) (*ExecResult, error) {
	h, p, err := m.resolve(owner, handleID)
	if err != nil {
		return nil, err
	}
	return p.Exec(ctx, h, cmd)
}

// Read reads content from a path inside a shell handle.
func (m *Manager) Read(ctx context.Context, owner string, handleID string, path string) ([]byte, error) {
	h, p, err := m.resolve(owner, handleID)
	if err != nil {
		return nil, err
	}
	return p.Read(ctx, h, path)
}

// Write copies content to a path inside a shell handle.
func (m *Manager) Write(ctx context.Context, owner string, handleID string, path string, content []byte) error {
	h, p, err := m.resolve(owner, handleID)
	if err != nil {
		return err
	}
	return p.Write(ctx, h, path, content)
}

// Destroy tears down a shell handle owned by a session.
func (m *Manager) Destroy(ctx context.Context, owner string, handleID string) error {
	h, p, err := m.resolve(owner, handleID)
	if err != nil {
		return err
	}
	if err := p.Destroy(ctx, h); err != nil {
		return err
	}
	m.forget(h.ID)
	return nil
}

// Current returns the owner's live handle, or nil when the session has none.
func (m *Manager) Current(owner string) *Handle {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.byOwner[owner] {
		if h, ok := m.handles[id]; ok && h.State == HandleRunning {
			return h
		}
	}
	return nil
}

// Ensure returns the owner's live handle, spawning one from the given provider
// and opts when none exists. Owners are session keys (one loop coroutine
// each), so the check-then-spawn window cannot double-spawn within a session.
func (m *Manager) Ensure(ctx context.Context, owner, providerName string, opts SpawnOpts) (*Handle, error) {
	if h := m.Current(owner); h != nil {
		return h, nil
	}
	return m.Spawn(ctx, owner, providerName, opts)
}

// Owned reports how many live shell handles a session owns in this process.
// The supervisor's idle janitor asks before unloading: a session holding a
// connection has something that would not survive being unloaded.
func (m *Manager) Owned(owner string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.byOwner[owner])
}

// ReapSession destroys every shell handle belonging to a session key.
func (m *Manager) ReapSession(ctx context.Context, owner string) {
	m.mu.Lock()
	ids := append([]string{}, m.byOwner[owner]...)
	m.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	for _, id := range ids {
		m.mu.Lock()
		h, ok := m.handles[id]
		m.mu.Unlock()
		if !ok {
			continue
		}
		if p, ok := m.providers[h.Provider]; ok {
			// Best-effort: the session is gone either way.
			if err := p.Destroy(ctx, h); err != nil {
				m.log.Warn("reap shell handle failed", "handle_id", id, "err", err)
			}
		}
		m.forget(id)
	}
	m.log.Info("session shell handles reaped", "owner", owner, "handles", len(ids))
}

// resolve finds a handle and its provider, checking that the session owns it.
func (m *Manager) resolve(owner, handleID string) (*Handle, ShellProvider, error) {
	if handleID == "" {
		return nil, nil, fmt.Errorf("missing shell handle id")
	}
	m.mu.Lock()
	h, ok := m.handles[handleID]
	m.mu.Unlock()
	if !ok {
		// Never created here, or created on an instance this session has since
		// left. A connection does not move with a session, and saying so tells
		// the loop what to do about it.
		return nil, nil, fmt.Errorf("shell handle %q not found on this instance: a remote connection ends with the instance that opened it; spawn a new one", handleID)
	}
	if h.owner != owner {
		return nil, nil, fmt.Errorf("shell handle %q not owned by %q", handleID, owner)
	}
	p, ok := m.providers[h.Provider]
	if !ok {
		return nil, nil, fmt.Errorf("unknown shell provider %q", h.Provider)
	}
	return h, p, nil
}

// forget drops a handle from the manager.
func (m *Manager) forget(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.handles[id]
	if !ok {
		return
	}
	delete(m.handles, id)
	ids := m.byOwner[h.owner]
	for i, v := range ids {
		if v == id {
			ids = append(ids[:i:i], ids[i+1:]...)
			break
		}
	}
	if len(ids) == 0 {
		delete(m.byOwner, h.owner)
	} else {
		m.byOwner[h.owner] = ids
	}
}
