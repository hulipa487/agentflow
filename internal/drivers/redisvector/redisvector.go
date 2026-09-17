// Package redisvector implements the builtin:redisvector vector backend
// provider. It provides the kv, prefix_scan, ttl, and vector features over a
// Redis server with the RediSearch module (Redis Stack).
//
// It is deliberately a separate provider from builtin:redis rather than an
// extension of it. Provider features are declared per provider name with no
// instance negotiation (memory.BackendProvider.Features), so folding "vector"
// into builtin:redis would claim vector support on every Redis, including the
// ones without the module; and RediSearch indexes hashes, while builtin:redis
// stores plain SET strings, so the layout could not be shared without breaking
// existing stored data anyway.
//
// Layout: one hash per record at "<table>:<key>" — the same key convention
// builtin:redis uses — with fields value (JSON), updated_at, and embedding (a
// little-endian float32 blob) when one was supplied. One RediSearch index per
// table, created lazily on first use.
package redisvector

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentflow/internal/core/memory"

	"github.com/redis/go-redis/v9"
)

// Provider implements memory.BackendProvider for "builtin:redisvector".
type Provider struct{}

func (Provider) Name() string { return "builtin:redisvector" }

func (Provider) Features() []string {
	// The value is stored next to the vector, so the kv features come free.
	return []string{"kv", "prefix_scan", "ttl", "vector"}
}

// defaultDim matches the embedding size of OpenAI's text-embedding-ada-002
// family, as builtin:pgvector does; most self-hosted models are smaller.
const defaultDim = 1536

func (Provider) Open(config map[string]any) (memory.BackendHandle, error) {
	url, _ := config["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("builtin:redisvector: url is required")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("builtin:redisvector: parse url: %w", err)
	}
	dim := defaultDim
	switch v := config["dim"].(type) {
	case int:
		dim = v
	case int64:
		dim = int(v)
	case float64:
		dim = int(v)
	}
	if dim <= 0 {
		return nil, fmt.Errorf("builtin:redisvector: dim must be positive, got %d", dim)
	}
	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("builtin:redisvector: ping: %w", err)
	}
	return &Handle{client: client, dim: dim, indexed: map[string]bool{}}, nil
}

// Handle is an opened Redis (RediSearch) backend.
type Handle struct {
	client *redis.Client
	dim    int

	// mu guards indexed, which records the tables whose RediSearch index has
	// been created so a Put does not reissue FT.CREATE on every write.
	mu      sync.Mutex
	indexed map[string]bool
}

func (h *Handle) Put(table, key string, value any, opts memory.PutOpts) error {
	if len(opts.Vector) > 0 && len(opts.Vector) != h.dim {
		return fmt.Errorf("builtin:redisvector: embedding has %d dimensions, backend configured for %d (set dim in the backend config to match the embedding model)", len(opts.Vector), h.dim)
	}
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal value: %w", err)
	}
	if len(opts.Vector) > 0 {
		if err := h.ensureIndex(table); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fullKey := h.fullKey(table, key)
	// HSET without the embedding field leaves any stored embedding alone, so
	// a value written with no vector keeps the one a previous Put stored —
	// the same behaviour builtin:pgvector gets from an ON CONFLICT update.
	args := []any{"HSET", fullKey, "value", b, "updated_at", time.Now().Unix()}
	if len(opts.Vector) > 0 {
		args = append(args, "embedding", encodeVector(opts.Vector))
	}
	if err := h.client.Do(ctx, args...).Err(); err != nil {
		return err
	}
	if opts.TTL > 0 {
		return h.client.Expire(ctx, fullKey, opts.TTL).Err()
	}
	return nil
}

func (h *Handle) Get(table, key string) (any, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := h.client.HGet(ctx, h.fullKey(table, key), "value").Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, true, err
	}
	return v, true, nil
}

func (h *Handle) Delete(table, key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return h.client.Del(ctx, h.fullKey(table, key)).Err()
}

func (h *Handle) Query(table string, q memory.Query) (memory.Iterator, error) {
	switch q.Kind {
	case "prefix":
		return h.queryScan(table, q.Prefix)
	case "all":
		return h.queryScan(table, "")
	case "vector":
		return h.queryVector(table, q)
	default:
		return nil, fmt.Errorf("builtin:redisvector: unsupported query kind %q", q.Kind)
	}
}

func (h *Handle) queryScan(table, prefix string) (memory.Iterator, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var keys []string
	var cursor uint64
	for {
		batch, next, err := h.client.Scan(ctx, cursor, h.fullKey(table, prefix)+"*", 256).Result()
		if err != nil {
			return nil, err
		}
		keys = append(keys, batch...)
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return &keyIter{keys: keys, table: table, handle: h}, nil
}

func (h *Handle) queryVector(table string, q memory.Query) (memory.Iterator, error) {
	if len(q.Vector) == 0 {
		return nil, fmt.Errorf("builtin:redisvector: vector query requires a query vector")
	}
	if len(q.Vector) != h.dim {
		return nil, fmt.Errorf("builtin:redisvector: query vector has %d dimensions, backend configured for %d", len(q.Vector), h.dim)
	}
	if table == "" {
		return nil, fmt.Errorf("builtin:redisvector: vector queries need a table (the index is per table)")
	}
	if err := h.ensureIndex(table); err != nil {
		return nil, err
	}
	k := q.K
	if k <= 0 {
		k = 10
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	reply, err := h.client.Do(ctx, searchArgs(indexFor(table), k, q.Vector)...).Result()
	if err != nil {
		return nil, err
	}
	keys, err := searchKeys(reply)
	if err != nil {
		return nil, err
	}
	// NOCONTENT keeps the reply a flat key list, so the values are fetched
	// here rather than parsed out of interleaved field arrays.
	pipe := h.client.Pipeline()
	vals := make([]*redis.StringCmd, len(keys))
	for i, key := range keys {
		vals[i] = pipe.HGet(ctx, key, "value")
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, err
	}
	recs := make([]memory.Record, 0, len(keys))
	for i, key := range keys {
		raw, err := vals[i].Bytes()
		if err == redis.Nil {
			continue // expired between the search and the fetch
		}
		if err != nil {
			return nil, err
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		recs = append(recs, memory.Record{Key: h.stripTable(key, table), Value: v})
	}
	return &sliceIter{recs: recs}, nil
}

// GC is a no-op: Redis TTL handles expiry natively, as in builtin:redis.
func (h *Handle) GC(table string, window int) error { return nil }

func (h *Handle) Close() error { return h.client.Close() }

// ensureIndex creates the table's RediSearch index once. A server without the
// module answers "unknown command" here, which is the first place the
// misconfiguration can be reported — this provider is opt-in precisely because
// RediSearch is not part of plain Redis.
func (h *Handle) ensureIndex(table string) error {
	if table == "" {
		return fmt.Errorf("builtin:redisvector: a table is required")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.indexed[table] {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := h.client.Do(ctx, indexArgs(indexFor(table), h.fullKey(table, ""), h.dim)...).Err()
	// An index left from a previous process is not an error.
	if err != nil && !isIndexExists(err) {
		return fmt.Errorf("builtin:redisvector: create index for table %q (needs the RediSearch module, e.g. Redis Stack): %w", table, err)
	}
	h.indexed[table] = true
	return nil
}

func isIndexExists(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Index already exists")
}

// indexArgs is the FT.CREATE command for one table. Kept pure so the exact
// shape can be asserted in a test without a server.
func indexArgs(index, prefix string, dim int) []any {
	return []any{
		"FT.CREATE", index,
		"ON", "HASH",
		"PREFIX", 1, prefix,
		"SCHEMA",
		"value", "TEXT",
		"embedding", "VECTOR", "HNSW", 6,
		"TYPE", "FLOAT32",
		"DIM", dim,
		"DISTANCE_METRIC", "COSINE",
	}
}

// searchArgs is the k-NN query for one table. NOCONTENT returns document keys
// only, which keeps the reply a flat list.
func searchArgs(index string, k int, vec []float32) []any {
	return []any{
		"FT.SEARCH", index,
		"(*)=>[KNN " + strconv.Itoa(k) + " @embedding $vec AS score]",
		"PARAMS", 2, "vec", encodeVector(vec),
		"SORTBY", "score",
		"DIALECT", 2,
		"NOCONTENT",
	}
}

// searchKeys reads the document keys out of an FT.SEARCH ... NOCONTENT reply,
// which is [total, key, key, ...].
func searchKeys(reply any) ([]string, error) {
	arr, ok := reply.([]any)
	if !ok {
		return nil, fmt.Errorf("builtin:redisvector: unexpected search reply %T", reply)
	}
	if len(arr) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(arr)-1)
	for _, e := range arr[1:] {
		switch v := e.(type) {
		case string:
			out = append(out, v)
		case []byte:
			out = append(out, string(v))
		default:
			return nil, fmt.Errorf("builtin:redisvector: unexpected search reply element %T", e)
		}
	}
	return out, nil
}

// encodeVector packs a float32 slice as the little-endian blob RediSearch
// expects for a FLOAT32 vector field.
func encodeVector(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

func indexFor(table string) string { return "idx:" + table }

// fullKey is builtin:redis's key convention: "<table>:<key>", or the bare key
// when there is no table.
func (h *Handle) fullKey(table, key string) string {
	if table == "" {
		return key
	}
	return table + ":" + key
}

// stripTable recovers the logical key from a document id.
func (h *Handle) stripTable(fullKey, table string) string {
	prefix := table + ":"
	if len(fullKey) >= len(prefix) && fullKey[:len(prefix)] == prefix {
		return fullKey[len(prefix):]
	}
	return fullKey
}

type sliceIter struct {
	recs []memory.Record
	i    int
}

func (it *sliceIter) Next() bool {
	if it.i >= len(it.recs) {
		return false
	}
	it.i++
	return true
}

func (it *sliceIter) Record() memory.Record { return it.recs[it.i-1] }
func (it *sliceIter) Err() error            { return nil }

// keyIter pulls the values behind a set of keys, one at a time, so a prefix
// scan over a large table does not hold every value in memory.
type keyIter struct {
	keys   []string
	table  string
	handle *Handle
	i      int
	rec    memory.Record
	err    error
	done   bool
}

func (it *keyIter) Next() bool {
	if it.done || it.err != nil {
		return false
	}
	for it.i < len(it.keys) {
		key := it.keys[it.i]
		it.i++
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		raw, err := it.handle.client.HGet(ctx, key, "value").Bytes()
		cancel()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			it.err = err
			return false
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			it.err = err
			return false
		}
		it.rec = memory.Record{Key: it.handle.stripTable(key, it.table), Value: v}
		return true
	}
	it.done = true
	return false
}

func (it *keyIter) Record() memory.Record { return it.rec }
func (it *keyIter) Err() error            { return it.err }
