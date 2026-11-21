package runtime

import (
	"context"
	"time"

	"agentflow/internal/core/media"
)

// MediaKV wraps a runtime.Rows store as a media.KV so internal/core/media can
// persist blob metadata, tenant grants, and derived-variant caches without
// importing internal/core/runtime.
func MediaKV(r Rows) media.KV {
	if r == nil {
		return nil
	}
	return rowsMediaKV{r: r}
}

type rowsMediaKV struct{ r Rows }

func (a rowsMediaKV) Put(ctx context.Context, key, value string, expiresAt time.Time) error {
	return a.r.PutRow(ctx, key, value, expiresAt)
}

func (a rowsMediaKV) Get(ctx context.Context, key string) (string, bool, error) {
	row, ok, err := a.r.GetRow(ctx, key)
	if err != nil || !ok {
		return "", ok, err
	}
	return row.Value, true, nil
}

func (a rowsMediaKV) Delete(ctx context.Context, key string) error {
	return a.r.DeleteRow(ctx, key)
}

func (a rowsMediaKV) List(ctx context.Context, prefix string) ([]media.KVEntry, error) {
	rows, err := a.r.ListRows(ctx, prefix)
	if err != nil {
		return nil, err
	}
	out := make([]media.KVEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, media.KVEntry{
			Key:       row.Key,
			Value:     row.Value,
			ExpiresAt: row.ExpiresAt,
		})
	}
	return out, nil
}
