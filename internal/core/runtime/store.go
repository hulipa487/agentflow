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
			user_uuid TEXT,
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

		-- Engine-owned key/value rows for the user-scoped file store (working
		-- tree, commit, ref, scratch records). Same inspectability as the
		-- journal: plain sqlite rows, no memory-provider layering.
		CREATE TABLE IF NOT EXISTS files_meta (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL DEFAULT 0
		);
	`+usageSchema)
	if err != nil {
		return err
	}
	// user_uuid arrived after the journal shipped, so an existing database
	// needs an explicit ALTER — and its index can only be created once
	// the column is there.
	if err := s.addColumnIfMissing(ctx, "message_journal", "user_uuid", "TEXT"); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS message_journal_user ON message_journal (user_uuid)`)
	return err
}

// addColumnIfMissing adds a column to an existing table. It is the migration
// path for columns introduced after a table shipped: CREATE TABLE IF NOT EXISTS
// silently does nothing on an existing table, so a new column needs this.
func (s *Store) addColumnIfMissing(ctx context.Context, table, column, typ string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column+` `+typ)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// Path returns the database path (for logging).
func (s *Store) Path() string { return s.path }
