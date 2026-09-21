package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// postgresStore is the runtime state in a PostgreSQL server: the shared store a
// fleet needs, where a journal row, a commit or a token count means the same
// thing to every instance. It implements Store with the same contract as the
// SQLite one.
//
// One deliberate difference from the SQLite implementation: the connection pool
// is bounded. A server has a connection limit and N instances share it, and an
// unbounded pool per instance is how a fleet takes its own database down.
type postgresStore struct {
	db  *sql.DB
	dsn string
}

// OpenPostgres connects to the runtime store in a PostgreSQL server and
// migrates it. A misconfigured or unreachable server fails here, at boot,
// rather than at the first write.
func OpenPostgres(dsn string) (Store, error) {
	if BackendFor(dsn) != BackendPostgres {
		return nil, fmt.Errorf("runtime: not a postgres dsn")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("runtime: open postgres: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("runtime: ping postgres: %w", err)
	}
	s := &postgresStore{db: db, dsn: dsn}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("runtime: migrate postgres: %w", err)
	}
	return s, nil
}

// Path is the credential-free form of the DSN, for logs.
func (s *postgresStore) Path() string { return redactDSN(s.dsn) }

func (s *postgresStore) Close() error { return s.db.Close() }

// postgresSchema is the runtime store's tables, one statement per entry.
//
// One statement per Exec rather than a single multi-statement string: the
// PostgreSQL driver's extended protocol refuses more than one command in a
// prepared statement, so a schema that worked on one backend is a boot failure
// on the other. Splitting it costs nothing and removes the question.
var postgresSchema = []string{
	`CREATE TABLE IF NOT EXISTS message_journal (
		id               TEXT NOT NULL,
		ts               BIGINT NOT NULL,
		direction        TEXT NOT NULL,
		status           TEXT NOT NULL,
		channel          TEXT,
		chat             TEXT,
		sender           TEXT,
		user_uuid        TEXT,
		agent            TEXT,
		session_id       TEXT,
		type             TEXT,
		text             TEXT,
		attachments_json TEXT,
		provenance_json  TEXT,
		err              TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS message_journal_id ON message_journal (id)`,
	`CREATE INDEX IF NOT EXISTS message_journal_ts ON message_journal (ts)`,
	`CREATE INDEX IF NOT EXISTS message_journal_session ON message_journal (session_id)`,
	`CREATE INDEX IF NOT EXISTS message_journal_user ON message_journal (user_uuid)`,
	// Engine-owned key/value rows for the user-scoped file store.
	`CREATE TABLE IF NOT EXISTS files_meta (
		key        TEXT PRIMARY KEY,
		value      TEXT NOT NULL,
		updated_at BIGINT NOT NULL,
		expires_at BIGINT NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE IF NOT EXISTS usage_daily (
		user_id     TEXT NOT NULL,
		day         TEXT NOT NULL,
		agent       TEXT NOT NULL,
		model       TEXT NOT NULL,
		input       BIGINT NOT NULL DEFAULT 0,
		output      BIGINT NOT NULL DEFAULT 0,
		cached      BIGINT NOT NULL DEFAULT 0,
		cache_write BIGINT NOT NULL DEFAULT 0,
		reasoning   BIGINT NOT NULL DEFAULT 0,
		calls       BIGINT NOT NULL DEFAULT 0,
		failed      BIGINT NOT NULL DEFAULT 0,
		PRIMARY KEY (user_id, day, agent, model)
	)`,
	`CREATE INDEX IF NOT EXISTS usage_daily_day ON usage_daily (day)`,
	// The per-agent rollup a deployment-wide agent budget reads.
	`CREATE INDEX IF NOT EXISTS usage_daily_agent_day ON usage_daily (agent, day)`,
	`CREATE TABLE IF NOT EXISTS usage_events (
		id          BIGSERIAL PRIMARY KEY,
		ts          BIGINT NOT NULL,
		user_id     TEXT NOT NULL,
		agent       TEXT NOT NULL,
		model       TEXT NOT NULL,
		kind        TEXT NOT NULL,
		input       BIGINT NOT NULL DEFAULT 0,
		output      BIGINT NOT NULL DEFAULT 0,
		cached      BIGINT NOT NULL DEFAULT 0,
		cache_write BIGINT NOT NULL DEFAULT 0,
		reasoning   BIGINT NOT NULL DEFAULT 0,
		ok          SMALLINT NOT NULL DEFAULT 1
	)`,
	`CREATE INDEX IF NOT EXISTS usage_events_user_ts ON usage_events (user_id, ts)`,
}

func (s *postgresStore) migrate(ctx context.Context) error {
	for _, stmt := range postgresSchema {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("postgres schema: %w", err)
		}
	}
	return nil
}

// --- message journal --------------------------------------------------------

func (s *postgresStore) RecordMessage(ctx context.Context, e JournalEntry) error {
	atts, err := json.Marshal(e.Attachments)
	if err != nil {
		return err
	}
	prov, err := json.Marshal(e.Provenance)
	if err != nil {
		return err
	}
	ts := e.Ts
	if ts == 0 {
		ts = time.Now().Unix()
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO message_journal
			(id, ts, direction, status, channel, chat, sender, user_uuid, agent, session_id,
			 type, text, attachments_json, provenance_json, err)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		e.ID, ts, e.Direction, e.Status, nullIfEmpty(e.Channel), nullIfEmpty(e.Chat),
		nullIfEmpty(e.Sender), nullIfEmpty(e.UserUUID), nullIfEmpty(e.Agent),
		nullIfEmpty(e.SessionID), nullIfEmpty(e.Type), nullIfEmpty(e.Text),
		string(atts), string(prov), nullIfEmpty(e.Err))
	return err
}

func (s *postgresStore) ListMessages(ctx context.Context, f JournalFilter) ([]JournalEntry, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	where := []string{"TRUE"}
	args := []any{}
	add := func(clause string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if f.UserUUID != "" {
		add("user_uuid = $%d", f.UserUUID)
	}
	if f.SessionID != "" {
		add("session_id = $%d", f.SessionID)
	}
	if f.Direction != "" {
		add("direction = $%d", f.Direction)
	}
	if f.Since > 0 {
		add("ts >= $%d", f.Since)
	}
	if f.Until > 0 {
		add("ts < $%d", f.Until)
	}
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, ts, direction, status, COALESCE(channel,''), COALESCE(chat,''),
		       COALESCE(sender,''), COALESCE(user_uuid,''), COALESCE(agent,''),
		       COALESCE(session_id,''), COALESCE(type,''), COALESCE(text,''),
		       COALESCE(attachments_json,''), COALESCE(provenance_json,''), COALESCE(err,'')
		FROM message_journal
		WHERE %s
		ORDER BY ts DESC, id DESC
		LIMIT $%d`, strings.Join(where, " AND "), len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []JournalEntry{}
	for rows.Next() {
		var e JournalEntry
		var atts, prov string
		if err := rows.Scan(&e.ID, &e.Ts, &e.Direction, &e.Status, &e.Channel, &e.Chat,
			&e.Sender, &e.UserUUID, &e.Agent, &e.SessionID, &e.Type, &e.Text,
			&atts, &prov, &e.Err); err != nil {
			return nil, err
		}
		if atts != "" {
			_ = json.Unmarshal([]byte(atts), &e.Attachments)
		}
		if prov != "" {
			_ = json.Unmarshal([]byte(prov), &e.Provenance)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *postgresStore) PruneMessages(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM message_journal WHERE ts < $1`, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *postgresStore) JournalRowCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_journal`).Scan(&n)
	return n, err
}

// --- key/value rows ---------------------------------------------------------

func (s *postgresStore) PutRow(ctx context.Context, key, value string, expiresAt time.Time) error {
	// Deadlines are unix nanoseconds, the same convention the SQLite store
	// uses, so a row written by one backend reads back identically in the other.
	exp := int64(0)
	if !expiresAt.IsZero() {
		exp = expiresAt.UnixNano()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO files_meta (key, value, updated_at, expires_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (key) DO UPDATE SET
			value = EXCLUDED.value,
			updated_at = EXCLUDED.updated_at,
			expires_at = EXCLUDED.expires_at`,
		key, value, time.Now().UnixNano(), exp)
	return err
}

func (s *postgresStore) GetRow(ctx context.Context, key string) (Row, bool, error) {
	var r Row
	var updated, exp int64
	err := s.db.QueryRowContext(ctx,
		`SELECT key, value, updated_at, expires_at FROM files_meta WHERE key = $1`, key).
		Scan(&r.Key, &r.Value, &updated, &exp)
	if err == sql.ErrNoRows {
		return Row{}, false, nil
	}
	if err != nil {
		return Row{}, false, err
	}
	r.UpdatedAt = time.Unix(0, updated)
	if exp != 0 {
		r.ExpiresAt = time.Unix(0, exp)
	}
	if exp != 0 && time.Now().UnixNano() > exp {
		// Lazy expiry, the same contract as the SQLite store: a read of an
		// expired row reports it missing and reclaims it.
		_ = s.DeleteRow(ctx, key)
		return Row{}, false, nil
	}
	return r, true, nil
}

func (s *postgresStore) DeleteRow(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM files_meta WHERE key = $1`, key)
	return err
}

func (s *postgresStore) ListRows(ctx context.Context, prefix string) ([]Row, error) {
	// starts_with rather than LIKE: it needs no escaping (a scope key embeds
	// ids like "u_1", where "_" would be a wildcard) and no collation
	// assumption. It scans, which is the right trade until the file table is
	// large enough to measure.
	rows, err := s.db.QueryContext(ctx, `
		SELECT key, value, updated_at, expires_at FROM files_meta
		WHERE starts_with(key, $1) ORDER BY key`, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Row{}
	now := time.Now().UnixNano()
	for rows.Next() {
		var r Row
		var updated, exp int64
		if err := rows.Scan(&r.Key, &r.Value, &updated, &exp); err != nil {
			return nil, err
		}
		if exp != 0 && now > exp {
			_ = s.DeleteRow(ctx, r.Key)
			continue
		}
		r.UpdatedAt = time.Unix(0, updated)
		if exp != 0 {
			r.ExpiresAt = time.Unix(0, exp)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *postgresStore) SweepExpired(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM files_meta WHERE expires_at != 0 AND expires_at < $1`, time.Now().UnixNano())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// --- usage ledger -----------------------------------------------------------

func (s *postgresStore) RecordUsage(rec UsageRecord, withEvent bool) error {
	if rec.At.IsZero() {
		rec.At = time.Now()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	failed := 0
	if !rec.OK {
		failed = 1
		rec.Input, rec.Output, rec.Cached, rec.CacheWrite, rec.Reasoning = 0, 0, 0, 0, 0
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_daily
			(user_id, day, agent, model, input, output, cached, cache_write, reasoning, calls, failed)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,1,$10)
		ON CONFLICT (user_id, day, agent, model) DO UPDATE SET
			input       = usage_daily.input + EXCLUDED.input,
			output      = usage_daily.output + EXCLUDED.output,
			cached      = usage_daily.cached + EXCLUDED.cached,
			cache_write = usage_daily.cache_write + EXCLUDED.cache_write,
			reasoning   = usage_daily.reasoning + EXCLUDED.reasoning,
			calls       = usage_daily.calls + 1,
			failed      = usage_daily.failed + EXCLUDED.failed`,
		rec.UserID, DayKey(rec.At), rec.Agent, rec.Model,
		rec.Input, rec.Output, rec.Cached, rec.CacheWrite, rec.Reasoning, failed); err != nil {
		return fmt.Errorf("record usage: %w", err)
	}

	if !withEvent {
		return nil
	}
	// The detail row goes through the same normalisation the rollup just did, so
	// the two agree about what a failed call cost.
	return s.AppendEvent(rec.UserID, EventFrom(rec))
}

// AppendEvent writes one per-call detail row. RecordUsage writes the rollup and
// the event together; this is the event on its own, which is what a deployment
// whose detail lives on a log plane calls instead.
func (s *postgresStore) AppendEvent(userID string, ev UsageEvent) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ok := 0
	if ev.OK {
		ok = 1
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_events
			(ts, user_id, agent, model, kind, input, output, cached, cache_write, reasoning, ok)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		ev.Ts, userID, ev.Agent, ev.Model, ev.Kind,
		ev.Input, ev.Output, ev.Cached, ev.CacheWrite, ev.Reasoning, ok); err != nil {
		return fmt.Errorf("record usage event: %w", err)
	}
	return nil
}

// PruneEvents drops per-call detail rows older than cutoff, reporting how many
// went — retention that deletes data says how much.
func (s *postgresStore) PruneEvents(before time.Time) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := s.db.ExecContext(ctx, `DELETE FROM usage_events WHERE ts < $1`, before.Unix())
	if err != nil {
		return 0, fmt.Errorf("prune usage events: %w", err)
	}
	n, err := res.RowsAffected()
	return n, err
}

// UsageForAgentDay returns one agent's totals for a UTC day, across every user
// it served — the figure a deployment-wide agent budget is measured against.
func (s *postgresStore) UsageForAgentDay(agent, day string) (UsageTotals, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var t UsageTotals
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(input),0), COALESCE(SUM(output),0), COALESCE(SUM(cached),0),
		       COALESCE(SUM(cache_write),0), COALESCE(SUM(reasoning),0), COALESCE(SUM(calls),0),
		       COALESCE(SUM(failed),0)
		FROM usage_daily WHERE agent = $1 AND day = $2`, agent, day).
		Scan(&t.Input, &t.Output, &t.Cached, &t.CacheWrite, &t.Reasoning, &t.Calls, &t.Failed)
	if err != nil {
		return UsageTotals{}, err
	}
	return t, nil
}

func (s *postgresStore) UsageForDay(userID, day string) (UsageTotals, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var t UsageTotals
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(input),0), COALESCE(SUM(output),0), COALESCE(SUM(cached),0),
		       COALESCE(SUM(cache_write),0), COALESCE(SUM(reasoning),0), COALESCE(SUM(calls),0),
		       COALESCE(SUM(failed),0)
		FROM usage_daily WHERE user_id = $1 AND day = $2`, userID, day).
		Scan(&t.Input, &t.Output, &t.Cached, &t.CacheWrite, &t.Reasoning, &t.Calls, &t.Failed)
	if err != nil {
		return UsageTotals{}, err
	}
	return t, nil
}

func (s *postgresStore) UsageDaily(day string) ([]UsageRow, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
		SELECT user_id, day, agent, model, input, output, cached, cache_write, reasoning, calls, failed
		FROM usage_daily WHERE day = $1
		ORDER BY (input + output) DESC, user_id`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanUsageRows(rows)
}

func (s *postgresStore) UsageHistory(userID string, days int) ([]UsageRow, error) {
	if days <= 0 || days > 90 {
		days = 7
	}
	from := DayKey(time.Now().AddDate(0, 0, -(days - 1)))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
		SELECT user_id, day, agent, model, input, output, cached, cache_write, reasoning, calls, failed
		FROM usage_daily WHERE user_id = $1 AND day >= $2
		ORDER BY day DESC, model`, userID, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanUsageRows(rows)
}

func (s *postgresStore) UsageEvents(userID string, since time.Time, limit int) ([]UsageEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, kind, model, agent, input, output, cached, cache_write, reasoning, ok
		FROM usage_events WHERE user_id = $1 AND ts >= $2
		ORDER BY ts DESC LIMIT $3`, userID, since.Unix(), limit)
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

func (s *postgresStore) PruneUsage(before time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM usage_daily WHERE day < $1`, DayKey(before)); err != nil {
		return err
	}
	_, err := s.PruneEvents(before)
	return err
}

// --- helpers ----------------------------------------------------------------

func scanUsageRows(rows *sql.Rows) ([]UsageRow, error) {
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

// likeEscape neutralizes the LIKE metacharacters in a prefix. Scope keys embed
// user ids like "u_1", and "_" is a single-character wildcard — unescaped, a
// prefix scan would match rows outside the scope it was asked for.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// nullIfEmpty stores an empty string as NULL, matching what the SQLite store's
// nullable columns mean so both backends read back the same shape.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
