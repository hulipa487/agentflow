package runtime

import (
	"context"
	"fmt"
	"time"
)

// usageSchema is the per-user token ledger, appended to the store migration.
//
// It has two layers on purpose. usage_daily is the rollup every quota check and
// dashboard reads: one row per (user, day, agent, model), folded with a single
// INSERT … ON CONFLICT DO UPDATE, which is atomic without a transaction (the
// store has none anywhere, and a read-modify-write here would race under
// concurrent LLM calls). usage_events is the per-call detail, opt-in because it
// is the high-volume table.
const usageSchema = `
	CREATE TABLE IF NOT EXISTS usage_daily (
		user_id     TEXT NOT NULL,
		day         TEXT NOT NULL,
		agent       TEXT NOT NULL,
		model       TEXT NOT NULL,
		input       INTEGER NOT NULL DEFAULT 0,
		output      INTEGER NOT NULL DEFAULT 0,
		cached      INTEGER NOT NULL DEFAULT 0,
		cache_write INTEGER NOT NULL DEFAULT 0,
		reasoning   INTEGER NOT NULL DEFAULT 0,
		calls       INTEGER NOT NULL DEFAULT 0,
		failed      INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (user_id, day, agent, model)
	);
	CREATE INDEX IF NOT EXISTS usage_daily_day ON usage_daily (day);

	CREATE TABLE IF NOT EXISTS usage_events (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		ts          INTEGER NOT NULL,
		user_id     TEXT NOT NULL,
		agent       TEXT NOT NULL,
		model       TEXT NOT NULL,
		kind        TEXT NOT NULL,
		input       INTEGER NOT NULL DEFAULT 0,
		output      INTEGER NOT NULL DEFAULT 0,
		cached      INTEGER NOT NULL DEFAULT 0,
		cache_write INTEGER NOT NULL DEFAULT 0,
		reasoning   INTEGER NOT NULL DEFAULT 0,
		ok          INTEGER NOT NULL DEFAULT 1
	);
	CREATE INDEX IF NOT EXISTS usage_events_user_ts ON usage_events (user_id, ts);
`

// UsageRecord is one metered LLM-family call, attributed to the user whose turn
// made it. An empty UserID is the shared service bucket: engine-fired work
// (scheduler, maintenance) has no user and must not be charged to one.
type UsageRecord struct {
	UserID     string
	Agent      string
	Model      string
	Kind       string // chat | stream | embed | rerank
	Input      int
	Output     int
	Cached     int
	CacheWrite int
	Reasoning  int
	OK         bool
	At         time.Time
}

// UsageTotals is the rollup for one ledger slice. Calls counts attempts and
// Failed the ones the provider refused, so "how many invocations" and "how many
// worked" are both answerable; tokens are only added by successful calls.
type UsageTotals struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	Cached     int64 `json:"cached"`
	CacheWrite int64 `json:"cache_write"`
	Reasoning  int64 `json:"reasoning"`
	Calls      int64 `json:"calls"`
	Failed     int64 `json:"failed"`
}

// Billable is what a quota charges for this slice: input + output, the same
// rule the budget pool already applies. A provider's input count already
// includes the cached prompt tokens and its output count already includes
// reasoning tokens, so adding those would double-charge; they are recorded for
// visibility instead. Discounting cache hits is a policy knob for later, once
// there is a price table to discount against.
func (u UsageTotals) Billable() int64 { return u.Input + u.Output }

// DayKey is the UTC day bucket a timestamp belongs to.
func DayKey(t time.Time) string { return t.UTC().Format("2006-01-02") }

// RecordUsage folds one call into the daily rollup and, when withEvent is set,
// appends the per-call detail row.
func (s *Store) RecordUsage(rec UsageRecord, withEvent bool) error {
	if rec.At.IsZero() {
		rec.At = time.Now()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A failed call still counts as an invocation but bills no tokens: the
	// provider refused or errored, so anything it consumed is unknown.
	failed := 0
	if !rec.OK {
		failed = 1
		rec.Input, rec.Output, rec.Cached, rec.CacheWrite, rec.Reasoning = 0, 0, 0, 0, 0
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_daily
			(user_id, day, agent, model, input, output, cached, cache_write, reasoning, calls, failed)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)
		ON CONFLICT (user_id, day, agent, model) DO UPDATE SET
			input       = input + excluded.input,
			output      = output + excluded.output,
			cached      = cached + excluded.cached,
			cache_write = cache_write + excluded.cache_write,
			reasoning   = reasoning + excluded.reasoning,
			calls       = calls + 1,
			failed      = failed + excluded.failed`,
		rec.UserID, DayKey(rec.At), rec.Agent, rec.Model,
		rec.Input, rec.Output, rec.Cached, rec.CacheWrite, rec.Reasoning, failed); err != nil {
		return fmt.Errorf("record usage: %w", err)
	}

	if !withEvent {
		return nil
	}
	ok := 0
	if rec.OK {
		ok = 1
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_events
			(ts, user_id, agent, model, kind, input, output, cached, cache_write, reasoning, ok)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.At.Unix(), rec.UserID, rec.Agent, rec.Model, rec.Kind,
		rec.Input, rec.Output, rec.Cached, rec.CacheWrite, rec.Reasoning, ok); err != nil {
		return fmt.Errorf("record usage event: %w", err)
	}
	return nil
}

// UsageForDay returns one user's totals for a UTC day.
func (s *Store) UsageForDay(userID, day string) (UsageTotals, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var t UsageTotals
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(input),0), COALESCE(SUM(output),0), COALESCE(SUM(cached),0),
		       COALESCE(SUM(cache_write),0), COALESCE(SUM(reasoning),0), COALESCE(SUM(calls),0),
		       COALESCE(SUM(failed),0)
		FROM usage_daily WHERE user_id = ? AND day = ?`, userID, day).
		Scan(&t.Input, &t.Output, &t.Cached, &t.CacheWrite, &t.Reasoning, &t.Calls, &t.Failed)
	if err != nil {
		return UsageTotals{}, err
	}
	return t, nil
}

// UsageRow is one ledger slice as an operator view sees it.
type UsageRow struct {
	UserID string `json:"user_id"`
	Day    string `json:"day"`
	Agent  string `json:"agent"`
	Model  string `json:"model"`
	UsageTotals
}

// UsageDaily returns every slice for a UTC day, largest first — the console's
// per-user accounting view.
func (s *Store) UsageDaily(day string) ([]UsageRow, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
		SELECT user_id, day, agent, model, input, output, cached, cache_write, reasoning, calls, failed
		FROM usage_daily WHERE day = ?
		ORDER BY (input + output) DESC, user_id`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageRow{}
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.UserID, &r.Day, &r.Agent, &r.Model,
			&r.Input, &r.Output, &r.Cached, &r.CacheWrite, &r.Reasoning, &r.Calls, &r.Failed); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageHistory returns one user's daily rows for the window ending today,
// newest first — the per-user trend a frontend renders.
func (s *Store) UsageHistory(userID string, days int) ([]UsageRow, error) {
	if days <= 0 || days > 90 {
		days = 7
	}
	from := DayKey(time.Now().AddDate(0, 0, -(days - 1)))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
		SELECT user_id, day, agent, model, input, output, cached, cache_write, reasoning, calls, failed
		FROM usage_daily WHERE user_id = ? AND day >= ?
		ORDER BY day DESC, model`, userID, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageRow{}
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.UserID, &r.Day, &r.Agent, &r.Model,
			&r.Input, &r.Output, &r.Cached, &r.CacheWrite, &r.Reasoning, &r.Calls, &r.Failed); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageEvent is one metered call's detail row.
type UsageEvent struct {
	Ts    int64  `json:"ts"`
	Kind  string `json:"kind"`
	Model string `json:"model"`
	Agent string `json:"agent"`
	UsageTotals
	OK bool `json:"ok"`
}

// UsageEvents returns a user's recent per-call rows, newest first (the detail
// view behind the rollup; only populated when usage.events is enabled).
func (s *Store) UsageEvents(userID string, since time.Time, limit int) ([]UsageEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, kind, model, agent, input, output, cached, cache_write, reasoning, ok
		FROM usage_events WHERE user_id = ? AND ts >= ?
		ORDER BY ts DESC LIMIT ?`, userID, since.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageEvent{}
	for rows.Next() {
		var e UsageEvent
		var ok int
		if err := rows.Scan(&e.Ts, &e.Kind, &e.Model, &e.Agent,
			&e.Input, &e.Output, &e.Cached, &e.CacheWrite, &e.Reasoning, &ok); err != nil {
			return nil, err
		}
		e.OK = ok != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneUsage drops ledger rows older than cutoff. The daily rollup is pruned by
// day string, the event log by timestamp.
func (s *Store) PruneUsage(before time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM usage_daily WHERE day < ?`, DayKey(before)); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM usage_events WHERE ts < ?`, before.Unix())
	return err
}
