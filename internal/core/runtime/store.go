// Package runtime owns the runtime-state SQLite store for scheduler metadata,
// budget accounting, and child lifecycle records. It is separate from the
// memory backend: the memory provider contract is for agent-retrieved records;
// the runtime store is for core-owned operational state.
package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the runtime-state SQLite database.
type Store struct {
	db   *sql.DB
	path string
}

// Open creates or opens a runtime store at the given path.
func Open(path string) (*Store, error) {
	if path == "" {
		path = "./data/agentflow-runtime.db"
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	dir := filepath.Dir(abs)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", abs)
	if err != nil {
		return nil, fmt.Errorf("open runtime store %s: %w", abs, err)
	}
	if _, err := db.Exec(`
		PRAGMA journal_mode = WAL;
		PRAGMA busy_timeout = 5000;
	`); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db, path: abs}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS budget_usage (
			owner TEXT NOT NULL,
			day TEXT NOT NULL,
			used INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (owner, day)
		);
		CREATE TABLE IF NOT EXISTS child_meta (
			session_id TEXT PRIMARY KEY,
			agent TEXT NOT NULL,
			parent_id TEXT,
			profile TEXT,
			created_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS timer_meta (
			timer_id TEXT PRIMARY KEY,
			owner TEXT NOT NULL,
			kind TEXT NOT NULL,
			next_fire INTEGER
		);
	`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// --- budget ---

// RecordBudgetUsage persists a day's token usage for a session owner.
func (s *Store) RecordBudgetUsage(ctx context.Context, owner string, day string, used int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO budget_usage (owner, day, used) VALUES (?, ?, ?)
		 ON CONFLICT(owner, day) DO UPDATE SET used = excluded.used`,
		owner, day, used)
	return err
}

// GetBudgetUsage returns the recorded usage for an owner on a given day.
func (s *Store) GetBudgetUsage(ctx context.Context, owner string, day string) (int64, error) {
	var used int64
	err := s.db.QueryRowContext(ctx,
		`SELECT used FROM budget_usage WHERE owner = ? AND day = ?`,
		owner, day).Scan(&used)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return used, err
}

// --- child metadata ---

type ChildMeta struct {
	SessionID string
	Agent     string
	ParentID  string
	Profile   string
	CreatedAt int64
}

// RecordChild persists a spawned child's metadata.
func (s *Store) RecordChild(ctx context.Context, m ChildMeta) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO child_meta (session_id, agent, parent_id, profile, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		m.SessionID, m.Agent, m.ParentID, m.Profile, m.CreatedAt)
	return err
}

// DeleteChild removes a child record (on exit).
func (s *Store) DeleteChild(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM child_meta WHERE session_id = ?`, sessionID)
	return err
}

// ListChildren returns all live children of a parent.
func (s *Store) ListChildren(ctx context.Context, parentID string) ([]ChildMeta, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT session_id, agent, parent_id, profile, created_at
		 FROM child_meta WHERE parent_id = ?`, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChildMeta
	for rows.Next() {
		var m ChildMeta
		if err := rows.Scan(&m.SessionID, &m.Agent, &m.ParentID, &m.Profile, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// --- timer metadata ---

type TimerMeta struct {
	TimerID  string
	Owner    string
	Kind     string
	NextFire int64
}

// RecordTimer persists a timer's metadata.
func (s *Store) RecordTimer(ctx context.Context, m TimerMeta) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO timer_meta (timer_id, owner, kind, next_fire)
		 VALUES (?, ?, ?, ?)`,
		m.TimerID, m.Owner, m.Kind, m.NextFire)
	return err
}

// DeleteTimer removes a timer record.
func (s *Store) DeleteTimer(ctx context.Context, timerID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM timer_meta WHERE timer_id = ?`, timerID)
	return err
}

// DeleteTimersByOwner removes all timers owned by a session (on exit).
func (s *Store) DeleteTimersByOwner(ctx context.Context, owner string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM timer_meta WHERE owner = ?`, owner)
	return err
}

// Path returns the database path (for logging).
func (s *Store) Path() string { return s.path }
