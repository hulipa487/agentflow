// Package shell provides the Docker and SSH shell execution providers.
// Shell handles created by a session are owned by that session and reaped
// when the session dies.
package shell

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// HandleState tracks the lifecycle of a shell handle.
type HandleState int

const (
	HandlePending   HandleState = iota // spawn initiated but not yet confirmed
	HandleRunning                      // container/connection is live
	HandleStopped                      // graceful stop
	HandleDestroyed                    // reclaimed
)

// SpawnOpts are the parameters for creating a new shell handle.
type SpawnOpts struct {
	Image    string            // Docker image name (ignored for SSH)
	WorkDir  string            // working directory
	Env      map[string]string // environment variables
	Network  string            // "none" | "bridge" | "host"
	MemLimit string            // e.g. "512m"
	CPULimit float64           // e.g. 1.0

	// SSH provider fields.
	Host     string // host:port
	User     string
	Password string
	KeyFile  string

	// ShellOpts is the generic escape hatch for provider-specific options that
	// don't map to a typed field above (e.g. a Vultr region/plan/os_id, or a
	// one-shot docker image override). Each provider reads the keys it knows.
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
	ID       string      `json:"id"`
	Provider string      `json:"provider"`   // "docker" | "ssh"
	Image    string      `json:"image"`      // empty for SSH
	State    HandleState `json:"state"`
	Meta     map[string]any `json:"meta,omitempty"`

	internal any          // provider-specific state (e.g. container ID, *ssh.Client)
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
type Manager struct {
	mu      sync.Mutex
	handles map[string]*Handle    // handle ID → handle
	byOwner map[string][]string   // session key → handle IDs
	providers map[string]ShellProvider
	log     *slog.Logger
}

// NewManager creates the shell manager with the registered providers.
func NewManager(providers []ShellProvider, log *slog.Logger) *Manager {
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

	m.mu.Lock()
	m.handles[h.ID] = h
	m.byOwner[owner] = append(m.byOwner[owner], h.ID)
	m.mu.Unlock()

	m.log.Info("shell handle spawned", "handle_id", h.ID, "provider", providerName, "owner", owner)
	return h, nil
}

// Exec runs a command inside a shell handle owned by a session.
func (m *Manager) Exec(ctx context.Context, owner string, handleID string, cmd string) (*ExecResult, error) {
	h, err := m.lookup(owner, handleID)
	if err != nil {
		return nil, err
	}
	p, ok := m.providers[h.Provider]
	if !ok {
		return nil, fmt.Errorf("unknown shell provider %q", h.Provider)
	}
	return p.Exec(ctx, h, cmd)
}

// Read reads content from a path inside a shell handle.
func (m *Manager) Read(ctx context.Context, owner string, handleID string, path string) ([]byte, error) {
	h, err := m.lookup(owner, handleID)
	if err != nil {
		return nil, err
	}
	p, ok := m.providers[h.Provider]
	if !ok {
		return nil, fmt.Errorf("unknown shell provider %q", h.Provider)
	}
	return p.Read(ctx, h, path)
}

// Write copies content to a path inside a shell handle.
func (m *Manager) Write(ctx context.Context, owner string, handleID string, path string, content []byte) error {
	h, err := m.lookup(owner, handleID)
	if err != nil {
		return err
	}
	p, ok := m.providers[h.Provider]
	if !ok {
		return fmt.Errorf("unknown shell provider %q", h.Provider)
	}
	return p.Write(ctx, h, path, content)
}

// Destroy tears down a shell handle owned by a session.
func (m *Manager) Destroy(ctx context.Context, owner string, handleID string) error {
	h, err := m.lookup(owner, handleID)
	if err != nil {
		return err
	}
	p, ok := m.providers[h.Provider]
	if !ok {
		return fmt.Errorf("unknown shell provider %q", h.Provider)
	}
	return p.Destroy(ctx, h)
}

// ReapSession destroys all shell handles belonging to a session key.
func (m *Manager) ReapSession(owner string) {
	m.mu.Lock()
	ids := append([]string{}, m.byOwner[owner]...)
	delete(m.byOwner, owner)
	m.mu.Unlock()

	for _, id := range ids {
		m.mu.Lock()
		h, ok := m.handles[id]
		m.mu.Unlock()
		if !ok {
			continue
		}
		p, ok := m.providers[h.Provider]
		if !ok {
			continue
		}
		// Ignore errors during reap — best-effort cleanup.
		if err := p.Destroy(context.Background(), h); err != nil {
			m.log.Warn("reap shell handle failed", "handle_id", id, "err", err)
		}
		m.mu.Lock()
		delete(m.handles, id)
		m.mu.Unlock()
	}
	m.log.Info("session shell handles reaped", "owner", owner)
}

func (m *Manager) lookup(owner, handleID string) (*Handle, error) {
	if handleID == "" {
		return nil, fmt.Errorf("missing shell handle id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.handles[handleID]
	if !ok {
		return nil, fmt.Errorf("shell handle %q not found", handleID)
	}
	// Ownership check: the handle must belong to this session.
	found := false
	for _, id := range m.byOwner[owner] {
		if id == handleID {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("shell handle %q not owned by %q", handleID, owner)
	}
	return h, nil
}
