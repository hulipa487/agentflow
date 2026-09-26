// The pool stratum's boundary tests — §4.2 written as tests, not comments:
// reading through a pool must not widen the writer's scope, a grant is
// required and its mode is enforced, and the master/member boundary is
// arithmetic (the user: rule) rather than a check anyone can forget.
package memory_test

import (
	"testing"

	"agentflow/internal/core/memory"
)

// fakePools is the authorizer seam the tenancy registry implements.
type fakePools struct {
	grants map[string]string // "pool|agent|tenant" -> mode
}

func (f *fakePools) AuthorizePool(pool, agent, tenant string) (string, bool) {
	m, ok := f.grants[pool+"|"+agent+"|"+tenant]
	return m, ok
}

const (
	testPool  = "11111111-1111-1111-1111-111111111111"
	otherPool = "22222222-2222-2222-2222-222222222222"
)

// tryGet is mustGet for paths where the test expects the error: the denial
// itself is the assertion.
func tryGet(h memory.BackendHandle, key string) (string, bool, error) {
	v, ok, err := h.Get("t", key)
	s, _ := v.(string)
	return s, ok, err
}

func poolHandle(h memory.BackendHandle, mode memory.ScopeMode, agent string, auth memory.PoolAuthorizer, personal string) memory.BackendHandle {
	return memory.WrapScoped(h, "pool:"+testPool, mode,
		memory.Caller{ScopeUUID: personal, PersonalUUID: personal},
		memory.PoolScope{UUID: testPool, Agent: agent, Auth: auth})
}

func TestPoolRowIsNeverLegacy(t *testing.T) {
	h := openOne(t)
	// Another tenant's session wrote a row into a pool carve this test's
	// caller holds no grant on.
	mustPut(t, h, "pool:"+testPool+"|shared", "pool row")

	// A plain user-scoped handle on the same physical table — every filtered
	// read admits the legacy stratum, so the pool head must be recognized as
	// its own stratum or this read leaks the row.
	u := wrap(h, "user", memory.ModeInteractive, "u1")
	if _, ok := mustGet(t, u, "shared"); ok {
		t.Fatal("a pool row was readable as legacy — splitScopedKey lost the pool: head")
	}
}

func TestPoolAccessRequiresAGrant(t *testing.T) {
	h := openOne(t)
	auth := &fakePools{grants: map[string]string{
		testPool + "|writer|u1": "rw",
	}}

	// Granted caller: the full read/write cycle, landing under the pool's
	// scope prefix in the physical table.
	p := poolHandle(h, memory.ModeInteractive, "writer", auth, "u1")
	if err := p.Put("t", "note", "hello", memory.PutOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := mustGet(t, h, "pool:"+testPool+"|note"); !ok {
		t.Fatal("a pool write did not land in the pool stratum")
	}
	if v, ok := mustGet(t, p, "note"); !ok || v != "hello" {
		t.Fatalf("the writer cannot read back its own pool row: %q, %v", v, ok)
	}

	// No grant at all: every op denies, naming the rule.
	stranger := poolHandle(h, memory.ModeInteractive, "writer", auth, "u2")
	if err := stranger.Put("t", "note", "x", memory.PutOpts{}); err == nil {
		t.Fatal("an ungranted tenant wrote to the pool")
	}
	if _, _, err := tryGet(stranger, "note"); err == nil {
		t.Fatal("an ungranted tenant read from the pool")
	}
	if err := stranger.Delete("t", "note"); err == nil {
		t.Fatal("an ungranted tenant deleted from the pool")
	}
	if _, err := stranger.Query("t", memory.Query{Kind: "all"}); err == nil {
		t.Fatal("an ungranted tenant queried the pool")
	}
}

func TestPoolReadOnlyGrant(t *testing.T) {
	h := openOne(t)
	auth := &fakePools{grants: map[string]string{
		testPool + "|writer|u1": "r",
	}}
	p := poolHandle(h, memory.ModeInteractive, "writer", auth, "u1")
	if err := p.Put("t", "note", "x", memory.PutOpts{}); err == nil {
		t.Fatal("a read-only grant wrote")
	}
	if err := p.Delete("t", "nothing"); err == nil {
		t.Fatal("a read-only grant deleted")
	}
	mustPut(t, h, "pool:"+testPool+"|seeded", "by a r/w holder")
	if v, ok := mustGet(t, p, "seeded"); !ok || v != "by a r/w holder" {
		t.Fatalf("a read-only grant could not read: %q, %v", v, ok)
	}
}

// TestPoolDoesNotWidenIntoPersonalRows: the pool world and the personal world
// are disjoint. A member's grant on the pool does not make their personal
// rows visible through the pool binding, and pool rows do not surface in a
// personal binding — the two directions of §4.2 #2.
func TestPoolDoesNotWidenIntoPersonalRows(t *testing.T) {
	h := openOne(t)
	auth := &fakePools{grants: map[string]string{
		testPool + "|writer|u1": "rw",
	}}
	p := poolHandle(h, memory.ModeInteractive, "writer", auth, "u1")
	if err := p.Put("t", "shared-note", "from the pool", memory.PutOpts{}); err != nil {
		t.Fatal(err)
	}
	u := wrap(h, "user", memory.ModeInteractive, "u1")
	mustPut(t, u, "private-note", "personal")

	if _, ok := mustGet(t, u, "shared-note"); ok {
		t.Fatal("a personal binding saw a pool row")
	}
	if _, ok := mustGet(t, p, "private-note"); ok {
		t.Fatal("a pool binding saw a personal row — the read widened the writer's scope")
	}
}

// TestPoolScopingDoesNotReuseTheCallerScopeUUID: the pool handle must not key
// on the caller's scope uuid even in group context (where the scope uuid is
// the membership uuid) — the carve is the pool's stratum, always.
func TestPoolScopingDoesNotReuseTheCallerScopeUUID(t *testing.T) {
	h := openOne(t)
	auth := &fakePools{grants: map[string]string{
		testPool + "|writer|member-p": "rw",
	}}
	// Group context: the scope uuid is the membership uuid, the personal uuid
	// is what the grant keys on.
	p := memory.WrapScoped(h, "pool:"+testPool, memory.ModeInteractive,
		memory.Caller{ScopeUUID: "membership-uuid", PersonalUUID: "member-p"},
		memory.PoolScope{UUID: testPool, Agent: "writer", Auth: auth})
	if err := p.Put("t", "k", "v", memory.PutOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := mustGet(t, h, "pool:"+testPool+"|k"); !ok {
		t.Fatal("the write did not land in the pool stratum")
	}
	if _, ok := mustGet(t, h, "user:membership-uuid|k"); ok {
		t.Fatal("the write landed under the caller's scope uuid")
	}
}

// TestMaintenancePoolNamedScopesAreValidated: maintenance provenance writes
// raw, scope-explicit keys — and a pool head in such a key crosses a tenant
// boundary, so it is the one caller-named scope that is validated.
func TestMaintenancePoolNamedScopesAreValidated(t *testing.T) {
	h := openOne(t)
	auth := &fakePools{grants: map[string]string{
		testPool + "|writer|distiller": "rw",
	}}
	m := memory.WrapScoped(h, "user", memory.ModeMaintenance,
		memory.Caller{ScopeUUID: "distiller", PersonalUUID: "distiller"},
		memory.PoolScope{UUID: testPool, Agent: "writer", Auth: auth})

	// Writing into a pool the agent's carve covers: allowed.
	if err := m.Put("t", "pool:"+testPool+"|watermark", "w", memory.PutOpts{}); err != nil {
		t.Fatalf("maintenance write to a granted pool: %v", err)
	}
	// Writing into any other pool: denied — this is the check that keeps a
	// maintenance loop from publishing into an unrelated carve.
	if err := m.Put("t", "pool:"+otherPool+"|watermark", "w", memory.PutOpts{}); err == nil {
		t.Fatal("maintenance wrote into an ungranted pool")
	}
	if _, _, err := m.Get("t", "pool:"+otherPool+"|x"); err == nil {
		t.Fatal("maintenance read an ungranted pool")
	}
	// Non-pool scope-explicit writes are unchanged (the distiller's
	// per-user watermarks live in the agent's own store).
	if err := m.Put("t", "user:u9|deep", "ok", memory.PutOpts{}); err != nil {
		t.Fatalf("maintenance scope-explicit user write: %v", err)
	}
}

// TestNoRegistryMeansNoPoolAccess: a runtime wired without a pool registry
// denies pool access with a named error rather than failing open.
func TestNoRegistryMeansNoPoolAccess(t *testing.T) {
	h := openOne(t)
	p := poolHandle(h, memory.ModeInteractive, "writer", nil, "u1")
	if err := p.Put("t", "k", "v", memory.PutOpts{}); err == nil {
		t.Fatal("a pool write succeeded with no registry wired")
	}
}

// TestMasterCannotReachMemberPrivateRows: §4.2 #1, stated in the master/
// member vocabulary. The rule for user: scopes is that the caller's acting
// uuid must equal the scope uuid — there is no check to forget and no grant
// that widens it, which is what makes "a master must never reach a member's
// private store" arithmetic rather than policy.
func TestMasterCannotReachMemberPrivateRows(t *testing.T) {
	h := openOne(t)
	// The member's rows, in the member's own scope, on a shared physical
	// table the master's agent also uses.
	member := wrap(h, "user", memory.ModeInteractive, "member-uuid")
	mustPut(t, member, "diary", "member private")

	// The master's handle — its acting uuid is its own, whatever group
	// authority it holds.
	master := wrap(h, "user", memory.ModeInteractive, "master-uuid")
	if _, ok := mustGet(t, master, "diary"); ok {
		t.Fatal("THE MASTER REACHED A MEMBER'S PRIVATE ROW")
	}

	// Maintenance provenance does not widen it either: the distiller reading
	// every scope reads only within its OWN binding's table world, and the
	// member's rows are visible only to a handle whose primary is the
	// member's scope — which nothing but the member's own session resolves.
	maint := wrap(h, "user", memory.ModeMaintenance, "master-uuid")
	if _, ok := mustGet(t, maint, "diary"); ok {
		t.Fatal("a master's maintenance context reached a member's private row")
	}
}
