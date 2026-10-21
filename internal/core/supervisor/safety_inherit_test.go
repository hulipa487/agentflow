package supervisor

import (
	"context"
	"testing"

	"agentflow/internal/core/safety"
	"agentflow/internal/core/session"
)

func spawnSafety(t *testing.T, sup *Supervisor, tmpl *SpawnTemplate, parent session.Identity) *safety.Dispatcher {
	t.Helper()
	sup.templates["child"] = tmpl
	res, err := sup.Spawn(context.Background(), parent, "child", nil)
	if err != nil {
		t.Fatal(err)
	}
	sup.mu.Lock()
	defer sup.mu.Unlock()
	return sup.safetyBySession[res.SessionID]
}

func TestSpawnInheritsParentSafety(t *testing.T) {
	sup := newTestSupervisor(t)
	parentChain := safety.New(&safety.Profile{Name: "parent"})
	parent := session.Identity{
		SessionID:    "planner|x",
		Agent:        "planner",
		Capabilities: map[string]bool{"agent.spawn": true, "llm.chat": true},
	}
	sup.mu.Lock()
	sup.safetyBySession[parent.SessionID] = parentChain
	sup.mu.Unlock()

	got := spawnSafety(t, sup, &SpawnTemplate{
		Name:          "child",
		LoopSrc:       directive() + "function loop() while true do session.inbox() end end",
		Capabilities:  map[string]bool{"llm.chat": true},
		Handlers:      map[string]session.OpHandler{},
		Memory:        &session.Info{Name: "spawn:child"},
		Safety:        safety.New(safety.None),
		InheritSafety: true,
	}, parent)
	if got != parentChain {
		t.Fatalf("child inherited %p; want parent chain %p", got, parentChain)
	}
}

func TestSpawnExplicitSafetyWins(t *testing.T) {
	sup := newTestSupervisor(t)
	parentChain := safety.New(&safety.Profile{Name: "parent"})
	childChain := safety.New(&safety.Profile{Name: "child"})
	parent := session.Identity{
		SessionID:    "planner|x",
		Agent:        "planner",
		Capabilities: map[string]bool{"agent.spawn": true, "llm.chat": true},
	}
	sup.mu.Lock()
	sup.safetyBySession[parent.SessionID] = parentChain
	sup.mu.Unlock()

	got := spawnSafety(t, sup, &SpawnTemplate{
		Name:         "child",
		LoopSrc:      directive() + "function loop() while true do session.inbox() end end",
		Capabilities: map[string]bool{"llm.chat": true},
		Handlers:     map[string]session.OpHandler{},
		Memory:       &session.Info{Name: "spawn:child"},
		Safety:       childChain,
	}, parent)
	if got != childChain {
		t.Fatalf("child safety = %p; want explicit chain %p", got, childChain)
	}
}
