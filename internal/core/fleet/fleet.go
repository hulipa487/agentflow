// Package fleet is the instance registry: which AgentFlow processes are using
// one runtime store right now.
//
// The engine can already run as a fleet — leases and the session inbox live in
// a shared SQL store — but until this package nothing recorded who the
// instances were. Two consequences were silent: a `single` deployment with a
// stray second process behaved as an unplanned fleet, and an operator looking
// at one console could not tell whether it was one process or five.
//
// A row is a heartbeat, not a lock. Each instance upserts its own row every
// HeartbeatInterval and is "live" while its last_seen is within LiveWindow. A
// killed instance therefore disappears after the window rather than blocking
// anything; the lease table remains the authority for who owns work.
package fleet

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"agentflow/internal/core/storedb"
)

const (
	// HeartbeatInterval is how often an instance reports that it is alive.
	HeartbeatInterval = 10 * time.Second
	// LiveWindow is how old a heartbeat may be and still count as live. It is
	// three heartbeats, so one lost write does not make a healthy peer vanish.
	LiveWindow = 30 * time.Second
	// SweepAfter is how long a dead instance's row is kept. The row is useful
	// after the process is gone — it explains which instance held a session —
	// so it ages out on the daily sweep rather than on the live window.
	SweepAfter = 24 * time.Hour
)

// schema is one table on both backends. Times are unix seconds: a registry row
// is a liveness label, not a lease deadline, and second granularity makes the
// table readable when an operator queries it.
var schema = `CREATE TABLE IF NOT EXISTS instances (
	owner        TEXT PRIMARY KEY,
	region       TEXT,
	zone         TEXT,
	version      TEXT,
	topology     TEXT,
	config_epoch TEXT,
	started_at   BIGINT NOT NULL,
	last_seen    BIGINT NOT NULL
)`

// Instance is one registered process.
type Instance struct {
	Owner       string `json:"owner"`
	Region      string `json:"region"`
	Zone        string `json:"zone"`
	Version     string `json:"version"`
	Topology    string `json:"topology"`
	ConfigEpoch string `json:"config_epoch"`
	StartedAt   int64  `json:"started_at"`
	LastSeen    int64  `json:"last_seen"`
}

// Config identifies the process being registered.
type Config struct {
	Target      string // the runtime store target
	Owner       string // lease.OwnerID(); unique per process
	Region      string
	Zone        string
	Version     string
	Topology    string
	ConfigEpoch string
	Log         *slog.Logger
}

// Registry writes one process's heartbeat and reads every process's row.
type Registry struct {
	st          *storedb.DB
	owner       string
	region      string
	zone        string
	version     string
	topology    string
	configEpoch string
	startedAt   int64
	log         *slog.Logger
}

// Open creates the registry table and returns a handle. It does not write a row
// yet: the caller checks for a competing instance first (CheckSingle), then
// starts the heartbeat.
func Open(c Config) (*Registry, error) {
	if c.Owner == "" {
		return nil, fmt.Errorf("fleet: empty owner")
	}
	if c.Log == nil {
		c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	st, err := storedb.Open(c.Target)
	if err != nil {
		return nil, fmt.Errorf("fleet: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := st.ExecDDL(ctx, schema); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("fleet: %w", err)
	}
	return &Registry{
		st:          st,
		owner:       c.Owner,
		region:      c.Region,
		zone:        c.Zone,
		version:     c.Version,
		topology:    c.Topology,
		configEpoch: c.ConfigEpoch,
		startedAt:   time.Now().Unix(),
		log:         c.Log.With("module", "fleet"),
	}, nil
}

// Owner is this instance's id.
func (r *Registry) Owner() string { return r.owner }

// Heartbeat writes or refreshes this instance's row.
func (r *Registry) Heartbeat(ctx context.Context) error {
	now := time.Now().Unix()
	_, err := r.st.Exec(ctx, `
		INSERT INTO instances (owner, region, zone, version, topology, config_epoch, started_at, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (owner) DO UPDATE SET
			region = excluded.region,
			zone = excluded.zone,
			version = excluded.version,
			topology = excluded.topology,
			config_epoch = excluded.config_epoch,
			last_seen = excluded.last_seen`,
		r.owner, r.region, r.zone, r.version, r.topology, r.configEpoch, r.startedAt, now)
	if err != nil {
		return fmt.Errorf("fleet: heartbeat: %w", err)
	}
	return nil
}

// Start heartbeats immediately and then every HeartbeatInterval until ctx is
// cancelled. A failed write is logged and retried on the next tick: a registry
// that cannot answer must not take the engine down with it.
func (r *Registry) Start(ctx context.Context) {
	go func() {
		r.beat(ctx)
		tick := time.NewTicker(HeartbeatInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				r.beat(ctx)
			}
		}
	}()
}

func (r *Registry) beat(ctx context.Context) {
	if err := r.Heartbeat(ctx); err != nil {
		r.log.Warn("instance heartbeat failed", "err", err)
	}
}

// List returns every live instance, including this one, most recently started
// first. A row that has aged past the live window is not returned even if it
// has not been swept yet.
func (r *Registry) List(ctx context.Context) ([]Instance, error) {
	return r.listSince(ctx, time.Now().Add(-LiveWindow).Unix())
}

// Peers returns the other live instances.
func (r *Registry) Peers(ctx context.Context) ([]Instance, error) {
	all, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, in := range all {
		if in.Owner != r.owner {
			out = append(out, in)
		}
	}
	return out, nil
}

func (r *Registry) listSince(ctx context.Context, since int64) ([]Instance, error) {
	rows, err := r.st.Query(ctx, `
		SELECT owner, region, zone, version, topology, config_epoch, started_at, last_seen
		FROM instances WHERE last_seen >= ?
		ORDER BY started_at, owner`, since)
	if err != nil {
		return nil, fmt.Errorf("fleet: list: %w", err)
	}
	defer rows.Close()
	out := []Instance{}
	for rows.Next() {
		var in Instance
		if err := rows.Scan(&in.Owner, &in.Region, &in.Zone, &in.Version, &in.Topology, &in.ConfigEpoch, &in.StartedAt, &in.LastSeen); err != nil {
			return nil, fmt.Errorf("fleet: list: %w", err)
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fleet: list: %w", err)
	}
	return out, nil
}

// CheckSingle refuses to start when another live instance uses the store. It is
// the boot check `single` topology promises: two processes on one store are an
// unplanned fleet, and every symptom of that (sessions running twice, singleton
// work per instance) looks like a bug rather than a topology mistake.
//
// A peer whose process is gone but whose row has not expired is exactly what
// -force-single is for; waiting out LiveWindow is the other answer.
func (r *Registry) CheckSingle(ctx context.Context) error {
	peers, err := r.Peers(ctx)
	if err != nil {
		return err
	}
	if len(peers) == 0 {
		return nil
	}
	p := peers[0]
	last := time.Unix(p.LastSeen, 0).Format(time.RFC3339)
	return fmt.Errorf(
		"another AgentFlow instance (%s, region %q zone %q, version %s) used this store, last seen %s; single topology allows one instance. Stop it, set runtime.topology: regional, or start with -force-single if it crashed",
		p.Owner, p.Region, p.Zone, p.Version, last)
}

// Sweep deletes rows whose last heartbeat is older than the given cutoff.
func (r *Registry) Sweep(ctx context.Context, before time.Time) (int64, error) {
	res, err := r.st.Exec(ctx, `DELETE FROM instances WHERE last_seen < ?`, before.Unix())
	if err != nil {
		return 0, fmt.Errorf("fleet: sweep: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("fleet: sweep: %w", err)
	}
	return n, nil
}

// Close removes this instance's row and releases the store. The delete is
// best-effort: a shutdown that cannot reach the store has already lost the
// argument, and the row expiry is what makes that harmless.
func (r *Registry) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.st.Exec(ctx, `DELETE FROM instances WHERE owner = ?`, r.owner); err != nil {
		r.log.Warn("instance row not removed on shutdown", "err", err)
	}
	return r.st.Close()
}
