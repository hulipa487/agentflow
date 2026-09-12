// Package identity mints and resolves stable user identities for inbound
// channel traffic. A user is a first-class addressable entity: every inbound
// message has its native From rewritten to "user:<uuid>" before the router
// sees it, so the route plugin and loops become channel-agnostic. The
// registry persists the uuid→{channel,reply_to} mapping so proactive push
// (session.push_user) can resolve a UUID back to a delivery target across
// restarts.
//
// It is an opt-in layer: when disabled, channel drivers submit to the router
// directly and From stays channel-native, exactly as before.
package identity

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// UserResolver resolves a user UUID to a delivery target. The actor uses it
// to implement session.push_user.
type UserResolver interface {
	LookupUser(uuid string) (channel, replyTo string, ok bool)
}

// Contact is a resolved user's delivery info.
type Contact struct {
	UUID     string
	Channel  string
	ReplyTo  string
	Username string
	Name     string
}

// Registry mints and resolves user identities, backed by a sqlite table.
// Mints are keyed by native_from (the channel driver's original From string,
// e.g. "user:telegram:123"), which is already a stable per-(channel, human)
// value — so identity is established without any driver change.
type Registry struct {
	db   *sql.DB
	log  *slog.Logger
	mu   sync.Mutex
	cache map[string]string // native_from → uuid (hit path avoids a DB read)
}

// Open creates the registry, opening (or creating) the sqlite database at
// path and running the schema migration.
func Open(path string, log *slog.Logger) (*Registry, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("identity: open %s: %w", path, err)
	}
	if _, err := db.Exec(`
		PRAGMA journal_mode = WAL;
		PRAGMA busy_timeout = 5000;
		PRAGMA foreign_keys = ON;
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("identity: pragma: %w", err)
	}
	r := &Registry{db: db, log: log.With("module", "identity"), cache: map[string]string{}}
	if err := r.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("identity: migrate: %w", err)
	}
	return r, nil
}

func (r *Registry) migrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := r.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS users (
			uuid         TEXT PRIMARY KEY,
			native_from  TEXT NOT NULL UNIQUE,
			channel      TEXT NOT NULL,
			reply_to     TEXT NOT NULL DEFAULT '',
			username     TEXT NOT NULL DEFAULT '',
			name         TEXT NOT NULL DEFAULT '',
			first_seen   INTEGER NOT NULL,
			last_seen    INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_users_native ON users(native_from);
	`)
	return err
}

// Close closes the underlying database.
func (r *Registry) Close() error { return r.db.Close() }

// Resolve returns the UUID for a native_from key, minting one on first
// contact. It refreshes the stored delivery target + profile fields on
// every call (a user may move between chats, or a webhook caller's reply
// target changes per request). Concurrent first-contacts for the same key
// are serialized so exactly one UUID is minted.
func (r *Registry) Resolve(channel, nativeFrom, replyTo string, profile map[string]any) (string, error) {
	// Fast path: cache hit. Refresh outside the lock (refresh does its own
	// DB write and doesn't touch the cache).
	r.mu.Lock()
	if uuid, ok := r.cache[nativeFrom]; ok {
		r.mu.Unlock()
		r.refresh(uuid, channel, replyTo, profile)
		return uuid, nil
	}
	r.mu.Unlock()

	// Slow path: serialize minting. The mutex is coarse (one mint at a time
	// across all keys); minting is rare (first contact only) and a finer lock
	// would buy nothing for the load profile.
	r.mu.Lock()
	// Re-check under the lock — another goroutine may have minted meanwhile.
	if uuid, ok := r.cache[nativeFrom]; ok {
		r.mu.Unlock()
		r.refresh(uuid, channel, replyTo, profile)
		return uuid, nil
	}
	// Look up an existing row (e.g. after a restart, before the cache is warm).
	uuid, ok, err := r.lookup(nativeFrom)
	if err != nil {
		r.mu.Unlock()
		return "", err
	}
	if !ok {
		uuid, err = r.mint(nativeFrom, channel, replyTo, profile)
		if err != nil {
			r.mu.Unlock()
			return "", err
		}
	}
	r.cache[nativeFrom] = uuid
	r.mu.Unlock()
	r.refresh(uuid, channel, replyTo, profile)
	return uuid, nil
}

func (r *Registry) lookup(nativeFrom string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var uuid string
	err := r.db.QueryRowContext(ctx,
		`SELECT uuid FROM users WHERE native_from = ?`, nativeFrom).Scan(&uuid)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return uuid, true, nil
}

func (r *Registry) mint(nativeFrom, channel, replyTo string, profile map[string]any) (string, error) {
	uuid := "u_" + randomID(12)
	now := time.Now().Unix()
	username, name := profileStrings(profile)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO users (uuid, native_from, channel, reply_to, username, name, first_seen, last_seen)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid, nativeFrom, channel, replyTo, username, name, now, now)
	if err != nil {
		return "", fmt.Errorf("mint user: %w", err)
	}
	r.log.Info("user minted", "uuid", uuid, "native_from", nativeFrom, "channel", channel)
	return uuid, nil
}

// refresh updates the delivery target and profile fields for a known user.
// It is best-effort: a failure here only means the next push may use a stale
// target, which the outbox layer handles. Errors are logged, not returned.
func (r *Registry) refresh(uuid, channel, replyTo string, profile map[string]any) {
	if uuid == "" {
		return
	}
	username, name := profileStrings(profile)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.db.ExecContext(ctx,
		`UPDATE users SET channel = ?, reply_to = ?, username = ?, name = ?, last_seen = ? WHERE uuid = ?`,
		channel, replyTo, username, name, time.Now().Unix(), uuid); err != nil {
		r.log.Warn("refresh user failed", "uuid", uuid, "err", err)
	}
}

// LookupUser resolves a UUID to its delivery target for proactive push.
func (r *Registry) LookupUser(uuid string) (channel, replyTo string, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := r.db.QueryRowContext(ctx,
		`SELECT channel, reply_to FROM users WHERE uuid = ?`, uuid).Scan(&channel, &replyTo)
	if err == sql.ErrNoRows {
		return "", "", false
	}
	if err != nil {
		r.log.Warn("lookup user failed", "uuid", uuid, "err", err)
		return "", "", false
	}
	return channel, replyTo, true
}

func profileStrings(p map[string]any) (username, name string) {
	if p == nil {
		return "", ""
	}
	if v, ok := p["username"]; ok {
		username = fmt.Sprint(v)
	}
	if v, ok := p["name"]; ok {
		name = fmt.Sprint(v)
	}
	return username, name
}

// randomID returns a lowercase hex string of n bytes (2n chars). crypto/rand
// keeps UUIDs unguessable so a forged "user:<uuid>" push target cannot be
// guessed without first observing it on an inbound.
func randomID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
