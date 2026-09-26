package tenancy

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"agentflow/internal/core/runtime"
)

// Tenancy is the mechanism behind the group and pool decisions; these tests
// pin the mechanism's guarantees: the derived membership uuid, exactly-one
// owner, the grant lookup order, and the disband sweep.

func openRegistry(t *testing.T) *Registry {
	t.Helper()
	rows, err := runtime.OpenSQLite(filepath.Join(t.TempDir(), "tenancy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rows.Close() })
	return New(rows, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestMembershipUUIDIsDerivedAndStable(t *testing.T) {
	u1 := MembershipUUID("person-a", "group-1")
	u2 := MembershipUUID("person-a", "group-1")
	if u1 != u2 {
		t.Fatal("the membership uuid must derive deterministically — a re-grant finds the old rows")
	}
	if u1 == MembershipUUID("person-a", "group-2") {
		t.Fatal("different groups must derive different uuids")
	}
	if u1 == MembershipUUID("person-b", "group-1") {
		t.Fatal("different people must derive different uuids")
	}
	// Disjoint from the personal space by construction — that disjointness is
	// what removes the need for a group: scope tier.
	if u1 == "person-a" {
		t.Fatal("the derived uuid must not be the personal uuid")
	}
}

func TestGroupLifecycle(t *testing.T) {
	ctx := context.Background()
	r := openRegistry(t)

	g, err := r.CreateGroup(ctx, "family", "master-u")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddMember(ctx, g.UUID, "member-u"); err != nil {
		t.Fatal(err)
	}
	member, err := r.IsMember(ctx, g.UUID, "member-u")
	if err != nil || !member {
		t.Fatalf("member-u = %v, %v", member, err)
	}

	// Mastership transfers to members only, by the master only.
	if err := r.SetMaster(ctx, g.UUID, "member-u", "master-u"); err == nil {
		t.Fatal("a non-master transferred mastership")
	}
	if err := r.SetMaster(ctx, g.UUID, "master-u", "outsider-u"); err == nil {
		t.Fatal("mastership transferred to a non-member")
	}
	if err := r.SetMaster(ctx, g.UUID, "master-u", "member-u"); err != nil {
		t.Fatalf("transfer: %v", err)
	}

	// Disbanding is the (current) master's act; access ends now, rows die at
	// retention. The transferred-away master no longer holds it.
	if err := r.Disband(ctx, g.UUID, "master-u"); err == nil {
		t.Fatal("the former master disbanded the group after transferring mastership")
	}
	if err := r.Disband(ctx, g.UUID, "member-u"); err != nil {
		t.Fatalf("disband: %v", err)
	}
	if _, err := r.AddMember(ctx, g.UUID, "new-u"); err == nil {
		t.Fatal("a disbanded group admitted a member")
	}
	groups, err := r.GroupsOf(ctx, "member-u")
	if err != nil {
		t.Fatal(err)
	}
	// GroupsOf hides disbanded groups: the toggle must not offer them.
	if len(groups) != 0 {
		t.Fatalf("GroupsOf = %v; want the disbanded group hidden", groups)
	}
}

func TestPoolHasExactlyOneOwner(t *testing.T) {
	ctx := context.Background()
	r := openRegistry(t)
	g, err := r.CreateGroup(ctx, "family", "master-u")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.CreatePool(ctx, "p1", "master-u", "tenant-u", "group-x", 30); err == nil {
		t.Fatal("a pool with two owners was created")
	}
	if _, err := r.CreatePool(ctx, "p1", "master-u", "", "", 30); err == nil {
		t.Fatal("a pool with no owner was created")
	}
	if _, err := r.CreatePool(ctx, "p1", "master-u", "", g.UUID, 0); err == nil {
		t.Fatal("a pool with no retention was created (retention is immutable, so it must be set)")
	}
	if _, err := r.CreatePool(ctx, "p1", "member-u", "", g.UUID, 30); err == nil {
		t.Fatal("a non-master created a group pool")
	}
	p, err := r.CreatePool(ctx, "p1", "master-u", "", g.UUID, 30)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreatePool(ctx, "p1", "tenant-u", "tenant-u", "", 30); err == nil {
		t.Fatal("a duplicate pool name was accepted")
	}
	own, err := r.CreatePool(ctx, "own", "other-u", "other-u", "", 30)
	if err != nil {
		t.Fatal(err)
	}

	// Only the owner may delete; for a group pool that is the master.
	if err := r.DeletePool(ctx, p.UUID, "member-u"); err == nil {
		t.Fatal("a member deleted the group pool")
	}
	if err := r.DeletePool(ctx, own.UUID, "other-u"); err != nil {
		t.Fatalf("owner delete: %v", err)
	}
}

func TestAuthorizePoolGrantLookup(t *testing.T) {
	ctx := context.Background()
	r := openRegistry(t)
	g, err := r.CreateGroup(ctx, "family", "master-u")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"master-u", "member-u", "other-u"} {
		if _, err := r.AddMember(ctx, g.UUID, u); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := r.CreatePool(ctx, "family-notes", "master-u", "", g.UUID, 30)
	if err != nil {
		t.Fatal(err)
	}
	// The carve: one wildcard r/w grant for the agent across members, and one
	// member held to read-only (the per-member override case).
	if err := r.SetGrant(ctx, pool.UUID, "writer", "", ModeReadWrite); err != nil {
		t.Fatal(err)
	}
	if err := r.SetGrant(ctx, pool.UUID, "writer", "other-u", ModeReadOnly); err != nil {
		t.Fatal(err)
	}

	const agent = "writer"
	if _, ok := r.AuthorizePool(pool.UUID, agent, "member-u"); !ok {
		t.Fatal("a member was denied the wildcard grant")
	}
	if mode, ok := r.AuthorizePool(pool.UUID, agent, "other-u"); !ok || mode != ModeReadOnly {
		t.Fatalf("override = %q, %v; want read-only", mode, ok)
	}
	if _, ok := r.AuthorizePool(pool.UUID, agent, "outsider-u"); ok {
		t.Fatal("an outsider was granted through the wildcard")
	}
	if _, ok := r.AuthorizePool(pool.UUID, "librarian", "member-u"); ok {
		t.Fatal("an uncarved agent was granted (the (pool, agent) half is load-bearing)")
	}

	// A tenant pool's wildcard covers nobody but its owner.
	ten, err := r.CreatePool(ctx, "personal-notes", "owner-u", "owner-u", "", 30)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetGrant(ctx, ten.UUID, "writer", "", ModeReadWrite); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.AuthorizePool(ten.UUID, "writer", "member-u"); ok {
		t.Fatal("a stranger was granted through a tenant pool's wildcard")
	}
	if _, ok := r.AuthorizePool(ten.UUID, "writer", "owner-u"); !ok {
		t.Fatal("the owning tenant was denied its own wildcard grant")
	}
}

func TestRemoveMemberDropsExactGrants(t *testing.T) {
	ctx := context.Background()
	r := openRegistry(t)
	g, err := r.CreateGroup(ctx, "family", "master-u")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddMember(ctx, g.UUID, "member-u"); err != nil {
		t.Fatal(err)
	}
	pool, err := r.CreatePool(ctx, "notes", "master-u", "", g.UUID, 30)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetGrant(ctx, pool.UUID, "writer", "member-u", ModeReadWrite); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveMember(ctx, g.UUID, "member-u"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.AuthorizePool(pool.UUID, "writer", "member-u"); ok {
		t.Fatal("a removed member kept its exact grant")
	}
	// The membership uuid is derivable regardless — re-adding restores it,
	// which is the settled intent.
	if MembershipUUID("member-u", g.UUID) == "" {
		t.Fatal("derivation lost")
	}
}

func TestSweepCascadesADisbandedGroup(t *testing.T) {
	ctx := context.Background()
	r := openRegistry(t)
	g, err := r.CreateGroup(ctx, "family", "master-u")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddMember(ctx, g.UUID, "member-u"); err != nil {
		t.Fatal(err)
	}
	pool, err := r.CreatePool(ctx, "notes", "master-u", "", g.UUID, 30)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetGrant(ctx, pool.UUID, "writer", "", ModeReadWrite); err != nil {
		t.Fatal(err)
	}
	// Shrink the retention to nothing, disband, force the row to expire, and
	// sweep: memberships, memberof rows, pools and grants go with the group.
	r.DisbandRetention = 1
	if err := r.Disband(ctx, g.UUID, "master-u"); err != nil {
		t.Fatal(err)
	}
	if err := r.rows.PutRow(ctx, "group|"+g.UUID, mustJSON(g), time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n, err := r.rows.SweepExpired(ctx); err != nil || n == 0 {
		t.Fatalf("SweepExpired = %d, %v; want the group row gone", n, err)
	}
	if err := r.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := r.Pool(ctx, pool.UUID); ok {
		t.Fatal("the disbanded group's pool survived the sweep")
	}
	grants, err := r.GrantsFor(ctx, pool.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 0 {
		t.Fatalf("grants survived the sweep: %v", grants)
	}
	if _, err := r.GroupsOf(ctx, "member-u"); err != nil {
		t.Fatal(err)
	}
}
