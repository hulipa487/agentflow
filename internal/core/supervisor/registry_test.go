package supervisor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"agentflow/internal/core/gateway"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/session"
)

// The runtime agent registry: agents are installed and removed while the
// runtime runs, so onboarding is a config write rather than a restart (§6.2).
// The grace semantics (E19) and the refuse-unless-force removal (E20) are the
// parts these tests pin.

func registrySupervisor(t *testing.T) *Supervisor {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sup := New(map[string]*AgentDef{}, gateway.NewRegistry(log), pool.New(1), nil, log)
	sup.Start(context.Background())
	return sup
}

func registryDef(name string) *AgentDef {
	return &AgentDef{
		Info:     &session.Info{Name: name, HistoryBudget: 100},
		Handlers: map[string]session.OpHandler{},
		LoopSrc:  directive() + "function loop() while true do session.inbox() end end",
	}
}

func waitForSessions(t *testing.T, sup *Supervisor, want int) []session.Identity {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		rows, _, _ := sup.Snapshot()
		if len(rows) == want {
			ids := make([]session.Identity, len(rows))
			for i, r := range rows {
				ids[i] = session.Identity{SessionID: r.SessionID, Agent: r.Agent}
			}
			return ids
		}
		select {
		case <-deadline:
			t.Fatalf("sessions = %d; want %d", len(rows), want)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestUpsertInstallsADeliverableAgent(t *testing.T) {
	sup := registrySupervisor(t)
	if err := sup.UpsertAgent("helper", registryDef("helper")); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := sup.DeliverLocal("helper", "webhook:c1", session.Message{ID: "m1", Type: "user", From: "u", Text: "hi"}); err != nil {
		t.Fatalf("deliver to upserted agent: %v", err)
	}
	waitForSessions(t, sup, 1)
}

func TestUpsertRefusesBrokenInput(t *testing.T) {
	sup := registrySupervisor(t)

	if err := sup.UpsertAgent("x", nil); err == nil {
		t.Fatal("nil definition accepted")
	}
	// A definition that names another agent would corrupt the def map's key
	// invariant (def.Info.Name drives agent.info and session identity).
	mislabeled := registryDef("other")
	if err := sup.UpsertAgent("x", mislabeled); err == nil {
		t.Fatal("definition naming another agent accepted")
	}
	// Spawn profiles are config-owned templates; an upsert must not shadow
	// them and create an undiscoverable twin.
	for _, name := range []string{"__spawn__worker", "spawn:worker"} {
		if err := sup.UpsertAgent(name, registryDef(name)); err == nil {
			t.Fatalf("spawn-namespace name %q accepted", name)
		}
		if _, err := sup.RemoveAgent(name, true); err == nil {
			t.Fatalf("spawn-namespace name %q removed", name)
		}
	}
}

func TestUpsertReplacesLiveSessions(t *testing.T) {
	sup := registrySupervisor(t)

	// Both loops probe their handlers: every message they take from the inbox
	// runs an op of type "probedef", which the handlers map of whichever def
	// the running loop was built from resolves. The restarted loop calling
	// the new handler is the observable of E19's grace semantics.
	probeLoop := directive() +
		"function loop() while true do session.inbox() af.op({type=\"probedef\"}) end end"
	old := &AgentDef{
		Info:     &session.Info{Name: "helper", HistoryBudget: 100},
		Handlers: map[string]session.OpHandler{},
		LoopSrc:  probeLoop,
	}
	var oldRuns, newRuns atomic.Int64
	old.Handlers["probedef"] = func(ctx context.Context, op session.Op) (string, bool) {
		oldRuns.Add(1)
		return `"ok"`, true
	}
	if err := sup.UpsertAgent("helper", old); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := sup.DeliverLocal("helper", "webhook:c1", session.Message{ID: "m1", Type: "user", From: "u", Text: "hi"}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	deadline := time.After(10 * time.Second)
	for oldRuns.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("the probe op never ran under the original def")
		case <-time.After(10 * time.Millisecond):
		}
	}

	replacement := &AgentDef{
		Info:     &session.Info{Name: "helper", HistoryBudget: 100},
		Handlers: map[string]session.OpHandler{},
		LoopSrc:  probeLoop,
	}
	replacement.Handlers["probedef"] = func(ctx context.Context, op session.Op) (string, bool) {
		newRuns.Add(1)
		return `"ok"`, true
	}
	if err := sup.UpsertAgent("helper", replacement); err != nil {
		t.Fatalf("replace: %v", err)
	}
	sup.mu.Lock()
	if sup.defs["helper"] != replacement {
		sup.mu.Unlock()
		t.Fatal("def map still holds the old definition")
	}
	sup.mu.Unlock()

	// Another message gives the restarted loop work to do; it must run the
	// probe under the replacement's handlers.
	if err := sup.DeliverLocal("helper", "webhook:c1", session.Message{ID: "m2", Type: "user", From: "u", Text: "hi again"}); err != nil {
		t.Fatalf("deliver after replace: %v", err)
	}
	for newRuns.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("the live session never restarted onto the replacement def")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestRemoveAgentRefusesWhileSessionsAreLive(t *testing.T) {
	sup := registrySupervisor(t)
	if err := sup.UpsertAgent("helper", registryDef("helper")); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := sup.DeliverLocal("helper", "webhook:c1", session.Message{ID: "m1", Type: "user", From: "u", Text: "hi"}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	waitForSessions(t, sup, 1)

	stopped, err := sup.RemoveAgent("helper", false)
	var live *LiveSessionsError
	if !errors.As(err, &live) {
		t.Fatalf("removal with a live session = %v; want LiveSessionsError", err)
	}
	if live.Sessions != 1 || stopped != 0 {
		t.Fatalf("refusal = stopped %d, sessions %d; want 0 and 1", stopped, live.Sessions)
	}
	if _, ok := sup.Agents()["helper"]; !ok {
		t.Fatal("refused removal still removed the definition")
	}

	stopped, err = sup.RemoveAgent("helper", true)
	if err != nil {
		t.Fatalf("forced removal: %v", err)
	}
	if stopped != 1 {
		t.Fatalf("forced removal stopped %d sessions; want 1", stopped)
	}
	waitForSessions(t, sup, 0)

	// The agent is gone for delivery too — a routed message after removal is
	// an unknown agent, not a silently respawned session.
	if err := sup.DeliverLocal("helper", "webhook:c1", session.Message{ID: "m2", Type: "user", From: "u", Text: "hi"}); err == nil {
		t.Fatal("delivery to a removed agent succeeded")
	}
}

func TestRemoveUnknownAgent(t *testing.T) {
	sup := registrySupervisor(t)
	if _, err := sup.RemoveAgent("ghost", false); !isUnknownAgent(err) {
		t.Fatalf("removing an unknown agent = %v; want UnknownAgentError", err)
	}
}

func isUnknownAgent(err error) bool {
	var unknown *UnknownAgentError
	return errors.As(err, &unknown)
}
