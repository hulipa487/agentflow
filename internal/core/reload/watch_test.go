package reload

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/core/gateway"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/session"
	"agentflow/internal/core/supervisor"
)

// syncBuf is a goroutine-safe log sink (the watcher polls on its own goroutine
// while the test reads).
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", d, what)
}

// agentsNamed returns the agents whose log line contains msg, read from the
// per-line "agent=" / "session=" attributes.
func agentsNamed(logs, msg string, attr string, agents ...string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, msg) {
			continue
		}
		for _, a := range agents {
			if strings.Contains(line, attr+a) {
				out[a] = true
			}
		}
	}
	return out
}

// TestSharedLoopPathReloadsEveryAgent: two agents bound to one loop directory
// must both reload when a member changes. The watcher's mtime bookkeeping is
// per (agent, path) — keyed by path alone, whichever agent poll() visited first
// would consume the change and the other would stay on the old loop, so the
// edit applied to only half the deployment, depending on map iteration order.
func TestSharedLoopPathReloadsEveryAgent(t *testing.T) {
	shared := t.TempDir()
	member := filepath.Join(shared, "10-main.lua")
	if err := os.WriteFile(member, []byte("function loop() while true do session.inbox() end end\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "20-tools.lua"), []byte("-- tools member\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	loopSrc := "function loop() while true do session.inbox() end end"

	defs := map[string]*supervisor.AgentDef{}
	for _, name := range []string{"alpha", "beta"} {
		defs[name] = &supervisor.AgentDef{
			Info:         &session.Info{Name: name, HistoryBudget: 100},
			Capabilities: map[string]bool{},
			Handlers:     map[string]session.OpHandler{},
			LoopFile:     shared,
			LoopSrc:      loopSrc,
		}
	}

	sup := supervisor.New(defs, gateway.NewRegistry(log), pool.New(2), nil, log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup.Start(ctx)

	// Live sessions, each parked on its mailbox — that is where a reload lands.
	for _, name := range []string{"alpha", "beta"} {
		if err := sup.Deliver(name, "k", session.Message{ID: "m1", Type: "user", From: "u", Text: "hi"}); err != nil {
			t.Fatalf("deliver to %s: %v", name, err)
		}
	}

	w := New(sup, log)
	w.Start()
	defer w.Stop()

	// Touch a member after Start() seeded the mtimes. The new mtime is set
	// explicitly: a same-second write is ambiguous on coarse-mtime filesystems.
	newer := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(shared, "20-tools.lua"), newer, newer); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "both agents to accept the new loop", 10*time.Second, func() bool {
		return len(agentsNamed(logs.String(), "reload: new version accepted", "agent=", "alpha", "beta")) == 2
	})
	waitFor(t, "both sessions to restart", 10*time.Second, func() bool {
		return len(agentsNamed(logs.String(), "hot reload: restarting loop", "session=", "alpha", "beta")) == 2
	})
	if got := agentsNamed(logs.String(), "reload: new version accepted", "agent=", "alpha", "beta"); !got["alpha"] || !got["beta"] {
		t.Fatalf("agents reloaded = %v; want both", got)
	}
}

// TestSharedInstructionsPathUpdatesEveryAgent: the same keying covers an
// instructions file shared by several agents — each one's loop must see the
// new content, not just whichever agent poll() happened to visit first.
func TestSharedInstructionsPathUpdatesEveryAgent(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "shared.md")
	if err := os.WriteFile(shared, []byte("first version\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	defs := map[string]*supervisor.AgentDef{}
	for _, name := range []string{"alpha", "beta"} {
		defs[name] = &supervisor.AgentDef{
			Info:             &session.Info{Name: name, HistoryBudget: 100, Instructions: &session.StringBox{}},
			Capabilities:     map[string]bool{},
			Handlers:         map[string]session.OpHandler{},
			InstructionsPath: shared,
		}
	}

	sup := supervisor.New(defs, gateway.NewRegistry(log), pool.New(1), nil, log)
	sup.Start(context.Background())

	w := New(sup, log)
	w.Start()
	defer w.Stop()

	if err := os.WriteFile(shared, []byte("second version\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	newer := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(shared, newer, newer); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "both agents to pick up the instructions", 10*time.Second, func() bool {
		return len(agentsNamed(logs.String(), "reload: instructions updated", "agent=", "alpha", "beta")) == 2
	})
	for name, def := range defs {
		if got := def.Info.Instructions.Load(); got != "second version\n" {
			t.Fatalf("agent %s instructions = %q", name, got)
		}
	}
}

// TestUnchangedPathDoesNotReload: the bookkeeping must not turn into a reload
// storm — a poll with no edits reloads nothing.
func TestUnchangedPathDoesNotReload(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "loop.lua")
	if err := os.WriteFile(shared, []byte("function loop() end\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	defs := map[string]*supervisor.AgentDef{
		"alpha": {
			Info: &session.Info{Name: "alpha", HistoryBudget: 100}, Capabilities: map[string]bool{},
			Handlers: map[string]session.OpHandler{}, LoopFile: shared,
		},
	}
	sup := supervisor.New(defs, gateway.NewRegistry(log), pool.New(1), nil, log)
	sup.Start(context.Background())

	w := New(sup, log)
	w.Start()
	defer w.Stop()

	// At least two poll ticks with no edits.
	time.Sleep(1200 * time.Millisecond)
	if got := logs.String(); strings.Contains(got, "reload:") {
		t.Fatalf("an untouched loop must not reload:\n%s", got)
	}
}
