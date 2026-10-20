package runtime

import (
	"context"
	"fmt"
)

// userDataPrefixes are the files_meta namespaces owned by one profile. Scope
// strings are "user:<uuid>"; scratch rows are keyed by session, not tenant, so
// they are left to the scratch TTL (they cannot be attributed reliably).
func userDataPrefixes(userID string) []string {
	scope := "user:" + userID
	return []string{
		"t|" + scope + "|",
		"r|" + scope + "|",
		"c|" + scope + "|",
	}
}

func deleteUserRows(ctx context.Context, list func(context.Context, string) ([]Row, error), del func(context.Context, string) error, userID string) error {
	for _, prefix := range userDataPrefixes(userID) {
		rows, err := list(ctx, prefix)
		if err != nil {
			return fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, row := range rows {
			if err := del(ctx, row.Key); err != nil {
				return fmt.Errorf("delete %s: %w", row.Key, err)
			}
		}
	}
	return nil
}

func (s *sqliteStore) DeleteUserData(ctx context.Context, userID string) error {
	for _, stmt := range []string{
		`DELETE FROM usage_events WHERE user_id = ?`,
		`DELETE FROM usage_daily WHERE user_id = ?`,
		`DELETE FROM message_journal WHERE user_uuid = ?`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt, userID); err != nil {
			return fmt.Errorf("delete user data: %w", err)
		}
	}
	return deleteUserRows(ctx, s.ListRows, s.DeleteRow, userID)
}

func (s *postgresStore) DeleteUserData(ctx context.Context, userID string) error {
	for _, stmt := range []string{
		`DELETE FROM usage_events WHERE user_id = $1`,
		`DELETE FROM usage_daily WHERE user_id = $1`,
		`DELETE FROM message_journal WHERE user_uuid = $1`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt, userID); err != nil {
			return fmt.Errorf("delete user data: %w", err)
		}
	}
	return deleteUserRows(ctx, s.ListRows, s.DeleteRow, userID)
}
