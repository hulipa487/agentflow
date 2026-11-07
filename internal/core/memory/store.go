// Package memory implements the provider → backend → store abstraction.
package memory

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Manager wraps a Registry and drives per-table GC across all backends.
type Manager struct {
	reg      *Registry
	log      *slog.Logger
	stopCh   chan struct{}
	stopOnce func()
	// generation resolves a session's ownership generation. It is wired from
	// the session hub; nil (a library consumer, or a runtime without a hub)
	// leaves instance-local scopes ungenerationed, which is the old behaviour.
	generation func(sessionKey string) int64
}

// SetGenerationSource wires the per-session ownership generation used to key
// instance-local memory. Called by main once the hub exists.
func (m *Manager) SetGenerationSource(fn func(sessionKey string) int64) { m.generation = fn }

// Generation reports the generation for a session, or 0 when unknown.
func (m *Manager) Generation(sessionKey string) int64 {
	if m.generation == nil || sessionKey == "" {
		return 0
	}
	return m.generation(sessionKey)
}

// NewManager creates a manager around an opened registry.
func NewManager(reg *Registry, log *slog.Logger) *Manager {
	return &Manager{reg: reg, log: log.With("module", "memory.manager")}
}

// BindForTable resolves a physical table name inside an agent's memory profile
// to a backend handle and binding.
func (m *Manager) BindForTable(am AgentMemory, table string) (BackendHandle, StoreBinding, error) {
	b, ok := am.Tables[table]
	if !ok {
		return nil, StoreBinding{}, fmt.Errorf("table %q not declared in agent memory profile", table)
	}
	h, ok := m.reg.Handle(b.Backend)
	if !ok {
		return nil, StoreBinding{}, fmt.Errorf("backend %q for table %q is not open", b.Backend, table)
	}
	return h, b, nil
}

// Features reports the provider features of a named backend (nil when
// unknown), so op handlers can gate capability-specific behavior.
func (m *Manager) Features(backend string) []string {
	return m.reg.Features(backend)
}

// StartGC runs retention/window enforcement on a ticker until stopped.
func (m *Manager) StartGC(ctx context.Context, interval time.Duration, profiles []AgentMemory) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	t := time.NewTicker(interval)
	stop := make(chan struct{})
	m.stopCh = stop
	m.stopOnce = sync.OnceFunc(func() { close(stop) })

	go func() {
		defer t.Stop()
		for {
			select {
			case <-t.C:
				m.runGC(profiles)
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Stop halts the GC goroutine.
func (m *Manager) Stop() {
	if m.stopOnce != nil {
		m.stopOnce()
	}
}

func (m *Manager) runGC(profiles []AgentMemory) {
	// Collect unique (backend, table, window) triples.
	type key struct{ backend, table string }
	targets := map[key]int{}
	for _, am := range profiles {
		for sname, s := range am.Stores {
			// Logical store names map 1:1 to physical tables here.
			_ = sname
			k := key{backend: s.Backend, table: s.Table}
			if s.Window > targets[k] {
				targets[k] = s.Window
			}
		}
	}
	for k, window := range targets {
		h, ok := m.reg.Handle(k.backend)
		if !ok {
			continue
		}
		if err := h.GC(k.table, window); err != nil {
			m.log.Warn("gc failed", "backend", k.backend, "table", k.table, "err", err)
		}
	}
}
