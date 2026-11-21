package media

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// KV is the minimal persistent row-store contract the media catalog needs
// (satisfied by runtime.MediaKV over SQLite and PostgreSQL without creating an
// import cycle with internal/core/runtime).
type KV interface {
	Put(ctx context.Context, key, value string, expiresAt time.Time) error
	Get(ctx context.Context, key string) (string, bool, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]KVEntry, error)
}

// KVEntry is one row returned by KV.List.
type KVEntry struct {
	Key       string
	Value     string
	ExpiresAt time.Time
}

// Row key prefixes inside runtime.Rows (disjoint from files' t|/r|/c|/s| and
// tenancy/router/shelljobs prefixes).
const (
	metaPrefix    = "mb|" // mb|<handle>
	grantPrefix   = "mg|" // mg|<scope>|<handle>
	variantPrefix = "mv|" // mv|<src_handle>|<variant_key>
)

// BlobMeta stores content-level metadata for one stored handle.
type BlobMeta struct {
	Handle    string `json:"handle"`
	MIME      string `json:"mime,omitempty"`
	Size      int64  `json:"size"`
	Origin    string `json:"origin,omitempty"` // channel | llm | mcp | http | derived | files
	CreatedAt int64  `json:"created_at"`
}

// GrantRecord records that one engine-resolved scope (user:<uuid>,
// user:<membership_uuid>, or agent:<name>) is authorized to materialize handle.
type GrantRecord struct {
	Scope     string `json:"scope"`
	Handle    string `json:"handle"`
	Origin    string `json:"origin,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

// VariantRecord caches a derived (transcoded) blob produced from SourceHandle.
type VariantRecord struct {
	SourceHandle string `json:"source_handle"`
	Handle       string `json:"handle"`
	MIME         string `json:"mime"`
	Size         int64  `json:"size"`
	CreatedAt    int64  `json:"created_at"`
}

// Catalog tracks blob metadata, per-scope authorization grants, and cached
// derived variants in KV.
type Catalog struct {
	kv  KV
	now func() time.Time
}

// NewCatalog wraps kv as a media Catalog. A nil kv yields a no-op catalog
// (useful in minimal unit tests).
func NewCatalog(kv KV) *Catalog {
	return &Catalog{kv: kv, now: time.Now}
}

func (c *Catalog) enabled() bool { return c != nil && c.kv != nil }

func metaKey(handle string) string                { return metaPrefix + handle }
func grantKey(scope, handle string) string        { return grantPrefix + scope + "|" + handle }
func grantScopePrefix(scope string) string        { return grantPrefix + scope + "|" }
func variantKey(srcHandle, specKey string) string { return variantPrefix + srcHandle + "|" + specKey }

// RecordMeta upserts metadata for handle.
func (c *Catalog) RecordMeta(ctx context.Context, m BlobMeta, ttl time.Duration) error {
	if !c.enabled() {
		return nil
	}
	if m.CreatedAt == 0 {
		m.CreatedAt = c.now().Unix()
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	var exp time.Time
	if ttl > 0 {
		exp = c.now().Add(ttl)
	}
	return c.kv.Put(ctx, metaKey(m.Handle), string(b), exp)
}

// GetMeta fetches stored metadata for handle.
func (c *Catalog) GetMeta(ctx context.Context, handle string) (BlobMeta, bool, error) {
	if !c.enabled() {
		return BlobMeta{}, false, nil
	}
	raw, ok, err := c.kv.Get(ctx, metaKey(handle))
	if err != nil || !ok {
		return BlobMeta{}, ok, err
	}
	var m BlobMeta
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return BlobMeta{}, false, fmt.Errorf("media catalog: corrupt meta %q: %w", handle, err)
	}
	return m, true, nil
}

// Grant records that scope may materialize handle until now+ttl (zero ttl = no expiry).
func (c *Catalog) Grant(ctx context.Context, scope, handle, origin string, ttl time.Duration) error {
	if !c.enabled() || scope == "" || !ValidHandle(handle) {
		return nil
	}
	rec := GrantRecord{
		Scope:     scope,
		Handle:    handle,
		Origin:    origin,
		CreatedAt: c.now().Unix(),
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	var exp time.Time
	if ttl > 0 {
		exp = c.now().Add(ttl)
	}
	return c.kv.Put(ctx, grantKey(scope, handle), string(b), exp)
}

// HasGrant reports whether scope holds a live (unexpired) grant for handle.
func (c *Catalog) HasGrant(ctx context.Context, scope, handle string) (bool, error) {
	if !c.enabled() {
		return true, nil
	}
	if scope == "" || !ValidHandle(handle) {
		return false, nil
	}
	_, ok, err := c.kv.Get(ctx, grantKey(scope, handle))
	return ok, err
}

// PutVariant caches a transcoded variant mapping with ttl.
func (c *Catalog) PutVariant(ctx context.Context, srcHandle, specKey string, v VariantRecord, ttl time.Duration) error {
	if !c.enabled() {
		return nil
	}
	if v.CreatedAt == 0 {
		v.CreatedAt = c.now().Unix()
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var exp time.Time
	if ttl > 0 {
		exp = c.now().Add(ttl)
	}
	return c.kv.Put(ctx, variantKey(srcHandle, specKey), string(b), exp)
}

// GetVariant looks up a cached transcoded variant.
func (c *Catalog) GetVariant(ctx context.Context, srcHandle, specKey string) (VariantRecord, bool, error) {
	if !c.enabled() {
		return VariantRecord{}, false, nil
	}
	raw, ok, err := c.kv.Get(ctx, variantKey(srcHandle, specKey))
	if err != nil || !ok {
		return VariantRecord{}, ok, err
	}
	var v VariantRecord
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return VariantRecord{}, false, fmt.Errorf("media catalog: corrupt variant: %w", err)
	}
	return v, true, nil
}

// PurgeScope removes every media grant owned by scope (e.g., on user purge).
// Underlying blobs are left for GC, which sweeps them once no other scope or
// files_meta entry references them.
func (c *Catalog) PurgeScope(ctx context.Context, scope string) (int, error) {
	if !c.enabled() || scope == "" {
		return 0, nil
	}
	rows, err := c.kv.List(ctx, grantScopePrefix(scope))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		if err := c.kv.Delete(ctx, r.Key); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// LiveHandles returns every handle referenced by an unexpired grant or cached
// derived variant, for unified mark-sweep blob GC.
func (c *Catalog) LiveHandles(ctx context.Context) (map[string]bool, error) {
	live := map[string]bool{}
	if !c.enabled() {
		return live, nil
	}
	now := c.now()

	grants, err := c.kv.List(ctx, grantPrefix)
	if err != nil {
		return nil, err
	}
	for _, r := range grants {
		if !r.ExpiresAt.IsZero() && now.After(r.ExpiresAt) {
			continue
		}
		var g GrantRecord
		if err := json.Unmarshal([]byte(r.Value), &g); err == nil && ValidHandle(g.Handle) {
			live[g.Handle] = true
		} else if i := strings.LastIndex(r.Key, "|"); i > 0 {
			if h := r.Key[i+1:]; ValidHandle(h) {
				live[h] = true
			}
		}
	}

	variants, err := c.kv.List(ctx, variantPrefix)
	if err != nil {
		return nil, err
	}
	for _, r := range variants {
		if !r.ExpiresAt.IsZero() && now.After(r.ExpiresAt) {
			continue
		}
		var v VariantRecord
		if err := json.Unmarshal([]byte(r.Value), &v); err == nil && ValidHandle(v.Handle) {
			live[v.Handle] = true
		}
	}
	return live, nil
}
