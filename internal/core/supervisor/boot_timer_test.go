package supervisor

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"agentflow/internal/core/gateway"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/scheduler"
	"agentflow/internal/core/session"
)

// probeHandler returns an op handler that forwards op.Text to the channel.
func probeHandler(ch chan string) session.OpHandler {
	return func(ctx context.Context, op session.Op) (string, bool) {
		ch <- op.Text
		return "true", true
	}
}

func awaitProbe(t *testing.T, ch chan string, what string) string {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return ""
	}
}

// TestBootPersistentSpawnsDaemon: a persistent agent's session spawns at boot
// with zero inbound traffic, receiving a Type "boot" message; a
// non-persistent agent stays unspawned.
func TestBootPersistentSpawnsDaemon(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := gateway.NewRegistry(log)
	recorded := make(chan string, 2)
	loop := `function loop()
  local msg = session.inbox()
  af.op({ type = "probe.record", text = msg.type })
end`
	defs := map[string]*AgentDef{
		"daemon": {
			Info:       &session.Info{Name: "daemon", HistoryBudget: 100},
			Handlers:   map[string]session.OpHandler{"probe.record": probeHandler(recorded)},
			LoopSrc:    loop,
			Persistent: true,
		},
		"web": {
			Info:     &session.Info{Name: "web", HistoryBudget: 100},
			Handlers: map[string]session.OpHandler{"probe.record": probeHandler(recorded)},
			LoopSrc:  loop,
		},
	}
	sup := New(defs, gw, pool.New(2), nil, log)
	sup.Start(context.Background())
	sup.BootPersistent(context.Background())

	if got := awaitProbe(t, recorded, "daemon boot turn"); got != "boot" {
		t.Fatalf("daemon first message type = %q, want boot", got)
	}
	rows, _, _ := sup.Snapshot()
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.SessionID] = true
	}
	if !seen["daemon|boot"] {
		t.Fatalf("daemon session not running: %v", seen)
	}
	for id := range seen {
		if id == "web|boot" || (len(id) > 4 && id[:4] == "web|") {
			t.Fatalf("non-persistent agent must not boot: %v", id)
		}
	}
}

// TestTimerMessagesCarryTimerID: two timers on one session deliver messages
// with distinct payload.timer_id values, so the loop can tell them apart.
func TestTimerMessagesCarryTimerID(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := gateway.NewRegistry(log)
	recorded := make(chan string, 8)
	defs := map[string]*AgentDef{
		"tick": {
			Info:     &session.Info{Name: "tick", HistoryBudget: 100},
			Handlers: map[string]session.OpHandler{"probe.record": probeHandler(recorded)},
			LoopSrc: `function loop()
  scheduler.every(0.1)
  scheduler.every(0.17)
  while true do
    local msg = session.inbox()
    if msg.type == "timer" and msg.payload and msg.payload.timer_id then
      af.op({ type = "probe.record", text = msg.payload.timer_id })
    end
  end
end`,
		},
	}
	sup := New(defs, gw, pool.New(2), nil, log)
	sup.Start(context.Background())
	sup.SetScheduler(scheduler.New(log))

	if err := sup.Deliver("tick", "web", session.Message{ID: "m1", Type: "user", From: "u", Ts: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}

	ids := map[string]bool{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(ids) < 2 {
		select {
		case id := <-recorded:
			ids[id] = true
		case <-time.After(100 * time.Millisecond):
		}
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 distinct timer_ids, got %v", ids)
	}
	for id := range ids {
		if len(id) < 6 || id[:6] != "timer-" {
			t.Fatalf("malformed timer_id %q", id)
		}
	}
}
