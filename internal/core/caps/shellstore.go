package caps

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agentflow/internal/core/runtime"
	"agentflow/internal/drivers/shell"
)

// ShellStore keeps shell handle records in the runtime store's row table — the
// same table the file store's metadata and a session's state use — so a fleet
// that shares a runtime store shares its shell handles for free, and an
// instance with a local store behaves exactly as it did before there was a
// registry at all.
//
// One row per handle, keyed by handle id. The owner lookups and the reclaim
// pass scan and filter instead of keeping a second index: a deployment holds
// one record per shell — tens, not millions — and an index is one more thing
// that can disagree with what it indexes.
type ShellStore struct {
	Store runtime.Store
}

// shellRowPrefix namespaces handle records in the shared row table.
const shellRowPrefix = "shell|h|"

// Save writes a handle record.
func (s ShellStore) Save(ctx context.Context, rec shell.Record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("shell store: encode %s: %w", rec.ID, err)
	}
	if err := s.Store.PutRow(ctx, shellRowPrefix+rec.ID, string(b), time.Time{}); err != nil {
		return fmt.Errorf("shell store: save %s: %w", rec.ID, err)
	}
	return nil
}

// Load reads one handle record.
func (s ShellStore) Load(ctx context.Context, id string) (shell.Record, bool, error) {
	row, ok, err := s.Store.GetRow(ctx, shellRowPrefix+id)
	if err != nil || !ok {
		return shell.Record{}, false, err
	}
	rec, err := decodeShellRecord(row.Value)
	if err != nil {
		return shell.Record{}, false, err
	}
	return rec, true, nil
}

// List returns the records for one owner, or every record when owner is empty.
func (s ShellStore) List(ctx context.Context, owner string) ([]shell.Record, error) {
	rows, err := s.Store.ListRows(ctx, shellRowPrefix)
	if err != nil {
		return nil, err
	}
	out := make([]shell.Record, 0, len(rows))
	for _, row := range rows {
		rec, err := decodeShellRecord(row.Value)
		if err != nil {
			return nil, err
		}
		if owner != "" && rec.Owner != owner {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// Delete removes a handle record. A record that is already gone is not an error:
// the caller is releasing a resource, and releasing it twice is one state.
func (s ShellStore) Delete(ctx context.Context, id string) error {
	if err := s.Store.DeleteRow(ctx, shellRowPrefix+id); err != nil {
		return fmt.Errorf("shell store: delete %s: %w", id, err)
	}
	return nil
}

// decodeShellRecord reads a stored record. The error names the shape of the
// failure rather than the row, because these rows are written by Save and a
// value that will not decode is corruption rather than a version difference.
func decodeShellRecord(value string) (shell.Record, error) {
	var rec shell.Record
	if err := json.Unmarshal([]byte(value), &rec); err != nil {
		return shell.Record{}, fmt.Errorf("shell store: decode a handle record: %w", err)
	}
	return rec, nil
}
