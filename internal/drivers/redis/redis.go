// Package redis implements the builtin:redis memory backend provider.
// It provides kv, prefix_scan, and ttl features via a Redis server.
package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agentflow/internal/core/memory"
	"github.com/redis/go-redis/v9"
)

// Provider implements memory.BackendProvider for "builtin:redis".
type Provider struct{}

func (Provider) Name() string { return "builtin:redis" }

func (Provider) Features() []string {
	return []string{"kv", "prefix_scan", "ttl"}
}

func (Provider) Open(config map[string]any) (memory.BackendHandle, error) {
	url, _ := config["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("builtin:redis: url is required")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("builtin:redis: parse url: %w", err)
	}
	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("builtin:redis: ping: %w", err)
	}
	return &Handle{client: client}, nil
}

type Handle struct {
	client *redis.Client
}

func (h *Handle) Put(table, key string, value any, opts memory.PutOpts) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fullKey := h.fullKey(table, key)
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if opts.TTL > 0 {
		return h.client.Set(ctx, fullKey, b, opts.TTL).Err()
	}
	return h.client.Set(ctx, fullKey, b, 0).Err()
}

func (h *Handle) Get(table, key string) (any, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fullKey := h.fullKey(table, key)
	raw, err := h.client.Get(ctx, fullKey).Bytes()
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
		return h.queryPrefix(table, q.Prefix)
	case "all":
		return h.queryAll(table)
	default:
		return nil, fmt.Errorf("builtin:redis: unsupported query kind %q", q.Kind)
	}
}

func (h *Handle) queryPrefix(table, prefix string) (memory.Iterator, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var vals []string
	var err error
	if table != "" {
		vals, err = h.client.Keys(ctx, h.fullKey(table, prefix)+"*").Result()
	} else {
		vals, err = h.client.Keys(ctx, prefix+"*").Result()
	}
	if err != nil {
		return nil, err
	}
	return &sliceIter{keys: vals, handle: h}, nil
}

func (h *Handle) queryAll(table string) (memory.Iterator, error) {
	return h.queryPrefix(table, "")
}

func (h *Handle) GC(table string, window int) error {
	// Redis TTL handles expiry natively; window-based GC is a no-op.
	return nil
}

func (h *Handle) Close() error { return h.client.Close() }

func (h *Handle) fullKey(table, key string) string {
	if table == "" {
		return key
	}
	return table + ":" + key
}