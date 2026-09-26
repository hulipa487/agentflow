package supervisor

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"agentflow/internal/core/caps"
	"agentflow/internal/core/gateway"
	"agentflow/internal/core/memory"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/volatile"
)

// This file is the security boundary for tenant-scope propagation (§6.1). It
// exercises a real delegation chain — planner → worker, with real actors and
// the real store handlers — because the failure it guards against is a message
// *path* losing the tenant, and no unit of the path can show that alone.
//
// The acceptance test is TestForgedPayloadUUIDCannotReachAnotherTenant: a loop
// that names another tenant's uuid in an agent.send payload must not reach that
// tenant's scope. Everything else here pins the rest of the contract: hops
// inherit, inherit_user: false stops it, and a turn that never had a tenant
// stays tenant-less rather than inventing one.

const (
	tenantA = "u_tenant_a"
	tenantB = "u_tenant_b" // the uuid a hostile loop tries to claim
)

// probeLoop is the loop the worker agent runs: it receives one message, writes
// through the real store path, and reports the scope the engine resolved for
// the turn. The write goes through the same handler map the engine uses, so the
// assertion is made on the stored key stratum rather than on a helper's opinion
// of it.
var probeLoop = directive() + `
function loop()
  while true do
    local _ = session.inbox()
    store.put("t", "wrote", "v")
    af.op({ type = "probe.done" })
  end
end
`

// delegatingLoop is an ordinary agent loop that delegates to another agent. It
// forges a foreign tenant into the payload when forge is set — exactly what a
// hostile or buggy proprietary loop would do. The chain under test is two hops
// (planner → middle → worker), the shape the production bug took: a pm → worker
// swarm, where every hop is a chance to lose the tenant.
func delegatingLoop(target string, forge bool) string {
	payload := `{ note = "task" }`
	if forge {
		payload = `{ user_uuid = "` + tenantB + `", note = "task" }`
	}
	return directive() + `
function loop()
  while true do
    local _ = session.inbox()
    agent.send("agent:` + target + `", ` + payload + `)
  end
end
`
}

// parkedLoop never touches the mailbox, so a test can inspect a delivered
// message itself instead of racing the actor for it.
var parkedLoop = directive() + `function loop() time.sleep(3600) end`

type tenantFixture struct {
	sup      *Supervisor
	raw      memory.BackendHandle // unwrapped: the test reads key strata directly
	workerCh chan string          // the scope uuid each worker turn resolved
	cancel   context.CancelFunc
}

// newTenantFixture builds a supervisor with a two-hop chain (planner → middle →
// worker) over one volatile store, wired the way main wires them: StoreHandlers
// is the same handler map the engine gates and hands to an agent. forge makes
// both delegating hops name a foreign tenant in the payload.
func newTenantFixture(t *testing.T, workerInfo *session.Info, forge bool) *tenantFixture {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	reg := memory.NewRegistry()
	reg.RegisterProvider(volatile.Provider{})
	reg.AddBackend("v", "volatile", nil)
	if err := reg.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr := memory.NewManager(reg, log)
	raw, ok := reg.Handle("v")
	if !ok {
		t.Fatal("volatile backend did not open")
	}
	am := memory.AgentMemory{
		Stores: map[string]memory.StoreBinding{"t": {Backend: "v", Table: "t", Scoping: "user"}},
		Tables: map[string]memory.StoreBinding{"t": {Backend: "v", Table: "t", Scoping: "user"}},
	}

	f := &tenantFixture{raw: raw, workerCh: make(chan string, 16)}
	handlers := caps.StoreHandlers(&am, mgr, nil)
	handlers["probe.done"] = func(ctx context.Context, op session.Op) (string, bool) {
		select {
		case f.workerCh <- session.ScopeUUIDFromCtx(ctx):
		default:
		}
		return "true", true
	}

	if workerInfo == nil {
		workerInfo = &session.Info{Name: "worker", HistoryBudget: 100}
	}
	workerInfo.Name = "worker"
	defs := map[string]*AgentDef{
		"planner": {
			Info:         &session.Info{Name: "planner", HistoryBudget: 100},
			CanContact:   map[string]bool{"middle": true},
			Capabilities: map[string]bool{"agent.send": true},
			Handlers:     map[string]session.OpHandler{},
			LoopSrc:      delegatingLoop("middle", forge),
		},
		"middle": {
			Info:         &session.Info{Name: "middle", HistoryBudget: 100},
			CanContact:   map[string]bool{"worker": true},
			Capabilities: map[string]bool{"agent.send": true},
			Handlers:     map[string]session.OpHandler{},
			LoopSrc:      delegatingLoop("worker", forge),
		},
		"worker": {
			Info:         workerInfo,
			CanContact:   map[string]bool{"planner": true},
			Capabilities: map[string]bool{"agent.send": true},
			Handlers:     handlers,
			LoopSrc:      probeLoop,
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	t.Cleanup(cancel)

	f.sup = New(defs, gateway.NewRegistry(log), pool.New(2), nil, log)
	f.sup.Start(ctx)
	return f
}

// deliverAs delivers a channel-originated inbound message to the planner as a
// given tenant. userUUID is the three-state stamp: nil = unknown (nothing
// stamped the message), "" = known to have no tenant.
func (f *tenantFixture) deliverAs(t *testing.T, userUUID string, stamped bool, from string) {
	t.Helper()
	prov := session.Provenance{Kind: "channel", Principal: "user:x"}
	if stamped {
		u := userUUID
		prov.UserUUID = &u
	}
	if err := f.sup.DeliverLocal("planner", "chat:1", session.Message{
		ID: "m1", Type: "user", From: from, Text: "go",
		Channel: "webhook", ReplyTo: "r1", Provenance: &prov,
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
}

func (f *tenantFixture) awaitProbe(t *testing.T) string {
	t.Helper()
	select {
	case got := <-f.workerCh:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("worker never reported")
		return ""
	}
}

// storedAt reports whether one raw key — scope prefix included — was written,
// straight out of the backend, so a test asserts the stratum that was actually
// written rather than a re-derivation of it.
func (f *tenantFixture) storedAt(t *testing.T, rawKey string) bool {
	t.Helper()
	_, found, err := f.raw.Get("t", rawKey)
	if err != nil {
		t.Fatalf("raw get %q: %v", rawKey, err)
	}
	return found
}

// TestForgedPayloadUUIDCannotReachAnotherTenant is the acceptance test for
// tenant-scope propagation (§6.1) and for §4.2 #4 — a loop can never name a
// scope it was not given.
//
// A turn belonging to tenant A delegates two hops to the worker, with
// user_uuid=tenantB in the payload at each hop. The payload is rebuilt from the
// caller's Lua table at every hop, so it is loop-controlled: if anything
// resolves identity from it, the worker writes into tenant B's scope.
// Provenance is the authority, so it writes into tenant A's.
func TestForgedPayloadUUIDCannotReachAnotherTenant(t *testing.T) {
	f := newTenantFixture(t, nil, true)
	f.deliverAs(t, tenantA, true, "user:"+tenantA)

	if got := f.awaitProbe(t); got != tenantA {
		t.Fatalf("worker acted as %q; a forged payload uuid must not decide the scope (want %q)", got, tenantA)
	}
	if !f.storedAt(t, "user:"+tenantA+"|wrote") {
		t.Fatal("the delegated write must land in the delegating tenant's scope")
	}
	if f.storedAt(t, "user:"+tenantB+"|wrote") {
		t.Fatal("the payload's user_uuid reached tenant B's scope: the payload is an authority again")
	}
	if f.storedAt(t, "service|wrote") {
		t.Fatal("a delegated write must not fall back to the fleet-wide service stratum")
	}
}

// TestDelegationHopInheritsTenant is the positive half: without any forgery, a
// two-hop chain that started as tenant A writes as tenant A at the end — the
// §6.1 fix. The observed production bug was a pm→worker swarm building
// per-user trees as one fleet-wide scope.
func TestDelegationHopInheritsTenant(t *testing.T) {
	f := newTenantFixture(t, nil, false)
	f.deliverAs(t, tenantA, true, "user:"+tenantA)

	if got := f.awaitProbe(t); got != tenantA {
		t.Fatalf("hop lost the tenant: worker scope %q, want %q", got, tenantA)
	}
	if f.storedAt(t, "service|wrote") || f.storedAt(t, "agent|wrote") {
		t.Fatal("an inherited tenant must not write in the service or agent stratum")
	}
}

// TestInheritUserFalseStopsInheritance: the per-agent opt-out. The worker runs
// tenant-less — service stratum, the pre-fix behaviour, deliberately preserved
// for agents that must never see a delegating tenant.
func TestInheritUserFalseStopsInheritance(t *testing.T) {
	optOut := false
	f := newTenantFixture(t, &session.Info{InheritUser: &optOut}, false)
	f.deliverAs(t, tenantA, true, "user:"+tenantA)

	if got := f.awaitProbe(t); got != "" {
		t.Fatalf("inherit_user: false must leave the worker tenant-less, got scope %q", got)
	}
	if f.storedAt(t, "user:"+tenantA+"|wrote") {
		t.Fatal("an opted-out agent must not write in the delegating tenant's scope")
	}
	if !f.storedAt(t, "service|wrote") {
		t.Fatal("an opted-out agent must write in the service stratum")
	}
}

// TestUnlinkedHandleHopHasNoTenant pins the three-state field's empty state: a
// handle known to belong to nobody stamps an explicit "no tenant", and the hop
// must not fall back to the sender address — which for an unregistered handle
// is its stable identity id, a value that must never become a scope uuid.
func TestUnlinkedHandleHopHasNoTenant(t *testing.T) {
	f := newTenantFixture(t, nil, false)
	f.deliverAs(t, "", true, "user:i_abc123")

	if got := f.awaitProbe(t); got != "" {
		t.Fatalf("an unlinked handle must act tenant-less, got scope %q", got)
	}
	if !f.storedAt(t, "service|wrote") {
		t.Fatal("an unlinked handle must write in the service stratum")
	}
	if f.storedAt(t, "user:i_abc123|wrote") {
		t.Fatal("the handle's identity id became a scope uuid")
	}
}

// TestUnknownProvenanceCarriesNoTenant: a message nobody stamped stays
// tenant-less. The distinction matters at the far end of a chain, so this pins
// that the unknown state resolves to no scope at all.
func TestUnknownProvenanceCarriesNoTenant(t *testing.T) {
	f := newTenantFixture(t, nil, false)
	f.deliverAs(t, "", false, "")

	if got := f.awaitProbe(t); got != "" {
		t.Fatalf("an unstamped message must leave the worker tenant-less, got %q", got)
	}
	if !f.storedAt(t, "service|wrote") {
		t.Fatal("an unstamped turn must write in the service stratum")
	}
}

// TestSpawnedChildCarriesInheritPolicy: a spawned child is reached by the same
// hop path as a static agent, so the one thing spawn can break is the policy
// itself — the template's Info is cloned per child, and the opt-out has to ride
// that clone (and the "spawn:<profile>" def a parent later addresses).
func TestSpawnedChildCarriesInheritPolicy(t *testing.T) {
	sup := newTestSupervisor(t)
	optOut := false
	sup.templates["coder"] = &SpawnTemplate{
		Name:         "coder",
		LoopSrc:      parkedLoop,
		Capabilities: map[string]bool{"llm.chat": true},
		Handlers:     map[string]session.OpHandler{},
		Memory:       &session.Info{Name: "spawn:coder", InheritUser: &optOut},
	}
	parent := session.Identity{
		SessionID:    "planner|x",
		Agent:        "planner",
		Capabilities: map[string]bool{"agent.spawn": true, "llm.chat": true},
	}
	res, err := sup.Spawn(context.Background(), parent, "coder", nil)
	if err != nil {
		t.Fatal(err)
	}
	sup.mu.Lock()
	child := sup.sessions[res.SessionID]
	sup.mu.Unlock()
	if child == nil {
		t.Fatal("child not registered")
	}
	if child.Info.InheritsUser() {
		t.Fatal("a spawn profile's inherit_user: false must reach the child")
	}

	// The default carries the other way: a template that says nothing inherits.
	sup.templates["plain"] = &SpawnTemplate{
		Name:         "plain",
		LoopSrc:      parkedLoop,
		Capabilities: map[string]bool{"llm.chat": true},
		Handlers:     map[string]session.OpHandler{},
		Memory:       &session.Info{Name: "spawn:plain"},
	}
	res, err = sup.Spawn(context.Background(), parent, "plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	sup.mu.Lock()
	child = sup.sessions[res.SessionID]
	sup.mu.Unlock()
	if child == nil || !child.Info.InheritsUser() {
		t.Fatal("a template that does not set inherit_user must inherit")
	}
}

// parkedSupervisor is a supervisor whose agents never read their mailboxes, so
// a test can inspect what a hop delivered instead of racing the actor for it.
func parkedSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	defs := map[string]*AgentDef{
		"planner": {
			Info:         &session.Info{Name: "planner", HistoryBudget: 100},
			CanContact:   map[string]bool{"worker": true},
			Capabilities: map[string]bool{"agent.send": true, "agent.request": true},
			Handlers:     map[string]session.OpHandler{},
			LoopSrc:      parkedLoop,
		},
		"worker": {
			Info:     &session.Info{Name: "worker", HistoryBudget: 100},
			Handlers: map[string]session.OpHandler{},
			LoopSrc:  parkedLoop,
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sup := New(defs, gateway.NewRegistry(log), pool.New(2), nil, log)
	sup.Start(ctx)
	return sup
}

// TestHopStampIsTheThreeStateField drives the supervisor's own stamping: the
// message a hop delivers carries the tenant on its provenance, taken from the
// sending turn's core-stamped identity and never from the payload — and all
// three states survive the hop intact.
func TestHopStampIsTheThreeStateField(t *testing.T) {
	sup := parkedSupervisor(t)
	source := session.Identity{
		SessionID:    "planner|x",
		Agent:        "planner",
		CanContact:   map[string]bool{"worker": true},
		Capabilities: map[string]bool{"agent.send": true, "agent.request": true},
	}
	const skey = "worker|agent:planner|x"

	// A personal-context turn of tenant A, whose Lua forges tenant B.
	ctx := session.WithPersonalIdentity(context.Background(), tenantA)
	if err := sup.Send(ctx, source, "agent:worker", map[string]any{"user_uuid": tenantB}); err != nil {
		t.Fatal(err)
	}
	msg := drainMailbox(t, sup, skey)
	if msg.Provenance == nil || msg.Provenance.UserUUID == nil {
		t.Fatal("the hop must carry the tenant on its provenance")
	}
	if *msg.Provenance.UserUUID != tenantA {
		t.Fatalf("hop tenant = %q, want %q", *msg.Provenance.UserUUID, tenantA)
	}
	if msg.Payload["user_uuid"] != tenantB {
		t.Fatal("the payload should still carry what Lua sent — it is simply not read")
	}

	// A turn known to have no tenant stamps an explicit empty, so the receiver
	// cannot fall back to the sender address either.
	if err := sup.Send(session.WithActingIdentity(context.Background(), session.ActingIdentity{}), source, "agent:worker", nil); err != nil {
		t.Fatal(err)
	}
	msg = drainMailbox(t, sup, skey)
	if msg.Provenance == nil || msg.Provenance.UserUUID == nil || *msg.Provenance.UserUUID != "" {
		t.Fatalf("a tenant-less turn must stamp an explicit empty, got %+v", msg.Provenance)
	}

	// An unstamped turn leaves the field nil: unknown, not empty.
	if err := sup.Send(context.Background(), source, "agent:worker", nil); err != nil {
		t.Fatal(err)
	}
	msg = drainMailbox(t, sup, skey)
	if msg.Provenance == nil || msg.Provenance.UserUUID != nil {
		t.Fatalf("an unstamped turn must leave the tenant unknown, got %+v", msg.Provenance)
	}
}

// TestReplyCarriesTenant: a reply resolves back to the requester, and it must
// carry the tenant of the turn that answered — so a delegated conversation
// stays on that tenant's scope in both directions.
func TestReplyCarriesTenant(t *testing.T) {
	sup := parkedSupervisor(t)
	replier := session.Identity{
		SessionID:    "worker|agent:planner|x",
		Agent:        "worker",
		Capabilities: map[string]bool{"agent.reply": true},
	}
	requestID, wait, cancel := sup.requests.Open("planner|x", "worker")
	defer cancel()

	ctx := session.WithPersonalIdentity(context.Background(), tenantA)
	if err := sup.Reply(ctx, replier, requestID, map[string]any{"done": true}); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-wait:
		if msg.Provenance == nil || msg.Provenance.UserUUID == nil || *msg.Provenance.UserUUID != tenantA {
			t.Fatalf("reply must carry the answering turn's tenant, got %+v", msg.Provenance)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reply never resolved the request")
	}
}

// drainMailbox takes the message a delivery queued for the named session.
func drainMailbox(t *testing.T, sup *Supervisor, skey string) session.Message {
	t.Helper()
	sup.mu.Lock()
	a := sup.sessions[skey]
	sup.mu.Unlock()
	if a == nil {
		t.Fatalf("session %q not spawned", skey)
	}
	select {
	case m := <-a.Mailbox:
		return m
	case <-time.After(2 * time.Second):
		t.Fatalf("session %q received nothing", skey)
		return session.Message{}
	}
}
