// Package identity mints and resolves stable user identities for inbound
// channel traffic, and owns the profile they belong to.
//
// A profile is a person; an identity is one (channel, handle) pair they
// arrive on. Every inbound resolves to an identity, and the engine stamps the
// *profile* id as the user scope — so a person's memory, files and credentials
// follow them across channels once their handles are linked.
//
// An identity that is not linked to a profile carries no user scope at all:
// its inbound runs in the shared service stratum until it is linked, so
// nothing personal accumulates under a handle nobody has claimed. Linking is
// proven by possession — the users API hands a challenge to the profile owner
// and the user echoes it back from the handle itself.
//
// The registry persists profiles, identities and their delivery targets so
// uuid→{channel,reply_to} resolution survives restarts for proactive push
// (session.push_user).
//
// It is an opt-in layer: when disabled, channel drivers submit to the router
// directly and From stays channel-native, exactly as before.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"agentflow/internal/core/metrics"

	_ "modernc.org/sqlite"
)

// UserResolver resolves a user to a delivery target. The actor uses it to
// implement session.push_user.
type UserResolver interface {
	LookupUser(uuid string) (channel, replyTo string, ok bool)
}

// Trust levels describing what a channel vouches for.
const (
	// TrustVerified: the platform authenticates the sender id, so a message
	// from a handle is proof of possession (telegram's From.ID).
	TrustVerified = "verified"
	// TrustAsserted: the sender string is supplied by the caller and vouches
	// for nothing. Such handles cannot be linked, and their traffic never
	// carries a user scope.
	TrustAsserted = "asserted"
	// TrustSystem: a machine integration rather than a person — every sender
	// collapses onto one handle (ghhook).
	TrustSystem = "system"
)

// ChannelTraits describes what a channel can vouch for.
type ChannelTraits struct {
	Trust       string
	Deliverable bool // can the engine address this sender later?
	Linkable    bool // may a handle here be linked to a profile?
}

// channelDefaults is the per-driver table of traits. An unknown channel gets
// the most conservative treatment: asserted, undeliverable, unlinkable.
var channelDefaults = map[string]ChannelTraits{
	"telegram": {Trust: TrustVerified, Deliverable: true, Linkable: true},
	"webhook":  {Trust: TrustAsserted, Deliverable: false, Linkable: false},
	"ghhook":   {Trust: TrustSystem, Deliverable: false, Linkable: false},
}

// Traits returns the traits for a channel name.
func Traits(channel string) ChannelTraits {
	if t, ok := channelDefaults[channel]; ok {
		return t
	}
	return ChannelTraits{Trust: TrustAsserted}
}

// Identity is one (channel, handle) pair a person arrives on.
type Identity struct {
	ID          string `json:"id"`
	UserID      string `json:"user_id"` // "" until linked to a profile
	Channel     string `json:"channel"`
	NativeFrom  string `json:"native_from"`
	ReplyTo     string `json:"reply_to,omitempty"`
	Username    string `json:"username,omitempty"`
	Name        string `json:"name,omitempty"`
	Trust       string `json:"trust"`
	Deliverable bool   `json:"deliverable"`
	Linkable    bool   `json:"linkable"`
	FirstSeen   int64  `json:"first_seen"`
	LastSeen    int64  `json:"last_seen"`
}

// Linked reports whether this handle belongs to a profile.
func (i Identity) Linked() bool { return i.UserID != "" }

// Profile is a person: the account their identities hang off.
type Profile struct {
	UserID       string     `json:"user_id"`
	DisplayName  string     `json:"display_name,omitempty"`
	Email        string     `json:"email,omitempty"`
	TokensPerDay int64      `json:"tokens_per_day"` // 0 = the deployment default
	CreatedAt    int64      `json:"created_at"`
	UpdatedAt    int64      `json:"updated_at"`
	Identities   []Identity `json:"identities"`
}

// Resolution is what one inbound resolves to: the handle it came from and the
// profile (when linked) it belongs to.
type Resolution struct {
	IdentityID  string
	UserID      string // stamped as the user scope; "" = unregistered
	Channel     string
	Trust       string
	Deliverable bool
	Linkable    bool
}

// Registered reports whether this inbound carries a user scope.
func (r Resolution) Registered() bool { return r.UserID != "" }

// Registry mints and resolves user identities, and owns their profiles,
// backed by a sqlite database.
type Registry struct {
	db    *sql.DB
	log   *slog.Logger
	mu    sync.Mutex
	cache map[string]Resolution // native_from → resolution
	// autoClaim restores pre-registration behavior: a first contact claims a
	// profile, so the handle gets a personal scope immediately. Default false —
	// unknown handles stay in the shared service stratum until linked.
	autoClaim bool
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
	r := &Registry{db: db, log: log.With("module", "identity"), cache: map[string]Resolution{}}
	if err := r.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("identity: migrate: %w", err)
	}
	return r, nil
}

// SetAutoClaim selects what an unlinked first contact becomes: a claimed
// profile (true, the pre-registration behavior) or an unregistered identity
// carrying no user scope (false, the default).
func (r *Registry) SetAutoClaim(v bool) {
	r.mu.Lock()
	r.autoClaim = v
	r.mu.Unlock()
}

func (r *Registry) migrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS profiles (
			user_id        TEXT PRIMARY KEY,
			display_name   TEXT NOT NULL DEFAULT '',
			email          TEXT NOT NULL DEFAULT '',
			tokens_per_day INTEGER NOT NULL DEFAULT 0,
			created_at     INTEGER NOT NULL,
			updated_at     INTEGER NOT NULL
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
			first_seen  INTEGER NOT NULL,
			last_seen   INTEGER NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_identities_user ON identities(user_id);`,
		`CREATE TABLE IF NOT EXISTS user_tokens (
			token_hash TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			last_used  INTEGER NOT NULL DEFAULT 0,
			revoked    INTEGER NOT NULL DEFAULT 0
		);`,
		`CREATE INDEX IF NOT EXISTS idx_user_tokens_user ON user_tokens(user_id);`,
		`CREATE TABLE IF NOT EXISTS link_challenges (
			code_hash  TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL,
			channel    TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			consumed   INTEGER NOT NULL DEFAULT 0
		);`,
		`CREATE TABLE IF NOT EXISTS invites (
			code_hash   TEXT PRIMARY KEY,
			created_at  INTEGER NOT NULL,
			expires_at  INTEGER NOT NULL,
			redeemed_by TEXT NOT NULL DEFAULT '',
			redeemed_at INTEGER NOT NULL DEFAULT 0
		);`,
	} {
		if _, err := r.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	// Expired challenges and unredeemed invites are dead weight; drop them at
	// boot rather than carrying them for the life of the database.
	now := time.Now().Unix()
	for _, stmt := range []string{
		`DELETE FROM link_challenges WHERE expires_at < ?`,
		`DELETE FROM invites WHERE expires_at < ? AND redeemed_by = ''`,
	} {
		if _, err := r.db.ExecContext(ctx, stmt, now); err != nil {
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
func (r *Registry) migrateLegacyUsers(ctx context.Context) error {
	var exists int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'users'`).Scan(&exists)
	if err != nil || exists == 0 {
		return err
	}
	rows, err := r.db.QueryContext(ctx,
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
		if _, err := r.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO profiles (user_id, created_at, updated_at) VALUES (?, ?, ?)`,
			l.uuid, l.firstSeen, l.lastSeen); err != nil {
			return err
		}
		t := Traits(l.channel)
		if _, err := r.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO identities
			   (id, user_id, channel, native_from, reply_to, username, name,
			    trust, deliverable, linkable, first_seen, last_seen)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			l.uuid, l.uuid, l.channel, l.nativeFrom, l.replyTo, l.username, l.name,
			t.Trust, boolInt(t.Deliverable), boolInt(t.Linkable), l.firstSeen, l.lastSeen); err != nil {
			return err
		}
	}
	if _, err := r.db.ExecContext(ctx, `DROP TABLE users`); err != nil {
		return err
	}
	if n := len(all); n > 0 {
		r.log.Info("identity: migrated legacy users to profiles", "count", n)
	}
	return nil
}

// Close closes the underlying database.
func (r *Registry) Close() error { return r.db.Close() }

// Resolve returns the identity (and profile, when linked) behind an inbound,
// creating the identity on first contact. It refreshes the stored delivery
// target + profile fields on every call, since a user may move between chats.
// Concurrent first-contacts for the same key are serialized so exactly one
// identity is minted.
func (r *Registry) Resolve(channel, nativeFrom, replyTo string, profile map[string]any) (Resolution, error) {
	// Fast path: cache hit. Refresh outside the lock (refresh does its own DB
	// write and does not touch the cache).
	r.mu.Lock()
	if res, ok := r.cache[nativeFrom]; ok {
		r.mu.Unlock()
		r.refresh(res.IdentityID, replyTo, profile)
		return res, nil
	}
	r.mu.Unlock()

	// Slow path: serialize minting. The mutex is coarse (one mint at a time
	// across all keys); minting is rare (first contact only) and a finer lock
	// would buy nothing for the load profile.
	r.mu.Lock()
	defer r.mu.Unlock()
	if res, ok := r.cache[nativeFrom]; ok {
		r.refresh(res.IdentityID, replyTo, profile)
		return res, nil
	}
	id, ok, err := r.lookupIdentity(nativeFrom)
	if err != nil {
		return Resolution{}, err
	}
	if !ok {
		return r.mintIdentity(nativeFrom, channel, replyTo, profile)
	}
	res := id.resolution()
	r.cache[nativeFrom] = res
	r.refresh(id.ID, replyTo, profile)
	return res, nil
}

// resolution projects a stored identity into the per-inbound view.
func (i Identity) resolution() Resolution {
	return Resolution{
		IdentityID:  i.ID,
		UserID:      i.UserID,
		Channel:     i.Channel,
		Trust:       i.Trust,
		Deliverable: i.Deliverable,
		Linkable:    i.Linkable,
	}
}

const identityCols = `id, user_id, channel, native_from, reply_to, username, name,
	trust, deliverable, linkable, first_seen, last_seen`

func (r *Registry) lookupIdentity(nativeFrom string) (Identity, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	row := r.db.QueryRowContext(ctx,
		`SELECT `+identityCols+` FROM identities WHERE native_from = ?`, nativeFrom)
	id, err := scanIdentity(row)
	if err == sql.ErrNoRows {
		return Identity{}, false, nil
	}
	if err != nil {
		return Identity{}, false, err
	}
	return id, true, nil
}

// rowScanner is satisfied by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanIdentity(s rowScanner) (Identity, error) {
	var id Identity
	var deliverable, linkable int
	if err := s.Scan(&id.ID, &id.UserID, &id.Channel, &id.NativeFrom, &id.ReplyTo,
		&id.Username, &id.Name, &id.Trust, &deliverable, &linkable,
		&id.FirstSeen, &id.LastSeen); err != nil {
		return Identity{}, err
	}
	id.Deliverable = deliverable != 0
	id.Linkable = linkable != 0
	return id, nil
}

// mintIdentity creates the identity row for a first contact. With autoClaim it
// also claims a profile, so the handle gets a personal scope immediately;
// otherwise the identity stays unregistered and carries no user scope.
func (r *Registry) mintIdentity(nativeFrom, channel, replyTo string, profile map[string]any) (Resolution, error) {
	now := time.Now().Unix()
	username, name := profileStrings(profile)
	t := Traits(channel)
	userID := ""
	if r.autoClaim {
		userID = "u_" + randomID(12)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := r.db.ExecContext(ctx,
			`INSERT INTO profiles (user_id, created_at, updated_at) VALUES (?, ?, ?)`,
			userID, now, now); err != nil {
			return Resolution{}, fmt.Errorf("claim profile: %w", err)
		}
	}

	id := Identity{
		ID: "i_" + randomID(12), UserID: userID, Channel: channel,
		NativeFrom: nativeFrom, ReplyTo: replyTo, Username: username, Name: name,
		Trust: t.Trust, Deliverable: t.Deliverable, Linkable: t.Linkable,
		FirstSeen: now, LastSeen: now,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO identities (`+identityCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id.ID, id.UserID, id.Channel, id.NativeFrom, id.ReplyTo, id.Username, id.Name,
		id.Trust, boolInt(id.Deliverable), boolInt(id.Linkable), id.FirstSeen, id.LastSeen); err != nil {
		return Resolution{}, fmt.Errorf("mint identity: %w", err)
	}
	res := id.resolution()
	r.cache[nativeFrom] = res
	metrics.Inc("agentflow_identity_mints")
	r.log.Info("identity minted", "identity", id.ID, "user_id", userID,
		"native_from", nativeFrom, "channel", channel, "registered", userID != "")
	return res, nil
}

// refresh updates the delivery target and profile fields for a known identity.
// It is best-effort: a failure here only means the next push may use a stale
// target, which the outbox layer handles. Errors are logged, not returned.
func (r *Registry) refresh(identityID, replyTo string, profile map[string]any) {
	if identityID == "" {
		return
	}
	username, name := profileStrings(profile)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.db.ExecContext(ctx,
		`UPDATE identities SET reply_to = ?, username = ?, name = ?, last_seen = ?
		 WHERE id = ?`,
		replyTo, username, name, time.Now().Unix(), identityID); err != nil {
		r.log.Warn("refresh identity failed", "identity", identityID, "err", err)
	}
}

// --- profiles ---------------------------------------------------------------

// CreateProfile registers a new profile. It is the registration primitive
// behind the users API; the returned profile has no identities yet.
func (r *Registry) CreateProfile(displayName, email string) (Profile, error) {
	now := time.Now().Unix()
	p := Profile{
		UserID:      "u_" + randomID(12),
		DisplayName: displayName,
		Email:       email,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO profiles (user_id, display_name, email, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		p.UserID, p.DisplayName, p.Email, p.CreatedAt, p.UpdatedAt); err != nil {
		return Profile{}, fmt.Errorf("create profile: %w", err)
	}
	return p, nil
}

// Get returns a profile with its linked identities.
func (r *Registry) Get(userID string) (Profile, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var p Profile
	err := r.db.QueryRowContext(ctx,
		`SELECT user_id, display_name, email, tokens_per_day, created_at, updated_at
		 FROM profiles WHERE user_id = ?`, userID).
		Scan(&p.UserID, &p.DisplayName, &p.Email, &p.TokensPerDay, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return Profile{}, false, nil
	}
	if err != nil {
		return Profile{}, false, err
	}
	ids, err := r.Identities(userID)
	if err != nil {
		return Profile{}, false, err
	}
	p.Identities = ids
	return p, true, nil
}

// List returns every profile ordered by creation, each with its identities.
func (r *Registry) List() ([]Profile, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := r.db.QueryContext(ctx,
		`SELECT user_id, display_name, email, tokens_per_day, created_at, updated_at
		 FROM profiles ORDER BY created_at, user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Profile
	for rows.Next() {
		var p Profile
		if err := rows.Scan(&p.UserID, &p.DisplayName, &p.Email, &p.TokensPerDay,
			&p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		ids, err := r.Identities(out[i].UserID)
		if err != nil {
			return nil, err
		}
		out[i].Identities = ids
	}
	return out, nil
}

// Update sets the mutable profile fields; a nil argument leaves that field
// unchanged.
func (r *Registry) Update(userID string, displayName, email *string, tokensPerDay *int64) error {
	sets := []string{}
	args := []any{}
	if displayName != nil {
		sets = append(sets, "display_name = ?")
		args = append(args, *displayName)
	}
	if email != nil {
		sets = append(sets, "email = ?")
		args = append(args, *email)
	}
	if tokensPerDay != nil {
		sets = append(sets, "tokens_per_day = ?")
		args = append(args, *tokensPerDay)
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, time.Now().Unix(), userID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := r.db.ExecContext(ctx,
		`UPDATE profiles SET `+strings.Join(sets, ", ")+` WHERE user_id = ?`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no such profile %q", userID)
	}
	return nil
}

// Identities returns a profile's linked handles, oldest first.
func (r *Registry) Identities(userID string) ([]Identity, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+identityCols+` FROM identities WHERE user_id = ? ORDER BY first_seen, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		id, err := scanIdentity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// --- linking ----------------------------------------------------------------

// Link binds a handle to a profile. The handle must already exist (the person
// must have written in at least once) and must not belong to another profile;
// the caller is responsible for having proven possession of it.
func (r *Registry) Link(userID, nativeFrom string) (Identity, error) {
	id, ok, err := r.lookupIdentity(nativeFrom)
	if err != nil {
		return Identity{}, err
	}
	if !ok {
		return Identity{}, fmt.Errorf("no identity for %q: the handle must message the runtime before it can be linked", nativeFrom)
	}
	if id.UserID != "" && id.UserID != userID {
		return Identity{}, fmt.Errorf("handle %q already belongs to another profile", nativeFrom)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.db.ExecContext(ctx,
		`UPDATE identities SET user_id = ? WHERE id = ?`, userID, id.ID); err != nil {
		return Identity{}, err
	}
	id.UserID = userID
	r.evict(nativeFrom)
	r.log.Info("identity linked", "identity", id.ID, "user_id", userID, "channel", id.Channel)
	return id, nil
}

// Unlink detaches a handle from its profile, returning it to the unregistered
// stratum. It refuses to strip a profile of its last handle, which would leave
// the account unreachable.
func (r *Registry) Unlink(userID, identityID string) error {
	ids, err := r.Identities(userID)
	if err != nil {
		return err
	}
	found := false
	for _, id := range ids {
		if id.ID == identityID {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("no such identity %q on profile %q", identityID, userID)
	}
	if len(ids) == 1 {
		return fmt.Errorf("cannot unlink the last handle of profile %q", userID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var native string
	if err := r.db.QueryRowContext(ctx,
		`SELECT native_from FROM identities WHERE id = ?`, identityID).Scan(&native); err != nil {
		return err
	}
	if _, err := r.db.ExecContext(ctx,
		`UPDATE identities SET user_id = '' WHERE id = ?`, identityID); err != nil {
		return err
	}
	r.evict(native)
	r.log.Info("identity unlinked", "identity", identityID, "user_id", userID)
	return nil
}

// evict drops a cached resolution so the next inbound re-reads the link state.
func (r *Registry) evict(nativeFrom string) {
	r.mu.Lock()
	delete(r.cache, nativeFrom)
	r.mu.Unlock()
}

// --- delivery ---------------------------------------------------------------

// Targets returns a profile's deliverable handles, most recently seen first —
// the candidate set for proactive push.
func (r *Registry) Targets(userID string) ([]Identity, error) {
	ids, err := r.Identities(userID)
	if err != nil {
		return nil, err
	}
	out := make([]Identity, 0, len(ids))
	for _, id := range ids {
		if id.Deliverable {
			out = append(out, id)
		}
	}
	// Most recently seen first: push should reach a person where they last
	// spoke, not wherever the table happened to store.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].LastSeen > out[j-1].LastSeen; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// LookupUser resolves a profile id — or an unlinked handle id — to one
// delivery target, preferring the most recently seen deliverable handle. It
// satisfies UserResolver for session.push_user.
func (r *Registry) LookupUser(id string) (channel, replyTo string, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if targets, err := r.Targets(id); err == nil && len(targets) > 0 {
		return targets[0].Channel, targets[0].ReplyTo, true
	}
	// Not a profile (or it has no deliverable handle): fall back to treating
	// the id as a single identity.
	var ch, rt string
	var deliverable int
	err := r.db.QueryRowContext(ctx,
		`SELECT channel, reply_to, deliverable FROM identities WHERE id = ?`, id).
		Scan(&ch, &rt, &deliverable)
	if err == sql.ErrNoRows {
		return "", "", false
	}
	if err != nil {
		r.log.Warn("lookup identity failed", "id", id, "err", err)
		return "", "", false
	}
	if deliverable == 0 {
		return "", "", false // nowhere to deliver (webhook reply targets are transient)
	}
	return ch, rt, true
}

// --- user API tokens --------------------------------------------------------

// tokenPrefix marks a user API token so a leaked string is recognizable and
// revocable by identifier.
const tokenPrefix = "afu_"

// IssueToken mints a user API token. The plaintext is returned exactly once:
// only its SHA-256 hash is stored, so a copy of the database cannot be
// replayed as a credential.
func (r *Registry) IssueToken(userID string) (string, error) {
	tok := tokenPrefix + randomID(24)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO user_tokens (token_hash, user_id, created_at) VALUES (?, ?, ?)`,
		hashSecret(tok), userID, time.Now().Unix()); err != nil {
		return "", fmt.Errorf("issue token: %w", err)
	}
	return tok, nil
}

// TokenUser resolves a bearer token to its profile, if the token is live.
func (r *Registry) TokenUser(token string) (string, bool) {
	if !strings.HasPrefix(token, tokenPrefix) {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := hashSecret(token)
	var userID string
	var revoked int
	if err := r.db.QueryRowContext(ctx,
		`SELECT user_id, revoked FROM user_tokens WHERE token_hash = ?`, h).Scan(&userID, &revoked); err != nil {
		return "", false
	}
	if revoked != 0 {
		return "", false
	}
	// Best effort: a failed touch only costs accuracy on the console's
	// last-used column, never authority.
	_, _ = r.db.ExecContext(ctx, `UPDATE user_tokens SET last_used = ? WHERE token_hash = ?`, time.Now().Unix(), h)
	return userID, true
}

// RevokeToken revokes one of a profile's tokens. Scoping the update by user
// means a token cannot be revoked by anyone who merely knows its value.
func (r *Registry) RevokeToken(userID, token string) error {
	if token == "" {
		return fmt.Errorf("no token")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := r.db.ExecContext(ctx,
		`UPDATE user_tokens SET revoked = 1 WHERE token_hash = ? AND user_id = ?`,
		hashSecret(token), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no such token for this profile")
	}
	return nil
}

// Tokens returns the profile's live token metadata (never the tokens).
func (r *Registry) Tokens(userID string) ([]TokenRef, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := r.db.QueryContext(ctx,
		`SELECT substr(token_hash, 1, 8), created_at, last_used FROM user_tokens
		 WHERE user_id = ? AND revoked = 0 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenRef
	for rows.Next() {
		var t TokenRef
		if err := rows.Scan(&t.Fingerprint, &t.CreatedAt, &t.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TokenRef is the non-secret view of an issued token.
type TokenRef struct {
	Fingerprint string `json:"fingerprint"` // first 8 hex chars of the hash
	CreatedAt   int64  `json:"created_at"`
	LastUsed    int64  `json:"last_used"`
}

// --- channel linking --------------------------------------------------------

// StartLink creates a single-use challenge for linking a handle on channel.
// The plaintext code is returned once (only its hash is stored) and the caller
// shows it to the profile owner, who proves possession by sending it from the
// handle they are linking.
func (r *Registry) StartLink(userID, channel string, ttl time.Duration) (string, int64, error) {
	if !Traits(channel).Linkable {
		return "", 0, fmt.Errorf("channel %q cannot be linked: its sender identity is not verifiable", channel)
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	if _, ok, err := r.Get(userID); err != nil {
		return "", 0, err
	} else if !ok {
		return "", 0, fmt.Errorf("no such profile %q", userID)
	}
	code := newCode()
	now := time.Now()
	expires := now.Add(ttl).Unix()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO link_challenges (code_hash, user_id, channel, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?)`,
		hashSecret(code), userID, channel, now.Unix(), expires); err != nil {
		return "", 0, fmt.Errorf("start link: %w", err)
	}
	return code, expires, nil
}

// ConsumeLink completes a link. The caller passes the channel and handle the
// code arrived from; the challenge must match both. Possession is the proof:
// only the profile owner sees the code, and only the handle's owner can send
// from it. The challenge is burned whether or not the link succeeds, so a code
// can never be replayed.
func (r *Registry) ConsumeLink(channel, nativeFrom, code string) (string, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return "", fmt.Errorf("empty link code")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := hashSecret(code)
	var userID, ch string
	var expires int64
	var consumed int
	err := r.db.QueryRowContext(ctx,
		`SELECT user_id, channel, expires_at, consumed FROM link_challenges WHERE code_hash = ?`, h).
		Scan(&userID, &ch, &expires, &consumed)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("unknown link code")
	}
	if err != nil {
		return "", err
	}
	if consumed != 0 {
		return "", fmt.Errorf("link code already used")
	}
	if time.Now().Unix() > expires {
		return "", fmt.Errorf("link code expired")
	}
	if ch != channel {
		return "", fmt.Errorf("this code is not valid for channel %q", channel)
	}
	// Compare-and-set: the update is the gate, so two concurrent attempts with
	// the same code cannot both proceed.
	res, err := r.db.ExecContext(ctx,
		`UPDATE link_challenges SET consumed = 1 WHERE code_hash = ? AND consumed = 0`, h)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", fmt.Errorf("link code already used")
	}
	if _, err := r.Link(userID, nativeFrom); err != nil {
		return "", err
	}
	return userID, nil
}

// --- registration invites ---------------------------------------------------

// IssueInvite mints a single-use registration code.
func (r *Registry) IssueInvite(ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	code := newCode() + newCode()
	now := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO invites (code_hash, created_at, expires_at) VALUES (?, ?, ?)`,
		hashSecret(code), now.Unix(), now.Add(ttl).Unix()); err != nil {
		return "", fmt.Errorf("issue invite: %w", err)
	}
	return code, nil
}

// RedeemInvite consumes an invite. It is a compare-and-set so exactly one
// registration can win, and the invite is burned even if the registration that
// follows fails — re-issuing is cheaper than a redeemable-after-crash code.
func (r *Registry) RedeemInvite(code string) error {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return fmt.Errorf("invite required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := hashSecret(code)
	var expires int64
	var redeemedBy string
	err := r.db.QueryRowContext(ctx,
		`SELECT expires_at, redeemed_by FROM invites WHERE code_hash = ?`, h).Scan(&expires, &redeemedBy)
	if err == sql.ErrNoRows {
		return fmt.Errorf("unknown invite code")
	}
	if err != nil {
		return err
	}
	if redeemedBy != "" {
		return fmt.Errorf("invite already used")
	}
	if time.Now().Unix() > expires {
		return fmt.Errorf("invite expired")
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE invites SET redeemed_by = 'pending', redeemed_at = ? WHERE code_hash = ? AND redeemed_by = ''`,
		time.Now().Unix(), h)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("invite already used")
	}
	return nil
}

// LimitFor returns a profile's own daily token limit (0 = inherit the
// deployment default). It exists so the accounting quota can resolve per-user
// limits through a function value, without importing this package.
func (r *Registry) LimitFor(userID string) (int64, error) {
	p, ok, err := r.Get(userID)
	if err != nil || !ok {
		return 0, err
	}
	return p.TokensPerDay, nil
}

// --- helpers ----------------------------------------------------------------

// hashSecret is the at-rest form of every bearer secret (tokens, link codes,
// invite codes): a stored hash cannot be replayed as the secret itself.
func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// codeAlphabet omits I/O/0/1 so a code can be read aloud or retyped without
// ambiguity.
const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// newCode returns an 8-character single-use code. The alphabet size divides
// 256, so the modulo introduces no bias.
func newCode() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("identity: crypto/rand: " + err.Error())
	}
	out := make([]byte, len(b))
	for i, v := range b {
		out[i] = codeAlphabet[int(v)%len(codeAlphabet)]
	}
	return string(out)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
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
// aimed at somebody else's account.
func randomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not recoverable at this layer; a panic beats
		// minting a predictable identity.
		panic("identity: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}
