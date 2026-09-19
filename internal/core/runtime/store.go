// Package runtime owns the runtime-state SQLite store for the core-owned
// message journal (the audit trail of every inbound/outbound message). It is
// separate from the memory backend: the memory provider contract is for
// agent-retrieved records; the runtime store is for core-owned operational
// state.
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
		CREATE TABLE IF NOT EXISTS message_journal (
			id TEXT NOT NULL,
			ts INTEGER NOT NULL,
			direction TEXT NOT NULL,
			status TEXT NOT NULL,
			channel TEXT,
			chat TEXT,
			sender TEXT,
			agent TEXT,
			session_id TEXT,
			type TEXT,
			text TEXT,
			attachments_json TEXT,
			provenance_json TEXT,
			err TEXT
		);
		CREATE INDEX IF NOT EXISTS message_journal_id ON message_journal (id);
		CREATE INDEX IF NOT EXISTS message_journal_ts ON message_journal (ts);
		CREATE INDEX IF NOT EXISTS message_journal_session ON message_journal (session_id);
	`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// Path returns the database path (for logging).
func (s *Store) Path() string { return s.path }
