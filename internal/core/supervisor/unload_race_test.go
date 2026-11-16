package supervisor

import (
	"context"
	"sync"
	"testing"
	"time"

	"agentflow/internal/core/session"
)

func liveActor(sup *Supervisor, key string) *session.Actor {
	sup.mu.Lock()
	defer sup.mu.Unlock()
	return sup.sessions[key]
}

// TestStaleUnloadRequestIsIgnored: the sweep only asks; the actor decides. A
// request made about an activity stamp that is no longer current — a turn ran
// since the sweep looked — must not unload the session.
func TestStaleUnloadRequestIsIgnored(t *testing.T) {
	sup, drv := idleSupervisor(t, echoLoop, map[string]session.OpHandler{}, nil, nil)
	deliverAndWait(t, sup, drv, "chat", "hi")
	a := liveActor(sup, "echo|chat")
	if a == nil {
		t.Fatal("no session")
	}
	stale := a.LastActivity().Add(-time.Hour)
	a.RequestUnload(stale)
	time.Sleep(50 * time.Millisecond)
	if liveActor(sup, "echo|chat") != a {
		t.Fatal("a stale unload request unloaded the session")
	}
	if got := deliverAndWait(t, sup, drv, "chat", "still here"); got != "still here" {
		t.Fatalf("turn after a stale request = %q", got)
	}
}

// lateHub delivers one message to the session at the moment its lease is being
// released — after the actor has decided to unload, while it is still in the
// supervisor's map. That is the window in which a message used to be lost: it
// was written into the mailbox of an actor that would never read it.
type lateHub struct {
	sup  *Supervisor
	once sync.Once
	late session.Message

	mu       sync.Mutex
	released bool
}

func (h *lateHub) Route(_ context.Context, agent, key string, msg session.Message) error {
	return h.sup.DeliverLocal(agent, key, msg)
}
func (h *lateHub) Claim(context.Context, string) (bool, error) { return true, nil }
func (h *lateHub) Live(context.Context, string) (bool, error)  { return false, nil }
func (h *lateHub) ReleaseSession(_ context.Context, sessKey string) error {
	h.once.Do(func() {
		_ = h.sup.DeliverLocal("echo", "chat", h.late)
	})
	h.mu.Lock()
	h.released = true
	h.mu.Unlock()
	return nil
}

func TestMessageArrivingDuringUnloadIsServed(t *testing.T) {
	sup, drv := idleSupervisor(t, echoLoop, map[string]session.OpHandler{}, nil, nil)
	hub := &lateHub{sup: sup, late: session.Message{
		ID: "m-late", Type: "user", From: "user:test", Text: "late",
		Channel: "bench", ReplyTo: "chat",
	}}
	sup.SetHub(hub)
	deliverAndWait(t, sup, drv, "chat", "hi")
	first := liveActor(sup, "echo|chat")

	time.Sleep(40 * time.Millisecond)
	sup.SweepIdle()

	select {
	case got := <-drv.replies:
		if got != "late" {
			t.Fatalf("reply = %q; want the late message's echo", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a message that arrived while the session was unloading was never served")
	}
	second := liveActor(sup, "echo|chat")
	if second == nil || second == first {
		t.Fatal("the late message was not served by a fresh session")
	}
	hub.mu.Lock()
	released := hub.released
	hub.mu.Unlock()
	if !released {
		t.Fatal("the lease was not released")
	}
}

// TestUnloadReleasesTheLeaseBeforeTheSessionLeavesTheMap pins the order. If
// the session were removed first, a message arriving in between would spawn a
// fresh actor under the old lease, and the release would then take the lease
// away from a session that is running.
func TestUnloadReleasesTheLeaseBeforeTheSessionLeavesTheMap(t *testing.T) {
	sup, drv := idleSupervisor(t, echoLoop, map[string]session.OpHandler{}, nil, nil)
	var first *session.Actor
	var sameAtRelease bool
	hub := &orderHub{onRelease: func() {
		sameAtRelease = liveActor(sup, "echo|chat") == first
	}}
	sup.SetHub(hub)
	deliverAndWait(t, sup, drv, "chat", "hi")
	first = liveActor(sup, "echo|chat")
	time.Sleep(40 * time.Millisecond)
	sup.SweepIdle()
	waitNoSession(t, sup, "echo|chat")
	if !hub.called() {
		t.Fatal("the lease was not released")
	}
	if !sameAtRelease {
		t.Fatal("the session had already left the map when its lease was released")
	}
}

type orderHub struct {
	mu        sync.Mutex
	done      bool
	onRelease func()
}

func (h *orderHub) Route(context.Context, string, string, session.Message) error { return nil }
func (h *orderHub) Claim(context.Context, string) (bool, error)                  { return true, nil }
func (h *orderHub) Live(context.Context, string) (bool, error)                   { return false, nil }
func (h *orderHub) ReleaseSession(context.Context, string) error {
	h.onRelease()
	h.mu.Lock()
	h.done = true
	h.mu.Unlock()
	return nil
}
func (h *orderHub) called() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.done
}

// TestChildSessionIsNotUnloaded: what makes a session a child — its parent and
// the task it was spawned with — is not something the next message could
// recreate, so an idle child stays loaded.
func TestChildSessionIsNotUnloaded(t *testing.T) {
	sup, drv := idleSupervisor(t, echoLoop, map[string]session.OpHandler{}, nil, nil)
	deliverAndWait(t, sup, drv, "chat", "hi")
	a := liveActor(sup, "echo|chat")
	a.Identity.ParentID = "echo|parent" // as agent.spawn would have set it
	time.Sleep(40 * time.Millisecond)
	sup.SweepIdle()
	time.Sleep(40 * time.Millisecond)
	if liveActor(sup, "echo|chat") != a {
		t.Fatal("a spawned child was unloaded")
	}
}
