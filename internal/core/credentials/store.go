// Package credentials provides an encrypted-at-rest, per-tenant store of API
// keys and other secrets, keyed by (user_uuid, service). A Lua loop never
// touches the secret: it references a credential by {service=...} on an op,
// and Go resolves it to the real value at request time.
//
// The master key is supplied by the operator (from an env var at boot) and
// used to AES-GCM encrypt each secret. A distinct random nonce is stored per
// record, so identical secrets produce different ciphertext.
package credentials

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, matching the repo's choice
)

// Store is an encrypted credential store backed by a sqlite file.
type Store struct {
	db       *sql.DB
	aesgcm   cipher.AEAD
	path     string
	log      *slog.Logger
}

// Secret is a resolved credential ready to inject into an HTTP request.
type Secret struct {
	Value  string // the real API key / token
	Header string // header name to set, e.g. "Authorization"
	Scheme string // scheme prefix, e.g. "Bearer" (may be "")
}

// ServiceRef is a credential's metadata — never its value — used by the admin
// list endpoint. Fingerprint is the last 4 characters of the secret (the same
// convention as Stripe/GitHub), enough to tell two keys apart without
// revealing anything usable.
type ServiceRef struct {
	Service     string `json:"service"`
	Kind        string `json:"kind"`
	Fingerprint string `json:"fingerprint,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

// Open opens (creating if needed) the credential store at path. masterKey is
// any non-empty string; it is hashed to a 32-byte AES key. A wrong key
// produces a hard decrypt error on first read, not silent data.
func Open(path string, masterKey string, log *slog.Logger) (*Store, error) {
	if masterKey == "" {
		return nil, fmt.Errorf("credentials: master key is empty")
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("credentials: open %s: %w", path, err)
	}
	if _, err := db.Exec(`
		PRAGMA journal_mode = WAL;
		PRAGMA busy_timeout = 5000;
		PRAGMA foreign_keys = ON;
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("credentials: pragma: %w", err)
	}
	key := sha256.Sum256([]byte(masterKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("credentials: cipher: %w", err)
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("credentials: gcm: %w", err)
	}
	s := &Store{db: db, aesgcm: aesgcm, path: path, log: log.With("module", "credentials")}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS credentials (
			user_uuid  TEXT NOT NULL,
			service    TEXT NOT NULL,
			kind       TEXT NOT NULL,
			secret     BLOB NOT NULL,        -- AES-GCM ciphertext (nonce||ct)
			header     TEXT NOT NULL DEFAULT 'Authorization',
			scheme     TEXT NOT NULL DEFAULT 'Bearer',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			revoked    INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (user_uuid, service)
		);
	`)
	return err
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Put upserts a credential. header/scheme default to "Authorization"/"Bearer"
// when empty. The secret is encrypted before it touches the database.
func (s *Store) Put(ctx context.Context, userUUID, service, kind, secret, header, scheme string) error {
	if userUUID == "" || service == "" || secret == "" {
		return fmt.Errorf("credentials: user_uuid, service and secret are required")
	}
	if header == "" {
		header = "Authorization"
	}
	if scheme == "" {
		scheme = "Bearer"
	}
	ct, err := s.encrypt(secret)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO credentials (user_uuid, service, kind, secret, header, scheme, created_at, updated_at, revoked)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT(user_uuid, service) DO UPDATE SET
			kind = excluded.kind,
			secret = excluded.secret,
			header = excluded.header,
			scheme = excluded.scheme,
			updated_at = excluded.updated_at,
			revoked = 0`,
		userUUID, service, kind, ct, header, scheme, now, now)
	if err != nil {
		return fmt.Errorf("credentials: put %s/%s: %w", userUUID, service, err)
	}
	return nil
}

// Get returns the resolved secret for (userUUID, service). ok=false when the
// credential is absent or revoked. A wrong master key surfaces as an error.
func (s *Store) Get(ctx context.Context, userUUID, service string) (Secret, bool, error) {
	var (
		ctBlob []byte
		header string
		scheme string
		rev    int
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT secret, header, scheme, revoked FROM credentials WHERE user_uuid = ? AND service = ?`,
		userUUID, service).Scan(&ctBlob, &header, &scheme, &rev)
	if err == sql.ErrNoRows {
		return Secret{}, false, nil
	}
	if err != nil {
		return Secret{}, false, fmt.Errorf("credentials: get %s/%s: %w", userUUID, service, err)
	}
	if rev != 0 {
		return Secret{}, false, nil
	}
	val, err := s.decrypt(ctBlob)
	if err != nil {
		return Secret{}, false, fmt.Errorf("credentials: decrypt %s/%s: %w (master key mismatch?)", userUUID, service, err)
	}
	return Secret{Value: val, Header: header, Scheme: scheme}, true, nil
}

// Delete removes a credential (revocation). Idempotent.
func (s *Store) Delete(ctx context.Context, userUUID, service string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM credentials WHERE user_uuid = ? AND service = ?`, userUUID, service)
	if err != nil {
		return fmt.Errorf("credentials: delete %s/%s: %w", userUUID, service, err)
	}
	return nil
}

// List returns service metadata (never values) for a user.
func (s *Store) List(ctx context.Context, userUUID string) ([]ServiceRef, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT service, kind, secret, created_at, updated_at FROM credentials WHERE user_uuid = ? AND revoked = 0 ORDER BY service`,
		userUUID)
	if err != nil {
		return nil, fmt.Errorf("credentials: list %s: %w", userUUID, err)
	}
	defer rows.Close()
	out := []ServiceRef{}
	for rows.Next() {
		var r ServiceRef
		var ctBlob []byte
		if err := rows.Scan(&r.Service, &r.Kind, &ctBlob, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("credentials: list scan: %w", err)
		}
		// Best-effort fingerprint: a decrypt failure (wrong master key) must
		// not fail the listing — the row just shows no fingerprint.
		if val, err := s.decrypt(ctBlob); err == nil && len(val) >= 4 {
			r.Fingerprint = "…" + val[len(val)-4:]
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListUsers returns the distinct user UUIDs that hold at least one
// non-revoked credential, for the admin UI's per-user browsing.
func (s *Store) ListUsers(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT user_uuid FROM credentials WHERE revoked = 0 ORDER BY user_uuid`)
	if err != nil {
		return nil, fmt.Errorf("credentials: list users: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, fmt.Errorf("credentials: list users scan: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// encrypt returns base64(nonce || ciphertext). The nonce is random per record.
func (s *Store) encrypt(plaintext string) ([]byte, error) {
	nonce := make([]byte, s.aesgcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("credentials: nonce: %w", err)
	}
	ct := s.aesgcm.Seal(nonce, nonce, []byte(plaintext), nil)
	// Store as base64 text so the BLOB is opaque in sqlite and tooling.
	enc := base64.StdEncoding.EncodeToString(ct)
	return []byte(enc), nil
}

func (s *Store) decrypt(b []byte) (string, error) {
	ct, err := base64.StdEncoding.DecodeString(string(b))
	if err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	ns := s.aesgcm.NonceSize()
	if len(ct) < ns {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, body := ct[:ns], ct[ns:]
	pt, err := s.aesgcm.Open(nil, nonce, body, nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
