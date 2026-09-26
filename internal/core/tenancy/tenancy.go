// Package tenancy is the engine's record of who shares what: groups
// (families, teams), their memberships, storage pools, and the grants that
// decide which agent may reach which pool for which tenant.
//
// The split of labor this package must not cross: the control plane owns the
// product decisions (who is invited, who administers, who pays); this package
// owns the mechanism those decisions act through, and the memory scope layer
// (internal/core/memory) enforces them. Enforcement never lives in Lua and
// never lives here — AuthorizePool is a lookup, not a policy.
//
// Isolation is structural, per the vision's §5.7:
//
//   - A group needs no identity of its own. Group context is one membership
//     uuid — derived, not assigned — plus the pool uuids the member holds
//     grants on. Private group-context storage is `user:<membership_uuid>`,
//     which the existing user: rule already isolates; no group: scope tier
//     exists anywhere.
//   - The only new stratum is `pool:<uuid>`, and the rule for it lives in
//     WrapScoped: the caller must hold a grant, respecting r/w vs r-only.
//   - The master boundary is arithmetic, not policy: a master's authority is
//     over group resources (this package's rows) only, and no API here can
//     mint another member's personal or membership uuid, so no master can
//     reach a member's private store.
package tenancy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"agentflow/internal/core/runtime"
)

// membershipNamespace is the derivation namespace for membership uuids:
// membership_uuid = SHA1(ns, personal_uuid + "\x00" + group_uuid). It is data
// compatibility surface — changing it orphans every member's group-context
// private store, the same way the qdrant driver's point namespace would. It
// is versioned by existing at all; treat it like a schema constant: choose it
// once, never change it.
var membershipNamespace = uuid.MustParse("8f9b2a60-4c1e-4d47-9a55-3b2f1d6c0e81")

// MembershipUUID derives the group-context uuid for a person in a group.
// Derivation is the point: the router already knows (user, group), so scope
// resolution is computed, never looked up — while authorization still
// consults the membership table. A removed member who is re-added derives the
// same uuid and so finds their old group-context rows: intended behaviour,
// not an artefact.
func MembershipUUID(personalUUID, groupUUID string) string {
	return uuid.NewSHA1(membershipNamespace, []byte(personalUUID+"\x00"+groupUUID)).String()
}

// Group is a named set of member tenants with one master.
type Group struct {
	UUID      string `json:"uuid"`
	Name      string `json:"name"`
	Master    string `json:"master"` // the master tenant's personal uuid
	CreatedAt int64  `json:"created_at"`
	// DisbandedAt is set when the group is disbanded; the group and its
	// members' access end at once, and the row itself is deleted when its
	// retention expires (B7x — deployment-configurable).
	DisbandedAt int64 `json:"disbanded_at,omitempty"`
}

// Membership is one person's presence in one group.
type Membership struct {
	Group    string `json:"group"`
	User     string `json:"user"`
	JoinedAt int64  `json:"joined_at"`
}

// Pool is a named shared store with exactly one owner — a tenant or a group —
// so a pool uuid implies which membership list governs it.
type Pool struct {
	UUID  string `json:"uuid"`
	Name  string `json:"name"` // globally unique; config store bindings reference it
	Owner struct {
		Tenant string `json:"tenant,omitempty"` // a personal uuid, or
		Group  string `json:"group,omitempty"`  // a group uuid — exactly one is set
	} `json:"owner"`
	// RetentionDays is immutable (C12): the agent or owner that created the
	// pool chose it, and it is read-only thereafter. Extending it is a new
	// pool, never an edit.
	RetentionDays int   `json:"retention_days"`
	CreatedAt     int64 `json:"created_at"`
	CreatedBy     string `json:"created_by"` // the tenant uuid that created it
}

// Grant modes. A grant is per (pool, agent, tenant); the tenant "" is the
// wildcard for "every active member" (C13: in a group every member writes
// into one carve), with an exact per-member grant taking precedence when one
// exists — the r-only-for-one-member case.
const (
	ModeReadWrite = "rw"
	ModeReadOnly  = "r"
)

// grantKey is the row key for one (pool, agent, tenant) grant. The wildcard
// is a literal "*", which no uuid or agent name contains.
func grantKey(poolUUID, agent, tenant string) string {
	if tenant == "" {
		tenant = "*"
	}
	return "grant|" + poolUUID + "|" + agent + "|" + tenant
}

// Registry is the tenancy store over the runtime rows table.
type Registry struct {
	rows runtime.Rows
	log  *slog.Logger

	// DisbandRetention is how long after disbanding a group's row survives
	// before the sweep deletes it (B7x). Zero means the default.
	DisbandRetention time.Duration

	mu sync.Mutex // serializes read-modify-write mutations
}

// DefaultDisbandRetention is B7x's number: thirty days, matching the route
// state's default TTL. The control plane may override it at construction.
const DefaultDisbandRetention = 30 * 24 * time.Hour

func New(rows runtime.Rows, log *slog.Logger) *Registry {
	return &Registry{rows: rows, log: log, DisbandRetention: DefaultDisbandRetention}
}

func (r *Registry) now() int64 { return time.Now().Unix() }

// --- groups ------------------------------------------------------------------

func (r *Registry) CreateGroup(ctx context.Context, name, creator string) (Group, error) {
	if strings.TrimSpace(name) == "" {
		return Group{}, fmt.Errorf("tenancy: group name is empty")
	}
	if creator == "" {
		return Group{}, fmt.Errorf("tenancy: group creator is empty")
	}
	g := Group{UUID: uuid.New().String(), Name: name, Master: creator, CreatedAt: r.now()}
	if err := r.put(ctx, "group|"+g.UUID, g); err != nil {
		return Group{}, err
	}
	if _, err := r.AddMember(ctx, g.UUID, creator); err != nil {
		return Group{}, err
	}
	return g, nil
}

func (r *Registry) Group(ctx context.Context, uuid string) (Group, bool, error) {
	var g Group
	ok, err := r.get(ctx, "group|"+uuid, &g)
	return g, ok, err
}

// ListGroups returns every group. The row layout ("group|<uuid>" versus
// "group|<uuid>|member|<user>") shares a prefix, so membership rows are
// filtered out by segment count.
func (r *Registry) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := r.rows.ListRows(ctx, "group|")
	if err != nil {
		return nil, err
	}
	var out []Group
	for _, row := range rows {
		if strings.Count(row.Key, "|") != 1 {
			continue // a membership row
		}
		var g Group
		if err := json.Unmarshal([]byte(row.Value), &g); err != nil {
			return nil, fmt.Errorf("tenancy: corrupt group row %q: %w", row.Key, err)
		}
		out = append(out, g)
	}
	return out, nil
}

// ListPools returns every pool.
func (r *Registry) ListPools(ctx context.Context) ([]Pool, error) {
	rows, err := r.rows.ListRows(ctx, "pool|")
	if err != nil {
		return nil, err
	}
	var out []Pool
	for _, row := range rows {
		var p Pool
		if err := json.Unmarshal([]byte(row.Value), &p); err != nil {
			return nil, fmt.Errorf("tenancy: corrupt pool row %q: %w", row.Key, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// GroupsOf lists the groups a person belongs to, via the memberof reverse
// index — the router's per-chat toggle needs this list to validate the
// identity a chat proposes to act under.
func (r *Registry) GroupsOf(ctx context.Context, user string) ([]Group, error) {
	rows, err := r.rows.ListRows(ctx, "memberof|"+user+"|")
	if err != nil {
		return nil, err
	}
	var out []Group
	for _, row := range rows {
		groupUUID := strings.TrimPrefix(row.Key, "memberof|"+user+"|")
		g, ok, err := r.Group(ctx, groupUUID)
		if err != nil {
			return nil, err
		}
		if ok && g.DisbandedAt == 0 {
			out = append(out, g)
		}
	}
	return out, nil
}

func (r *Registry) IsMember(ctx context.Context, groupUUID, user string) (bool, error) {
	_, ok, err := r.rows.GetRow(ctx, "group|"+groupUUID+"|member|"+user)
	return ok, err
}

// AddMember records one person in one group. It is the mechanism behind an
// accepted invite; who may invite is the control plane's decision (B7), and
// the admin API is the only caller.
func (r *Registry) AddMember(ctx context.Context, groupUUID, user string) (Membership, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok, err := r.Group(ctx, groupUUID)
	if err != nil {
		return Membership{}, err
	}
	if !ok {
		return Membership{}, fmt.Errorf("tenancy: group %q does not exist", groupUUID)
	}
	if g.DisbandedAt != 0 {
		return Membership{}, fmt.Errorf("tenancy: group %q is disbanded", g.Name)
	}
	m := Membership{Group: groupUUID, User: user, JoinedAt: r.now()}
	if err := r.put(ctx, "group|"+groupUUID+"|member|"+user, m); err != nil {
		return Membership{}, err
	}
	if err := r.rows.PutRow(ctx, "memberof|"+user+"|"+groupUUID, "1", time.Time{}); err != nil {
		return Membership{}, err
	}
	return m, nil
}

// RemoveMember drops one person's membership and grants. Their rows stay:
// in a shared carve nothing is attributed per member, and their
// membership-scoped private store is keyed by a uuid this package never
// deletes — so a re-add finds the old scope intact, which the design settles
// as intended (§5.7).
func (r *Registry) RemoveMember(ctx context.Context, groupUUID, user string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok, err := r.Group(ctx, groupUUID); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("tenancy: group %q does not exist", groupUUID)
	}
	if err := r.rows.DeleteRow(ctx, "group|"+groupUUID+"|member|"+user); err != nil {
		return err
	}
	if err := r.rows.DeleteRow(ctx, "memberof|"+user+"|"+groupUUID); err != nil {
		return err
	}
	// The member's exact grants go with the membership; wildcard grants for
	// active members simply stop applying to them (the membership check).
	return r.dropMemberGrants(ctx, groupUUID, user)
}

func (r *Registry) dropMemberGrants(ctx context.Context, groupUUID, user string) error {
	pools, err := r.poolsOwnedByGroup(ctx, groupUUID)
	if err != nil {
		return err
	}
	for _, p := range pools {
		rows, err := r.rows.ListRows(ctx, "grant|"+p.UUID+"|")
		if err != nil {
			return err
		}
		for _, row := range rows {
			// grant|<pool>|<agent>|<tenant> — the tenant is the last segment.
			parts := strings.Split(row.Key, "|")
			if len(parts) == 4 && parts[3] == user {
				if err := r.rows.DeleteRow(ctx, row.Key); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (r *Registry) Members(ctx context.Context, groupUUID string) ([]Membership, error) {
	rows, err := r.rows.ListRows(ctx, "group|"+groupUUID+"|member|")
	if err != nil {
		return nil, err
	}
	out := make([]Membership, 0, len(rows))
	for _, row := range rows {
		var m Membership
		if err := json.Unmarshal([]byte(row.Value), &m); err != nil {
			return nil, fmt.Errorf("tenancy: corrupt membership row %q: %w", row.Key, err)
		}
		out = append(out, m)
	}
	return out, nil
}

// SetMaster transfers mastership (B7). Only the current master may transfer,
// and only to an active member.
func (r *Registry) SetMaster(ctx context.Context, groupUUID, from, to string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok, err := r.Group(ctx, groupUUID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("tenancy: group %q does not exist", groupUUID)
	}
	if g.Master != from {
		return fmt.Errorf("tenancy: only the group master may transfer mastership")
	}
	member, err := r.IsMember(ctx, groupUUID, to)
	if err != nil {
		return err
	}
	if !member {
		return fmt.Errorf("tenancy: mastership transfers to members only; %q is not in %q", to, g.Name)
	}
	g.Master = to
	return r.put(ctx, "group|"+g.UUID, g)
}

// Disband ends a group: access stops now, and the group's rows (and its
// pools' grants) are deleted when the retention elapses (B7x). The pools
// themselves die with the sweep — data persisted for the retention window,
// then deleted.
func (r *Registry) Disband(ctx context.Context, groupUUID, by string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok, err := r.Group(ctx, groupUUID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("tenancy: group %q does not exist", groupUUID)
	}
	if g.Master != by {
		return fmt.Errorf("tenancy: only the group master may disband")
	}
	if g.DisbandedAt != 0 {
		return nil // already disbanded
	}
	retention := r.DisbandRetention
	if retention <= 0 {
		retention = DefaultDisbandRetention
	}
	g.DisbandedAt = r.now()
	if err := r.put(ctx, "group|"+g.UUID, g); err != nil {
		return err
	}
	// Mark the row to expire; SweepExpired deletes it, and Sweep then
	// cascades to memberships and group pools.
	return r.rows.PutRow(ctx, "group|"+g.UUID, mustJSON(g), time.Now().Add(retention))
}

// --- pools -------------------------------------------------------------------

// CreatePool creates a pool with exactly one owner. Retention is recorded
// once and never mutated (C12). For a group pool the creator must be the
// group's master — the master administers the group's resources (§2.1) — and
// for a tenant pool the creator must be the tenant.
func (r *Registry) CreatePool(ctx context.Context, name, creator string, ownerTenant, ownerGroup string, retentionDays int) (Pool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.TrimSpace(name) == "" {
		return Pool{}, fmt.Errorf("tenancy: pool name is empty")
	}
	if (ownerTenant == "") == (ownerGroup == "") {
		return Pool{}, fmt.Errorf("tenancy: a pool has exactly one owner — a tenant or a group")
	}
	if retentionDays <= 0 {
		return Pool{}, fmt.Errorf("tenancy: pool %q needs a positive retention (it is immutable once set)", name)
	}
	if _, ok, err := r.rows.GetRow(ctx, "poolname|"+name); err != nil {
		return Pool{}, err
	} else if ok {
		return Pool{}, fmt.Errorf("tenancy: pool name %q is already taken", name)
	}
	switch {
	case ownerGroup != "":
		g, ok, err := r.Group(ctx, ownerGroup)
		if err != nil {
			return Pool{}, err
		}
		if !ok {
			return Pool{}, fmt.Errorf("tenancy: pool owner group %q does not exist", ownerGroup)
		}
		if g.Master != creator {
			return Pool{}, fmt.Errorf("tenancy: only the group master may create a group pool")
		}
	case ownerTenant != "":
		if ownerTenant != creator {
			return Pool{}, fmt.Errorf("tenancy: a tenant pool is created by its owner")
		}
	}
	p := Pool{UUID: uuid.New().String(), Name: name, RetentionDays: retentionDays, CreatedAt: r.now(), CreatedBy: creator}
	p.Owner.Tenant = ownerTenant
	p.Owner.Group = ownerGroup
	if err := r.put(ctx, "pool|"+p.UUID, p); err != nil {
		return Pool{}, err
	}
	if err := r.rows.PutRow(ctx, "poolname|"+name, p.UUID, time.Time{}); err != nil {
		return Pool{}, err
	}
	return p, nil
}

func (r *Registry) Pool(ctx context.Context, poolUUID string) (Pool, bool, error) {
	var p Pool
	ok, err := r.get(ctx, "pool|"+poolUUID, &p)
	return p, ok, err
}

// PoolByName resolves a config store binding's pool reference — the bind-time
// half of the carve. A name that does not resolve is a boot error, which is
// what makes a misconfigured carve fail at boot the way checkRequires does.
func (r *Registry) PoolByName(ctx context.Context, name string) (Pool, bool, error) {
	row, ok, err := r.rows.GetRow(ctx, "poolname|"+name)
	if err != nil || !ok {
		return Pool{}, false, err
	}
	return r.Pool(ctx, row.Value)
}

// PoolUUIDByName is the Resolver the memory registry binds with.
func (r *Registry) PoolUUIDByName(ctx context.Context, name string) (string, error) {
	p, ok, err := r.PoolByName(ctx, name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("tenancy: pool %q does not exist (create it before binding stores to it)", name)
	}
	return p.UUID, nil
}

func (r *Registry) poolsOwnedByGroup(ctx context.Context, groupUUID string) ([]Pool, error) {
	rows, err := r.rows.ListRows(ctx, "pool|")
	if err != nil {
		return nil, err
	}
	var out []Pool
	for _, row := range rows {
		var p Pool
		if err := json.Unmarshal([]byte(row.Value), &p); err != nil {
			continue
		}
		if p.Owner.Group == groupUUID {
			out = append(out, p)
		}
	}
	return out, nil
}

// DeletePool removes a pool and its grants. The rows written under its scope
// age out with the owning stores' retention; the scope prefix itself simply
// stops resolving. Only the owner (or the group master) may delete.
func (r *Registry) DeletePool(ctx context.Context, poolUUID, by string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok, err := r.Pool(ctx, poolUUID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("tenancy: pool %q does not exist", poolUUID)
	}
	if p.Owner.Group != "" {
		g, ok, err := r.Group(ctx, p.Owner.Group)
		if err != nil {
			return err
		}
		if !ok || g.Master != by {
			return fmt.Errorf("tenancy: only the group master may delete a group pool")
		}
	} else if p.Owner.Tenant != by {
		return fmt.Errorf("tenancy: only the owning tenant may delete a pool")
	}
	if err := r.rows.DeleteRow(ctx, "poolname|"+p.Name); err != nil {
		return err
	}
	if err := r.rows.DeleteRow(ctx, "pool|"+p.UUID); err != nil {
		return err
	}
	grants, err := r.rows.ListRows(ctx, "grant|"+p.UUID+"|")
	if err != nil {
		return err
	}
	for _, row := range grants {
		if err := r.rows.DeleteRow(ctx, row.Key); err != nil {
			return err
		}
	}
	return nil
}

// SetGrant records one (pool, agent, tenant) grant. tenant "" is the member
// wildcard. The (pool, agent) half is the carve — validated at bind time;
// this call is the control plane or the owning tenant setting it.
func (r *Registry) SetGrant(ctx context.Context, poolUUID, agent, tenant, mode string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch mode {
	case ModeReadWrite, ModeReadOnly:
	default:
		return fmt.Errorf("tenancy: grant mode %q is not a mode (want %q or %q)", mode, ModeReadWrite, ModeReadOnly)
	}
	if strings.TrimSpace(agent) == "" {
		return fmt.Errorf("tenancy: grant agent is empty")
	}
	if _, ok, err := r.Pool(ctx, poolUUID); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("tenancy: pool %q does not exist", poolUUID)
	}
	return r.rows.PutRow(ctx, grantKey(poolUUID, agent, tenant), mode, time.Time{})
}

func (r *Registry) RemoveGrant(ctx context.Context, poolUUID, agent, tenant string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rows.DeleteRow(ctx, grantKey(poolUUID, agent, tenant))
}

// GrantsFor lists a pool's grants.
func (r *Registry) GrantsFor(ctx context.Context, poolUUID string) ([]GrantRow, error) {
	rows, err := r.rows.ListRows(ctx, "grant|"+poolUUID+"|")
	if err != nil {
		return nil, err
	}
	out := make([]GrantRow, 0, len(rows))
	for _, row := range rows {
		parts := strings.Split(row.Key, "|")
		if len(parts) != 4 {
			continue
		}
		out = append(out, GrantRow{Pool: parts[1], Agent: parts[2], Tenant: parts[3], Mode: row.Value})
	}
	return out, nil
}

// GrantRow is one stored grant, split back into its axes.
type GrantRow struct {
	Pool   string `json:"pool"`
	Agent  string `json:"agent"`
	Tenant string `json:"tenant"` // "*" is the member wildcard
	Mode   string `json:"mode"`
}

// AuthorizePool is the call-time half of the grant check — the only question
// the memory scope layer asks. Exact (pool, agent, tenant) grants win; the
// member wildcard applies to anyone active in the owning group (or, for a
// tenant pool, covers nobody: a tenant pool's wildcard still requires the
// caller to be the owner).
func (r *Registry) AuthorizePool(poolUUID, agent, tenantUUID string) (string, bool) {
	ctx := context.Background()
	mode, ok, err := r.rows.GetRow(ctx, grantKey(poolUUID, agent, tenantUUID))
	if err != nil {
		r.log.Warn("tenancy: grant lookup failed", "pool", poolUUID, "err", err)
		return "", false
	}
	if ok {
		return mode.Value, true
	}
	// Wildcard path: the caller must hold the membership the wildcard gates.
	p, ok, err := r.Pool(ctx, poolUUID)
	if err != nil || !ok {
		return "", false
	}
	if p.Owner.Group != "" {
		member, err := r.IsMember(ctx, p.Owner.Group, tenantUUID)
		if err != nil || !member {
			return "", false
		}
	} else if p.Owner.Tenant != tenantUUID {
		return "", false
	}
	mode, ok, err = r.rows.GetRow(ctx, grantKey(poolUUID, agent, ""))
	if err != nil || !ok {
		return "", false
	}
	return mode.Value, true
}

// --- sweep -------------------------------------------------------------------

// Sweep cascades what SweepExpired has already expired: memberships and
// memberof rows of groups that no longer exist, and group-owned pools (with
// their grants) of groups whose retention has elapsed. It is called from the
// engine's maintenance loop, next to the other sweeps.
func (r *Registry) Sweep(ctx context.Context) error {
	pools, err := r.rows.ListRows(ctx, "pool|")
	if err != nil {
		return err
	}
	for _, row := range pools {
		var p Pool
		if err := json.Unmarshal([]byte(row.Value), &p); err != nil {
			continue
		}
		if p.Owner.Group == "" {
			continue
		}
		if _, ok, err := r.Group(ctx, p.Owner.Group); err != nil {
			return err
		} else if ok {
			continue
		}
		// The group is gone (retention elapsed): its pools die with it. The
		// deletion is done directly — DeletePool's ownership check consults
		// the master, who no longer exists.
		if err := r.rows.DeleteRow(ctx, "poolname|"+p.Name); err != nil {
			return err
		}
		if err := r.rows.DeleteRow(ctx, "pool|"+p.UUID); err != nil {
			return err
		}
		grants, err := r.rows.ListRows(ctx, "grant|"+p.UUID+"|")
		if err != nil {
			return err
		}
		for _, g := range grants {
			if err := r.rows.DeleteRow(ctx, g.Key); err != nil {
				return err
			}
		}
	}
	memberofs, err := r.rows.ListRows(ctx, "memberof|")
	if err != nil {
		return err
	}
	for _, row := range memberofs {
		parts := strings.Split(row.Key, "|")
		if len(parts) != 3 {
			continue
		}
		if _, ok, err := r.Group(ctx, parts[2]); err != nil {
			return err
		} else if ok {
			continue
		}
		if err := r.rows.DeleteRow(ctx, row.Key); err != nil {
			return err
		}
		if err := r.rows.DeleteRow(ctx, "group|"+parts[2]+"|member|"+parts[1]); err != nil {
			return err
		}
	}
	return nil
}

// --- rows helpers --------------------------------------------------------------

func (r *Registry) put(ctx context.Context, key string, v any) error {
	return r.rows.PutRow(ctx, key, mustJSON(v), time.Time{})
}

func (r *Registry) get(ctx context.Context, key string, into any) (bool, error) {
	row, ok, err := r.rows.GetRow(ctx, key)
	if err != nil || !ok {
		return false, err
	}
	if err := json.Unmarshal([]byte(row.Value), into); err != nil {
		return false, fmt.Errorf("tenancy: corrupt row %q: %w", key, err)
	}
	return true, nil
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("tenancy: %v", err))
	}
	return string(b)
}
