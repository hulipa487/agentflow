package shell

import (
	"context"
	"time"
)

// Registration is what makes a shell handle outlive the process that created
// it.
//
// A handle is a resource somewhere else: a container on a Docker host, a
// connection to a host. In a single instance the manager's maps are the whole
// story. In a fleet they are not, and two things break without a record of the
// handle outside this process:
//
//   - A session that moves to another instance — its lease lapsed, a peer took
//     it — cannot reach the container its own conversation created. The loop
//     holds a handle id from its history and gets "not found" for a container
//     that is still running.
//   - A handle whose instance was killed is never reaped, because reaping runs
//     on the instance that owned the session and that instance is gone. The
//     container stays on the host forever.
//
// The record answers both: it names the resource (a container id is addressable
// from anywhere that can reach the Docker host) and it says which session owns
// it, so an instance that takes the session over can adopt it, and a reclaim
// pass can destroy what no live session can still be using.
//
// The record is deliberately not a handle: it holds no connection, no client,
// and no secret. A password or key never reaches it — which is exactly why an
// SSH handle cannot be adopted across instances (see Attacher) while a Docker
// container can.
//
// The record lives on the same store as everything else, so it is shared when
// the runtime store is: a per-instance store gives each instance its own view,
// the same limit the session inbox and the leases have.
type Record struct {
	ID        string         `json:"id"`
	Owner     string         `json:"owner"` // the session key the handle belongs to
	Provider  string         `json:"provider"`
	Image     string         `json:"image,omitempty"`
	State     HandleState    `json:"state"`
	Container string         `json:"container,omitempty"` // docker: the container to attach to or remove
	Host      string         `json:"host,omitempty"`      // ssh: the target, for logs and audits
	User      string         `json:"user,omitempty"`      // ssh: the login, for the same
	Instance  string         `json:"instance,omitempty"`  // the instance that created it
	CreatedAt int64          `json:"created_at"`
	LiveAt    int64          `json:"live_at"` // last seen belonging to a live session
	Meta      map[string]any `json:"meta,omitempty"`
}

// Registry stores handle records where every instance can see them. The store
// adapter lives above this package (caps.ShellStore over the runtime store);
// the manager treats a nil Registry as "single instance", which is the default
// and costs nothing.
type Registry interface {
	Save(ctx context.Context, rec Record) error
	Load(ctx context.Context, id string) (Record, bool, error)
	// List returns the records for one owner, or every record when owner is
	// empty. A deployment holds one record per shell — tens, not millions —
	// which is why the sweep and the owner lookups can share one call.
	List(ctx context.Context, owner string) ([]Record, error)
	Delete(ctx context.Context, id string) error
}

// Attacher is implemented by providers whose handle can be rebuilt from its
// record on another instance, because the resource is addressable rather than
// an open connection. Docker implements it: a container is reached by id from
// any process that can reach the Docker host, so a session that moves keeps the
// shell it was using — its files, its environment, its processes.
//
// A provider that does not implement it cannot be adopted: the SSH handle's
// state is a live client, and its credentials are deliberately not in the
// record, so a takeover cannot rebuild it. The session spawns a fresh one
// instead — the remote host is unchanged, so only the connection is new.
type Attacher interface {
	Attach(rec Record) (*Handle, error)
}

// Forgetter is implemented by providers that can release the resource a record
// describes without having a live handle for it. It is what the reclaim pass
// calls on a handle whose instance is gone: Docker removes the container by the
// id in the record. A provider whose resource dies with the process that opened
// it (SSH) returns nil — there is nothing left to release, and the record is
// deleted.
//
// An error means the resource is still there and the record must survive, so
// the next pass tries again.
type Forgetter interface {
	Forget(ctx context.Context, rec Record) error
}

// ReclaimGrace is how long a handle is kept after the session that owns it
// stopped being alive anywhere.
//
// It is a grace window, not a cache TTL: a session's shell is part of the
// conversation, so a failover has to find it. The window is measured from the
// last time the handle was *seen* to belong to a live session (LiveAt,
// refreshed by the reclaim pass), not from when it was created — otherwise a
// container created last week in a session that was alive until a minute ago
// would be reclaimed the moment its instance died, which is precisely the
// failover it exists to survive.
const ReclaimGrace = 24 * time.Hour

// ReclaimInterval is how often an instance sweeps for handles to reclaim. It
// sets the resolution of the grace window — a handle is reclaimed at the first
// pass after the window has passed — so it is short relative to the window
// (minutes against a day) and long relative to a sweep's cost (one read of the
// handle records, and one write per live handle whose clock is due).
const ReclaimInterval = 5 * time.Minute

// recordOf builds the durable record for a live handle.
//
// The provider's address comes from the handle's Meta, which is where each
// provider already publishes what its handle is: docker sets "container", ssh
// sets "host" and "user". A provider that sets neither produces a record with
// no address — registered, reapable, and not adoptable, which is the honest
// description of a handle nothing outside its process can reach.
func recordOf(h *Handle, owner, instance string) Record {
	now := time.Now().Unix()
	rec := Record{
		ID:        h.ID,
		Owner:     owner,
		Provider:  h.Provider,
		Image:     h.Image,
		State:     h.State,
		Instance:  instance,
		CreatedAt: now,
		LiveAt:    now,
		Meta:      h.Meta,
	}
	if h.Meta != nil {
		rec.Container, _ = h.Meta["container"].(string)
		rec.Host, _ = h.Meta["host"].(string)
		rec.User, _ = h.Meta["user"].(string)
	}
	return rec
}

// handleOf rebuilds a handle from a record. It is how a record that is being
// released without a live handle still reaches a provider: the provider needs
// its own address back, and nothing else in the record is its business.
func handleOf(rec Record) *Handle {
	return &Handle{
		ID:       rec.ID,
		Provider: rec.Provider,
		Image:    rec.Image,
		State:    rec.State,
		Meta:     rec.Meta,
	}
}
