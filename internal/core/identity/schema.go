package identity

import (
	"context"
	"time"

	"agentflow/internal/core/storedb"
)

// schema is the identity layer's tables, written once for both backends.
//
// Types are the portable subset: TEXT for strings and ids, BIGINT for unix
// seconds (PostgreSQL's INTEGER is 32-bit and would run out in 2038), INTEGER
// for the 0/1 flags — SQLite gives BIGINT and INTEGER the same affinity, so one
// schema serves a local file and a shared server.
//
// There are no ALTER migrations. A SQLite database written by an earlier
// release has the same columns with the same affinities; a PostgreSQL store is
// new here, so there is nothing to upgrade.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS profiles (
		user_id        TEXT PRIMARY KEY,
		display_name   TEXT NOT NULL DEFAULT '',
		email          TEXT NOT NULL DEFAULT '',
		tokens_per_day BIGINT NOT NULL DEFAULT 0,
		created_at     BIGINT NOT NULL,
		updated_at     BIGINT NOT NULL
	);`,
	`CREATE TABLE IF NOT EXISTS identities (
		id          TEXT PRIMARY KEY,
		user_id     TEXT NOT NULL DEFAULT '',
		channel     TEXT NOT NULL,
		native_from TEXT NOT NULL UNIQUE,
		reply_to    TEXT NOT NULL DEFAULT '',
		username    TEXT NOT NULL DEFAULT '',
		name        TEXT NOT NULL DEFAULT '',
		trust       TEXT NOT NULL DEFAULT 'asserted',
		deliverable INTEGER NOT NULL DEFAULT 0,
		linkable    INTEGER NOT NULL DEFAULT 0,
		first_seen  BIGINT NOT NULL,
		last_seen   BIGINT NOT NULL
	);`,
	`CREATE INDEX IF NOT EXISTS idx_identities_user ON identities(user_id);`,
	`CREATE TABLE IF NOT EXISTS user_tokens (
		token_hash TEXT PRIMARY KEY,
		user_id    TEXT NOT NULL,
		created_at BIGINT NOT NULL,
		last_used  BIGINT NOT NULL DEFAULT 0,
		revoked    INTEGER NOT NULL DEFAULT 0
	);`,
	`CREATE INDEX IF NOT EXISTS idx_user_tokens_user ON user_tokens(user_id);`,
	`CREATE TABLE IF NOT EXISTS link_challenges (
		code_hash  TEXT PRIMARY KEY,
		user_id    TEXT NOT NULL,
		channel    TEXT NOT NULL,
		created_at BIGINT NOT NULL,
		expires_at BIGINT NOT NULL,
		consumed   INTEGER NOT NULL DEFAULT 0
	);`,
	`CREATE TABLE IF NOT EXISTS invites (
		code_hash   TEXT PRIMARY KEY,
		created_at  BIGINT NOT NULL,
		expires_at  BIGINT NOT NULL,
		redeemed_by TEXT NOT NULL DEFAULT '',
		redeemed_at BIGINT NOT NULL DEFAULT 0
	);`,
}

// migrate creates the schema and clears what is already dead: an expired
// challenge or an unredeemed expired invite is weight carried for the life of
// the database otherwise.
func (r *Registry) migrate(ctx context.Context) error {
	if err := r.st.ExecDDL(ctx, schema...); err != nil {
		return err
	}
	now := time.Now().Unix()
	for _, stmt := range []string{
		`DELETE FROM link_challenges WHERE expires_at < ?`,
		`DELETE FROM invites WHERE expires_at < ? AND redeemed_by = ''`,
	} {
		if _, err := r.st.Exec(ctx, stmt, now); err != nil {
			return err
		}
	}
	return r.migrateLegacyUsers(ctx)
}

// migrateLegacyUsers folds the pre-profile `users` table into profiles and
// identities. Each legacy row becomes a profile whose id IS the old uuid — so
// every scoped key minted before this release (memory, files, credentials)
// stays valid without moving a byte — plus the identity that produced it.
//
// The copy is INSERT OR IGNORE per row and the drop happens only after every
// row is copied, so a crash mid-migration is repaired by the next boot rather
// than losing rows.
//
// This is a SQLite-only path by construction: `users` was a table in the local
// file an earlier release wrote, and a shared store starts empty.
func (r *Registry) migrateLegacyUsers(ctx context.Context) error {
	if r.st.Backend() != storedb.BackendSQLite {
		return nil
	}
	var exists int
	err := r.st.QueryRow(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'users'`).Scan(&exists)
	if err != nil || exists == 0 {
		return err
	}
	rows, err := r.st.Query(ctx,
		`SELECT uuid, native_from, channel, reply_to, username, name, first_seen, last_seen FROM users`)
	if err != nil {
		return err
	}
	type legacy struct {
		uuid, nativeFrom, channel, replyTo, username, name string
		firstSeen, lastSeen                                int64
	}
	var all []legacy
	for rows.Next() {
		var l legacy
		if err := rows.Scan(&l.uuid, &l.nativeFrom, &l.channel, &l.replyTo,
			&l.username, &l.name, &l.firstSeen, &l.lastSeen); err != nil {
			rows.Close()
			return err
		}
		all = append(all, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, l := range all {
		if _, err := r.st.Exec(ctx,
			`INSERT OR IGNORE INTO profiles (user_id, created_at, updated_at) VALUES (?, ?, ?)`,
			l.uuid, l.firstSeen, l.lastSeen); err != nil {
			return err
		}
		t := Traits(l.channel)
		if _, err := r.st.Exec(ctx,
			`INSERT OR IGNORE INTO identities
			   (id, user_id, channel, native_from, reply_to, username, name,
			    trust, deliverable, linkable, first_seen, last_seen)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			l.uuid, l.uuid, l.channel, l.nativeFrom, l.replyTo, l.username, l.name,
			t.Trust, boolInt(t.Deliverable), boolInt(t.Linkable), l.firstSeen, l.lastSeen); err != nil {
			return err
		}
	}
	if _, err := r.st.Exec(ctx, `DROP TABLE users`); err != nil {
		return err
	}
	if n := len(all); n > 0 {
		r.log.Info("identity: migrated legacy users to profiles", "count", n)
	}
	return nil
}
