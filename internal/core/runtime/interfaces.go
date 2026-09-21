package runtime

import (
	"context"
	"time"
)

// The store's surface, split by what a consumer actually needs.
//
// The engine's state is three planes with different shapes, and a deployment
// may want them in different places:
//
//   - Rows — the key/value table the engine's own state lives in (file
//     metadata, session state, route state, shell handles). Read and written on
//     the hot path, small, transactional.
//   - Ledger — the token rollups every quota check and budget refresh reads.
//     Small, aggregated, and exact: sums and immediate consistency are the
//     whole point.
//   - Journal and EventLog — the append-only planes: the message audit trail
//     and the per-call usage detail. Written on every message and every
//     metered call, read rarely (an operator view), and expired by age. These
//     are the ones a deployment can put somewhere else — a log plane, in the
//     config — without anything above them changing, which is why they are
//     separate interfaces rather than methods on Store.
//
// Store is all of them, which is what the SQL backends implement and what a
// single-instance deployment uses for everything. A consumer takes the
// narrowest interface it can work with, so wiring a plane to a different
// backend is a constructor argument rather than a code change.
type Store interface {
	Journal
	Rows
	Ledger
	EventLog

	// Path identifies the store for logs. A PostgreSQL store reports a
	// credential-free form of its DSN: a store's address may be logged, its
	// password must not be.
	Path() string
	Close() error
}

// Journal is the core-owned audit trail: every inbound message (at the router)
// and every outbound reply (at the session egress) is appended here, so no loop
// can skip, edit or forge it. Writes are best-effort at both call sites — a
// journal write that fails is logged and the turn continues — and reads are an
// operator view.
type Journal interface {
	RecordMessage(ctx context.Context, e JournalEntry) error
	ListMessages(ctx context.Context, f JournalFilter) ([]JournalEntry, error)
	PruneMessages(ctx context.Context, cutoff time.Time) (int64, error)
	JournalRowCount(ctx context.Context) (int64, error)
}

// Rows is the key/value table the engine's own state lives in. Every consumer
// namespaces its keys with a prefix of its own ("t|" for file trees, "session|"
// for session state, "route|state|" for route state, "shell|h|" for handles),
// and nothing above this interface knows how the store keeps them.
type Rows interface {
	PutRow(ctx context.Context, key, value string, expiresAt time.Time) error
	GetRow(ctx context.Context, key string) (Row, bool, error)
	DeleteRow(ctx context.Context, key string) error
	ListRows(ctx context.Context, prefix string) ([]Row, error)
	SweepExpired(ctx context.Context) (int, error)
}

// Ledger is the token ledger: the per-user, per-agent, per-model, per-day
// rollups that quotas and budgets are enforced from, and the metered write that
// folds one call into them.
//
// The ledger methods take no context: each bounds itself with its own timeout,
// which is how this plane was written before the row and journal planes grew
// theirs. They are kept as they are rather than changed under every caller.
type Ledger interface {
	RecordUsage(rec UsageRecord, withEvent bool) error
	UsageForDay(userID, day string) (UsageTotals, error)
	UsageForAgentDay(agent, day string) (UsageTotals, error)
	UsageHistory(userID string, days int) ([]UsageRow, error)
	UsageDaily(day string) ([]UsageRow, error)
	PruneUsage(before time.Time) error
}

// EventLog is the per-call usage detail: one row per metered LLM call, written
// when usage.events is on, read by the console's per-user view, and expired by
// age. It is separate from Ledger because a deployment can keep the detail on a
// log plane while the rollups — which a quota check reads before every call —
// stay in the transactional store.
//
// The user id is a parameter rather than a field of UsageEvent because the
// event is what the provider call cost; whose call it was is the caller's
// attribution, and a file-backed plane has nowhere else to put it.
type EventLog interface {
	AppendEvent(userID string, ev UsageEvent) error
	UsageEvents(userID string, since time.Time, limit int) ([]UsageEvent, error)
	PruneEvents(before time.Time) (int64, error)
}

// EventFrom is the per-call detail row for a metered record, with the same
// normalisation RecordUsage applies to the rollup: a call the provider refused
// or errored on counts as an invocation and bills no tokens. Sharing the rule
// is what keeps the detail consistent with the rollup it sums into.
func EventFrom(rec UsageRecord) UsageEvent {
	if rec.At.IsZero() {
		rec.At = time.Now()
	}
	if !rec.OK {
		rec.Input, rec.Output, rec.Cached, rec.CacheWrite, rec.Reasoning = 0, 0, 0, 0, 0
	}
	return UsageEvent{
		Ts:    rec.At.Unix(),
		Kind:  rec.Kind,
		Model: rec.Model,
		Agent: rec.Agent,
		UsageTotals: UsageTotals{
			Input:      int64(rec.Input),
			Output:     int64(rec.Output),
			Cached:     int64(rec.Cached),
			CacheWrite: int64(rec.CacheWrite),
			Reasoning:  int64(rec.Reasoning),
		},
		OK: rec.OK,
	}
}
