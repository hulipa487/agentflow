package supervisor

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"agentflow/internal/core/caps"
	"agentflow/internal/core/gateway"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/scheduler"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/shell"
)

func metric(t *testing.T, name string) int64 {
	t.Helper()
	c, ok := metrics.Global().Get(name)
	if !ok {
		t.Fatalf("metric %q is not registered", name)
	}
	return c.Value()
}

// idleSupervisor builds a supervisor with a configurable handler map and an
// optional scheduler/shell manager, started after the caller sets its policy.
func idleSupervisor(t *testing.T, loopSrc string, handlers map[string]session.OpHandler, sched *scheduler.Service, shellMgr *shell.Manager) (*Supervisor, *benchDriver) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := gateway.NewRegistry(log)
	drv := &benchDriver{replies: make(chan string, 16)}
	gw.Register(drv)
	defs := map[string]*AgentDef{
		"echo": {
			Info:     &session.Info{Name: "echo", HistoryBudget: 100},
			Handlers: handlers,
			LoopSrc:  directive() + loopSrc,
		},
	}
	sup := New(defs, gw, pool.New(2), shellMgr, log)
	if sched != nil {
		sup.SetScheduler(sched)
	}
	sup.SetIdleUnload(25 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	sup.Start(ctx)
	t.Cleanup(func() {
		cancel()
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer waitCancel()
		sup.Wait(waitCtx)
	})
	return sup, drv
}

func deliverAndWait(t *testing.T, sup *Supervisor, drv *benchDriver, key, text string) string {
	t.Helper()
	if err := sup.DeliverLocal("echo", key, session.Message{
		ID: "m-" + key, Type: "user", From: "user:test", Text: text,
		Channel: "bench", ReplyTo: key,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-drv.replies:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
	}
	return ""
}

func waitNoSession(t *testing.T, sup *Supervisor, key string) {
	t.Helper()
	waitFor(t, "session "+key+" to leave", 5*time.Second, func() bool {
		sup.mu.Lock()
		defer sup.mu.Unlock()
		_, live := sup.sessions[key]
		return !live
	})
}

// TestIdleSessionIsUnloadedAndComesBack: an idle conversation is unloaded after
// the window, its id is not retired, and the next message recreates it. The
// loop's Lua locals are gone; state written under session.state survives.
func TestIdleSessionIsUnloadedAndComesBack(t *testing.T) {
	store, err := runtime.OpenSQLite(filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	handlers := caps.SessionStateHandlers{Store: store}.Handlers()
	sup, drv := idleSupervisor(t, `
function loop() {
  while (true) {
    const m = session.inbox();
    const n = session.state.get("count") || 0;
    session.state.set("count", n + 1);
    session.send(String(n + 1));
  }
}`, handlers, nil, nil)

	unloadsBefore := metric(t, "agentflow_sessions_unloaded")
	if got := deliverAndWait(t, sup, drv, "chat", "hi"); got != "1" {
		t.Fatalf("first turn = %q", got)
	}
	time.Sleep(40 * time.Millisecond)
	sup.SweepIdle()
	waitNoSession(t, sup, "echo|chat")

	if n := metric(t, "agentflow_sessions_unloaded") - unloadsBefore; n != 1 {
		t.Fatalf("agentflow_sessions_unloaded moved by %d; want 1", n)
	}
	sup.mu.Lock()
	retired := len(sup.retired)
	sup.mu.Unlock()
	if retired != 0 {
		t.Fatalf("an unloaded session was retired (%d entries); the next message must be able to recreate it", retired)
	}

	// The next message recreates the actor, and session.state still holds 1.
	if got := deliverAndWait(t, sup, drv, "chat", "again"); got != "2" {
		t.Fatalf("second turn = %q; want 2 (state survived the unload)", got)
	}
}

// TestLiveResourcesBlockUnload: a session holding a timer, a shell, a live
// child or a parked agent.request is not idle even when its mailbox is empty.
func TestLiveResourcesBlockUnload(t *testing.T) {
	t.Run("timer", func(t *testing.T) {
		sched := scheduler.New(nil)
		t.Cleanup(func() { sched.CancelOwner("echo|chat") })
		sup, drv := idleSupervisor(t, `
function loop() {
  const m = session.inbox();
  scheduler.after(300);
  session.send(m.text);
  while (true) { session.inbox(); }
}`, map[string]session.OpHandler{}, sched, nil)
		deliverAndWait(t, sup, drv, "chat", "hi")
		time.Sleep(40 * time.Millisecond)
		sup.SweepIdle()
		sup.mu.Lock()
		_, live := sup.sessions["echo|chat"]
		sup.mu.Unlock()
		if !live {
			t.Fatal("a session waiting on a timer was unloaded")
		}
	})

	t.Run("pending request", func(t *testing.T) {
		sup, drv := idleSupervisor(t, echoLoop, map[string]session.OpHandler{}, nil, nil)
		deliverAndWait(t, sup, drv, "chat", "hi")
		_, _, cancel := sup.requests.Open("echo|chat", "other")
		defer cancel()
		time.Sleep(40 * time.Millisecond)
		sup.SweepIdle()
		sup.mu.Lock()
		_, live := sup.sessions["echo|chat"]
		sup.mu.Unlock()
		if !live {
			t.Fatal("a session waiting on an agent.request was unloaded")
		}
	})

	t.Run("live child", func(t *testing.T) {
		sup, drv := idleSupervisor(t, echoLoop, map[string]session.OpHandler{}, nil, nil)
		deliverAndWait(t, sup, drv, "chat", "hi")
		child := session.New("echo|child", session.Identity{SessionID: "echo|child", Agent: "echo", ParentID: "echo|chat"},
			&session.Info{Name: "echo"}, nil, nil, nil, nil, nil, map[string]session.OpHandler{}, pool.New(1),
			slog.New(slog.NewTextHandler(io.Discard, nil)))
		sup.mu.Lock()
		sup.sessions["echo|child"] = child
		sup.cancels["echo|child"] = func() {}
		sup.mu.Unlock()
		t.Cleanup(func() {
			sup.mu.Lock()
			delete(sup.sessions, "echo|child")
			delete(sup.cancels, "echo|child")
			sup.mu.Unlock()
		})
		time.Sleep(40 * time.Millisecond)
		sup.SweepIdle()
		sup.mu.Lock()
		_, live := sup.sessions["echo|chat"]
		sup.mu.Unlock()
		if !live {
			t.Fatal("a session with a live child was unloaded")
		}
	})

	t.Run("shell", func(t *testing.T) {
		shellMgr := shell.NewManager([]shell.ShellProvider{fakeProvider{}}, nil)
		sup, drv := idleSupervisor(t, echoLoop, map[string]session.OpHandler{}, nil, shellMgr)
		deliverAndWait(t, sup, drv, "chat", "hi")
		if _, err := shellMgr.Spawn(context.Background(), "echo|chat", "fake", shell.SpawnOpts{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(40 * time.Millisecond)
		sup.SweepIdle()
		sup.mu.Lock()
		_, live := sup.sessions["echo|chat"]
		sup.mu.Unlock()
		if !live {
			t.Fatal("a session holding a shell was unloaded")
		}
	})
}

// fakeProvider is a ShellProvider that hands out handles without a container.
type fakeProvider struct{}

func (fakeProvider) Name() string { return "fake" }
func (fakeProvider) Spawn(context.Context, shell.SpawnOpts) (*shell.Handle, error) {
	return &shell.Handle{ID: "h1", Provider: "fake"}, nil
}
func (fakeProvider) Exec(context.Context, *shell.Handle, string) (*shell.ExecResult, error) {
	return &shell.ExecResult{}, nil
}
func (fakeProvider) Read(context.Context, *shell.Handle, string) ([]byte, error) { return nil, nil }
func (fakeProvider) Write(context.Context, *shell.Handle, string, []byte) error  { return nil }
func (fakeProvider) Destroy(context.Context, *shell.Handle) error                { return nil }
func (fakeProvider) Alive(*shell.Handle) bool                                    { return true }

// TestUnloadReleasesTheLease: the next message may land on another instance, so
// the unloaded session's claim is given up at once instead of waiting out the
// session TTL.
func TestUnloadReleasesTheLease(t *testing.T) {
	sup, drv := idleSupervisor(t, echoLoop, map[string]session.OpHandler{}, nil, nil)
	hub := &countingHub{}
	sup.SetHub(hub)
	// DeliverLocal skips the hub's routing while still making this instance the
	// holder, the same shape as a session that arrived here and then went idle.
	deliverAndWait(t, sup, drv, "chat", "hi")
	time.Sleep(40 * time.Millisecond)
	sup.SweepIdle()
	waitNoSession(t, sup, "echo|chat")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(hub.releasedKeys()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := hub.releasedKeys(); len(got) != 1 || got[0] != "echo|chat" {
		t.Fatalf("released = %v; want [echo|chat]", got)
	}
}

// TestPersistentAgentIsNotUnloaded: a daemon's idle boot session is the daemon.
func TestPersistentAgentIsNotUnloaded(t *testing.T) {
	sup, drv := idleSupervisor(t, echoLoop, map[string]session.OpHandler{}, nil, nil)
	sup.defs["echo"].Persistent = true
	deliverAndWait(t, sup, drv, "chat", "hi")
	time.Sleep(40 * time.Millisecond)
	sup.SweepIdle()
	sup.mu.Lock()
	_, live := sup.sessions["echo|chat"]
	sup.mu.Unlock()
	if !live {
		t.Fatal("a persistent agent's session was unloaded")
	}
}
