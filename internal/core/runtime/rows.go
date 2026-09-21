package runtime

import (
	"context"
	"database/sql"
	"time"
)

// Row is one engine-owned files_meta record. Key is the caller's namespaced
// key ("t|<scope>|<project>|<path>", "s|<owner>|<name>", ...); Value is JSON;
// ExpiresAt is a unix-nanos deadline, 0 = never.
type Row struct {
	Key       string
	Value     string
	UpdatedAt time.Time
	ExpiresAt time.Time
}

// PutRow writes one row, replacing any existing row with the same key.
// expiresAt zero means the row never expires.
func (s *sqliteStore) PutRow(ctx context.Context, key, value string, expiresAt time.Time) error {
	var exp int64
	if !expiresAt.IsZero() {
		exp = expiresAt.UnixNano()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO files_meta (key, value, updated_at, expires_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			updated_at = excluded.updated_at,
			expires_at = excluded.expires_at`,
		key, value, time.Now().UnixNano(), exp)
	return err
}

// GetRow returns the row for key. A row past its expiry deadline is deleted
// here (lazy expiry) and reported as not found.
func (s *sqliteStore) GetRow(ctx context.Context, key string) (Row, bool, error) {
	var r Row
	var updated, exp int64
	err := s.db.QueryRowContext(ctx,
		`SELECT key, value, updated_at, expires_at FROM files_meta WHERE key = ?`, key).
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
		_ = s.DeleteRow(ctx, key)
		return Row{}, false, nil
	}
	return r, true, nil
}

// DeleteRow removes one row (idempotent).
func (s *sqliteStore) DeleteRow(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM files_meta WHERE key = ?`, key)
	return err
}

// ListRows returns rows whose key starts with prefix, in key order. The range
// scan (>= prefix, < prefix||'\xff') uses the primary key — LIKE would not.
// Expired rows are skipped and deleted as they are scanned.
func (s *sqliteStore) ListRows(ctx context.Context, prefix string) ([]Row, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT key, value, updated_at, expires_at FROM files_meta
		WHERE key >= ? AND key < ? ORDER BY key`, prefix, prefix+"\xff")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
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

// SweepExpired deletes every expired row and returns how many were removed.
// Runs at boot so scratch space does not accumulate across restarts.
func (s *sqliteStore) SweepExpired(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM files_meta WHERE expires_at != 0 AND expires_at < ?`,
		time.Now().UnixNano())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
