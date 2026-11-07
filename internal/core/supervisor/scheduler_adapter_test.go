package supervisor

import (
	"io"
	"log/slog"
	"testing"

	"agentflow/internal/core/scheduler"
	"agentflow/internal/core/session"
)

// A timer tick is traffic for a session, and with a hub installed it has to be
// routed like any other message. The old path wrote straight to the local
// mailbox, which is how an instance that had lost the lease kept running turns
// the new owner was also running.
func TestTimerTickRoutesThroughHub(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := &fakeRouter{}
	sup := &Supervisor{
		sessions: map[string]*session.Actor{"bot|chat-1": {}},
		hub:      router,
		log:      log,
	}
	a := newSchedulerAdapter(scheduler.New(log), sup, log)
	scope := session.SchedulerScope{}
	a.deliver("bot|chat-1", scheduler.TimerID("t1"), scope)
	a.deliver("bot|chat-1", scheduler.TimerID("t1"), scope)

	if router.routed != 2 {
		t.Fatalf("timer ticks routed=%d, want 2", router.routed)
	}
	if router.lastAgent != "bot" || router.lastKey != "chat-1" {
		t.Fatalf("routed to %q|%q, want bot|chat-1", router.lastAgent, router.lastKey)
	}
	if router.lastMsg.Type != "timer" {
		t.Fatalf("message type %q, want timer", router.lastMsg.Type)
	}
	if router.lastMsg.ID == "" {
		t.Fatal("timer message has no id")
	}
	// The two fires of one timer must not share an id: the inbox deduplicates
	// on it, so a stable id would swallow every tick after the first.
	second := router.lastMsg.ID
	a.deliver("bot|chat-1", scheduler.TimerID("t1"), scope)
	if router.lastMsg.ID == second {
		t.Fatalf("consecutive timer fires reused id %q", second)
	}
}

// A tick that fires as its session exits must be dropped, not routed: the hub
// would hand it to the supervisor, which spawns the conversation again just to
// deliver a timer the dead session no longer has.
func TestTimerTickForExitedSessionIsDropped(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := &fakeRouter{}
	sup := &Supervisor{sessions: map[string]*session.Actor{}, hub: router, log: log}
	a := newSchedulerAdapter(scheduler.New(log), sup, log)
	a.deliver("bot|chat-1", scheduler.TimerID("t1"), session.SchedulerScope{})
	if router.routed != 0 {
		t.Fatalf("a tick for a session with no local actor was routed %d times", router.routed)
	}
}
