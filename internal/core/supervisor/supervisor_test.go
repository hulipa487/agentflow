package supervisor

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"agentflow/internal/core/gateway"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/session"
)

// This file tests the multi-agent authority layer: ACL enforcement, request
// correlation, spawn attenuation, and lifecycle cleanup. It uses lightweight
// actors with a no-op loop so no real LLM/VM is required for the core paths.

func newTestSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := gateway.NewRegistry(log)
	p := pool.New(2)
	defs := map[string]*AgentDef{
		"planner": {
			Info:     &session.Info{Name: "planner", HistoryBudget: 100},
			CanContact: map[string]bool{"worker": true},
			Capabilities: map[string]bool{"llm.chat": true, "agent.send": true, "agent.request": true, "agent.spawn": true},
			Handlers: map[string]session.OpHandler{},
			LoopSrc:  "function loop() while true do session.inbox() end end",
		},
		"worker": {
			Info:     &session.Info{Name: "worker", HistoryBudget: 100},
			CanContact: map[string]bool{"planner": true},
			Capabilities: map[string]bool{"llm.chat": true, "agent.reply": true},
			Handlers: map[string]session.OpHandler{},
			LoopSrc:  "function loop() while true do session.inbox() end end",
		},
	}
	sup := New(defs, gw, p, nil, log)
	sup.Start(context.Background())
	return sup
}

func TestSendEnforcesACL(t *testing.T) {
	sup := newTestSupervisor(t)
	parent := session.Identity{
		SessionID:    "planner|x",
		Agent:        "planner",
		CanContact:   map[string]bool{"worker": true},
		Capabilities: map[string]bool{"agent.send": true},
	}
	if err := sup.Send(context.Background(), parent, "agent:worker", map[string]any{"hi": 1}); err != nil {
		t.Fatalf("expected send to worker to succeed, got %v", err)
	}
	// planner has no contact entry for "intruder"
	if err := sup.Send(context.Background(), parent, "agent:intruder", nil); err == nil {
		t.Fatal("expected send to intruder to fail")
	}
}

func TestSendRequiresCapability(t *testing.T) {
	sup := newTestSupervisor(t)
	source := session.Identity{
		SessionID:    "worker|x",
		Agent:        "worker",
		CanContact:   map[string]bool{"planner": true},
		Capabilities: map[string]bool{}, // no agent.send
	}
	if err := sup.Send(context.Background(), source, "agent:planner", nil); err == nil {
		t.Fatal("expected send without capability to fail")
	}
}

func TestRequestTimeout(t *testing.T) {
	sup := newTestSupervisor(t)
	parent := session.Identity{
		SessionID:    "planner|x",
		Agent:        "planner",
		CanContact:   map[string]bool{"worker": true},
		Capabilities: map[string]bool{"agent.request": true},
	}
	_, err := sup.Request(context.Background(), parent, "agent:worker", nil, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestSpawnAttenuation(t *testing.T) {
	sup := newTestSupervisor(t)
	sup.templates["coder"] = &SpawnTemplate{
		Name:         "coder",
		LoopSrc:      "function loop() while true do session.inbox() end end",
		Capabilities: map[string]bool{"llm.chat": true, "agent.send": true, "shell.exec": true},
		CanContact:   map[string]bool{"worker": true},
		Handlers:     map[string]session.OpHandler{},
		Memory:       &session.Info{Name: "coder"},
	}
	parent := session.Identity{
		SessionID:    "planner|x",
		Agent:        "planner",
		CanContact:   map[string]bool{"worker": true},
		Capabilities: map[string]bool{"agent.spawn": true, "llm.chat": true, "agent.send": true},
	}
	res, err := sup.Spawn(context.Background(), parent, "coder", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Agent != "spawn:coder" {
		t.Fatalf("expected agent name spawn:coder, got %q", res.Agent)
	}

	// The child's caps should be parent ∩ template = {llm.chat, agent.send}; shell.exec dropped.
	sup.mu.Lock()
	child := sup.sessions[res.SessionID]
	sup.mu.Unlock()
	if child == nil {
		t.Fatal("child not registered")
	}
	if child.Identity.Capabilities["shell.exec"] {
		t.Fatal("child should not retain shell.exec after attenuation")
	}
	if !child.Identity.Capabilities["agent.send"] {
		t.Fatal("child should retain agent.send")
	}
	// Parent should now be able to contact the child.
	if !child.Identity.CanContact["planner"] {
		t.Fatal("child should have implicit parent contact")
	}
}

func TestSpawnRequiresCapability(t *testing.T) {
	sup := newTestSupervisor(t)
	sup.templates["coder"] = &SpawnTemplate{
		Name:         "coder",
		LoopSrc:      "function loop() end",
		Capabilities: map[string]bool{"llm.chat": true},
		Handlers:     map[string]session.OpHandler{},
	}
	parent := session.Identity{
		SessionID:    "planner|x",
		Agent:        "planner",
		Capabilities: map[string]bool{}, // no agent.spawn
	}
	if _, err := sup.Spawn(context.Background(), parent, "coder", nil); err == nil {
		t.Fatal("expected spawn without capability to fail")
	}
}

// TestSendToSpawnedChildSession verifies that a parent can deliver to a
// freshly spawned child by its session:<id> address. This is the path the
// orchestrator's PM uses to receive its brief after agent.spawn returns.
func TestSendToSpawnedChildSession(t *testing.T) {
	sup := newTestSupervisor(t)
	sup.templates["coder"] = &SpawnTemplate{
		Name:         "coder",
		LoopSrc:      "function loop() while true do session.inbox() end end",
		Capabilities: map[string]bool{"llm.chat": true, "agent.send": true},
		Handlers:     map[string]session.OpHandler{},
		Memory:       &session.Info{Name: "coder"},
	}
	// Register the spawn template under the child agent name too, so Deliver
	// can route to an existing spawned child by agent name (see main.go wiring).
	sup.defs["spawn:coder"] = &AgentDef{
		Info:         &session.Info{Name: "spawn:coder"},
		CanContact:   map[string]bool{"planner": true},
		Capabilities: map[string]bool{"llm.chat": true, "agent.send": true},
		Handlers:     map[string]session.OpHandler{},
		LoopSrc:      "function loop() while true do session.inbox() end end",
	}
	parent := session.Identity{
		SessionID:    "planner|x",
		Agent:        "planner",
		CanContact:   map[string]bool{"worker": true},
		Capabilities: map[string]bool{"agent.spawn": true, "agent.send": true, "llm.chat": true},
	}
	res, err := sup.Spawn(context.Background(), parent, "coder", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Send to the child by its session address — this must reach the child.
	if err := sup.Send(context.Background(), parent, res.Address, map[string]any{"type": "brief"}); err != nil {
		t.Fatalf("send to child session %q failed: %v", res.Address, err)
	}
	// The child's mailbox should now hold the brief.
	sup.mu.Lock()
	child := sup.sessions[res.SessionID]
	sup.mu.Unlock()
	if child == nil {
		t.Fatal("child session gone")
	}
	select {
	case m := <-child.Mailbox:
		if m.Payload == nil || m.Payload["type"] != "brief" {
			t.Fatalf("child got wrong message: %+v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("child never received the brief")
	}
}

// Send to a session address that has no live session yet must find-or-create
// it as a routed session of the named static agent (keyed by the id's key
// segment), gated by the sender's can_contact. This is what lets a chat agent
// kick off a not-yet-running project-manager session.
func TestSendFindOrCreatesRoutedSession(t *testing.T) {
	sup := newTestSupervisor(t)
	sup.defs["pm"] = &AgentDef{
		Info:         &session.Info{Name: "pm", HistoryBudget: 100},
		CanContact:   map[string]bool{"planner": true},
		Capabilities: map[string]bool{"llm.chat": true},
		Handlers:     map[string]session.OpHandler{},
		LoopSrc:      "function loop() while true do session.inbox() end end",
	}
	parent := session.Identity{
		SessionID:    "planner|x",
		Agent:        "planner",
		CanContact:   map[string]bool{"pm": true},
		Capabilities: map[string]bool{"agent.send": true, "llm.chat": true},
	}
	if err := sup.Send(context.Background(), parent, "session:pm|proj:acme-web", map[string]any{"type": "brief"}); err != nil {
		t.Fatalf("send to unknown pm session failed: %v", err)
	}
	sup.mu.Lock()
	created := sup.sessions["pm|proj:acme-web"]
	sup.mu.Unlock()
	if created == nil {
		t.Fatal("session was not find-or-created")
	}
	select {
	case m := <-created.Mailbox:
		if m.Payload == nil || m.Payload["type"] != "brief" {
			t.Fatalf("created session got wrong message: %+v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("created session never received the brief")
	}
}

// Find-or-create must still enforce can_contact: a sender may not create a
// session of an agent it is not authorized to reach.
func TestSendFindOrCreateEnforcesACL(t *testing.T) {
	sup := newTestSupervisor(t)
	sup.defs["pm"] = &AgentDef{
		Info:         &session.Info{Name: "pm", HistoryBudget: 100},
		Capabilities: map[string]bool{"llm.chat": true},
		Handlers:     map[string]session.OpHandler{},
		LoopSrc:      "function loop() while true do session.inbox() end end",
	}
	parent := session.Identity{
		SessionID:    "planner|x",
		Agent:        "planner",
		CanContact:   map[string]bool{"worker": true}, // not "pm"
		Capabilities: map[string]bool{"agent.send": true, "llm.chat": true},
	}
	if err := sup.Send(context.Background(), parent, "session:pm|proj:acme-web", nil); err == nil {
		t.Fatal("send to unauthorized agent session should have been rejected")
	}
	sup.mu.Lock()
	_, created := sup.sessions["pm|proj:acme-web"]
	sup.mu.Unlock()
	if created {
		t.Fatal("session must not be created for an unauthorized sender")
	}
}

// A session id whose agent segment names no defined agent cannot be created.
func TestSendFindOrCreateRejectsUnknownAgent(t *testing.T) {
	sup := newTestSupervisor(t)
	parent := session.Identity{
		SessionID:    "planner|x",
		Agent:        "planner",
		CanContact:   map[string]bool{"ghost": true},
		Capabilities: map[string]bool{"agent.send": true, "llm.chat": true},
	}
	if err := sup.Send(context.Background(), parent, "session:ghost|proj:x", nil); err == nil {
		t.Fatal("send to undefined agent session should have been rejected")
	}
}

// A send to a session that ran and exited must fail, not find-or-create a
// blank replacement. Resurrecting a dead session would silently drop the
// in-flight conversation and can spawn loops of re-created actors.
func TestSendToExitedSessionFails(t *testing.T) {
	sup := newTestSupervisor(t)
	sup.defs["pm"] = &AgentDef{
		Info:         &session.Info{Name: "pm", HistoryBudget: 100},
		Capabilities: map[string]bool{"llm.chat": true},
		Handlers:     map[string]session.OpHandler{},
		// A loop that returns immediately so the session exits right away.
		LoopSrc: "function loop() end",
	}
	parent := session.Identity{
		SessionID:    "planner|x",
		Agent:        "planner",
		CanContact:   map[string]bool{"pm": true},
		Capabilities: map[string]bool{"agent.send": true, "llm.chat": true},
	}
	// First send creates the never-before-seen session.
	if err := sup.Send(context.Background(), parent, "session:pm|proj:y", map[string]any{"n": 1}); err != nil {
		t.Fatalf("initial find-or-create failed: %v", err)
	}
	// The session exits (its loop returns). Give onActorExit a moment to retire it,
	// then ensure termination with StopSession in case the loop is still parked.
	deadline := time.Now().Add(2 * time.Second)
	for {
		sup.StopSession("pm|proj:y")
		sup.mu.Lock()
		_, live := sup.sessions["pm|proj:y"]
		_, retired := sup.retired["pm|proj:y"]
		sup.mu.Unlock()
		if !live && retired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session did not retire (live=%v retired=%v)", live, retired)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A subsequent send must fail, not resurrect.
	if err := sup.Send(context.Background(), parent, "session:pm|proj:y", map[string]any{"n": 2}); err == nil {
		t.Fatal("send to exited session should have failed, not resurrected it")
	}
	sup.mu.Lock()
	_, recreated := sup.sessions["pm|proj:y"]
	sup.mu.Unlock()
	if recreated {
		t.Fatal("exited session must not be resurrected")
	}
}

