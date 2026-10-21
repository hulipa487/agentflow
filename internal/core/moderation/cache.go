package moderation

import (
	"container/list"
	"sync"
	"time"

	"agentflow/internal/core/safety"
)

// verdictCache is a small TTL+LRU cache of raw classifier verdicts. The key is
// a content hash, so the cache never stores message text. Policy (thresholds
// and label actions) is applied after a hit, which means changing a threshold
// does not require invalidating the cache.
type verdictCache struct {
	mu    sync.Mutex
	cap   int
	ttl   time.Duration
	items map[string]*list.Element
	order *list.List
}

type cacheEntry struct {
	key   string
	value safety.Verdict
	exp   time.Time
}

func newVerdictCache(capacity int, ttl time.Duration) *verdictCache {
	if capacity <= 0 {
		return nil
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &verdictCache{
		cap:   capacity,
		ttl:   ttl,
		items: make(map[string]*list.Element, capacity),
		order: list.New(),
	}
}

func (c *verdictCache) get(key string) (safety.Verdict, bool) {
	if c == nil {
		return safety.Verdict{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return safety.Verdict{}, false
	}
	entry := el.Value.(*cacheEntry)
	if time.Now().After(entry.exp) {
		c.order.Remove(el)
		delete(c.items, key)
		return safety.Verdict{}, false
	}
	c.order.MoveToFront(el)
	return entry.value, true
}

func (c *verdictCache) put(key string, v safety.Verdict) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		entry := el.Value.(*cacheEntry)
		entry.value = v
		entry.exp = time.Now().Add(c.ttl)
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&cacheEntry{key: key, value: v, exp: time.Now().Add(c.ttl)})
	c.items[key] = el
	for c.order.Len() > c.cap {
		old := c.order.Back()
		if old == nil {
			break
		}
		entry := old.Value.(*cacheEntry)
		delete(c.items, entry.key)
		c.order.Remove(old)
	}
}
