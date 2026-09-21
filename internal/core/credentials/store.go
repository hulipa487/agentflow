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

	"agentflow/internal/core/storedb"
)

// Store is an encrypted credential store on the engine's SQL store: a local
// file on one machine, or the shared server a fleet agrees on.
type Store struct {
	st     *storedb.DB
	aesgcm cipher.AEAD
	path   string
	log    *slog.Logger
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

// Open opens (creating if needed) the credential store at target: a SQLite file
// path, or a PostgreSQL DSN. A fleet points every instance at the same server,
// so a key an operator adds on one instance resolves on all of them. masterKey
// is any non-empty string; it is hashed to a 32-byte AES key. A wrong key
// produces a hard decrypt error on first read, not silent data.
func Open(target string, masterKey string, log *slog.Logger) (*Store, error) {
	if masterKey == "" {
		return nil, fmt.Errorf("credentials: master key is empty")
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	st, err := storedb.Open(target)
	if err != nil {
		return nil, fmt.Errorf("credentials: %w", err)
	}
	key := sha256.Sum256([]byte(masterKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("credentials: cipher: %w", err)
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("credentials: gcm: %w", err)
	}
	s := &Store{st: st, aesgcm: aesgcm, path: st.Target(), log: log.With("module", "credentials")}
	if err := s.migrate(); err != nil {
		_ = st.Close()
		return nil, err
	}
	return s, nil
}

// schema is the credential table, written once for both backends.
//
// The ciphertext column is TEXT, not a binary type: the stored value is
// base64 of nonce||ciphertext, which is what makes it the same bytes in a
// SQLite file and in a server, and legible to whoever is looking at the
// database. (A BLOB column in an existing file keeps working — a declared type
// only sets SQLite's affinity, and TEXT is the affinity these bytes want.)
var schema = []string{
	`CREATE TABLE IF NOT EXISTS credentials (
		user_uuid  TEXT NOT NULL,
		service    TEXT NOT NULL,
		kind       TEXT NOT NULL,
		secret     TEXT NOT NULL,
		header     TEXT NOT NULL DEFAULT 'Authorization',
		scheme     TEXT NOT NULL DEFAULT 'Bearer',
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL,
		revoked    INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (user_uuid, service)
	);`,
}

func (s *Store) migrate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.st.ExecDDL(ctx, schema...)
}

// Close releases the store's hold on the connection pool.
func (s *Store) Close() error { return s.st.Close() }

// Put upserts a credential. header/scheme default to "Authorization"/"Bearer"
// when empty. The secret is encrypted before it touches the database. An
// empty userUUID stores the credential engine-wide (the CLI and the lazy
// config resolver use this tenancy); per-user records use the caller's UUID.
func (s *Store) Put(ctx context.Context, userUUID, service, kind, secret, header, scheme string) error {
	if service == "" || secret == "" {
		return fmt.Errorf("credentials: service and secret are required")
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
	// string(ct), not ct: the column is text, and a []byte handed to a text
	// column is a bytea on a server — which PostgreSQL refuses rather than
	// casting. The bytes are the same either way.
	_, err = s.st.Exec(ctx, `
		INSERT INTO credentials (user_uuid, service, kind, secret, header, scheme, created_at, updated_at, revoked)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT(user_uuid, service) DO UPDATE SET
			kind = excluded.kind,
			secret = excluded.secret,
			header = excluded.header,
			scheme = excluded.scheme,
			updated_at = excluded.updated_at,
			revoked = 0`,
		userUUID, service, kind, string(ct), header, scheme, now, now)
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
	err := s.st.QueryRow(ctx,
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
	_, err := s.st.Exec(ctx,
		`DELETE FROM credentials WHERE user_uuid = ? AND service = ?`, userUUID, service)
	if err != nil {
		return fmt.Errorf("credentials: delete %s/%s: %w", userUUID, service, err)
	}
	return nil
}

// List returns service metadata (never values) for a user.
func (s *Store) List(ctx context.Context, userUUID string) ([]ServiceRef, error) {
	rows, err := s.st.Query(ctx,
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
	rows, err := s.st.Query(ctx,
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
