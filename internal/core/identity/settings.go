package identity

import (
	"context"
	"sync"
	"time"
)

// DefaultSettingsTTL bounds how stale a per-user override may be on one
// instance. It matters in a fleet: an operator changing someone's model through
// one instance's console is seen by the others within this window, and the
// alternative — a read per loop turn — is a store query on the hot path.
const DefaultSettingsTTL = 30 * time.Second

// maxCachedSettings bounds the cache. It is a crude ceiling rather than an LRU:
// the entries are two small strings, and past ten thousand users a periodic
// flush costs one extra read each and is simpler than eviction order.
const maxCachedSettings = 10_000

// SettingsSource serves a profile's per-user overrides to the session layer,
// with a short cache: the session asks on every loop turn, and the answer is
// almost always "nothing".
type SettingsSource struct {
	reg   *Registry
	ttl   time.Duration
	mu    sync.Mutex
	cache map[string]cachedSettings
}

type cachedSettings struct {
	model        string
	instructions string
	at           time.Time
}

// NewSettingsSource wraps a registry. ttl <= 0 takes DefaultSettingsTTL.
func NewSettingsSource(reg *Registry, ttl time.Duration) *SettingsSource {
	if ttl <= 0 {
		ttl = DefaultSettingsTTL
	}
	return &SettingsSource{reg: reg, ttl: ttl, cache: map[string]cachedSettings{}}
}

// Settings reports a profile's model and instruction overrides. It implements
// the session layer's lookup by signature alone.
//
// An unknown or unregistered user is not an error — they have no overrides —
// and a store that cannot be read falls back to what was last known, so a
// profile read never fails a person's turn.
func (s *SettingsSource) Settings(ctx context.Context, userID string) (model, instructionsAppend string, err error) {
	if s == nil || s.reg == nil || userID == "" {
		return "", "", nil
	}
	now := time.Now()
	s.mu.Lock()
	hit, ok := s.cache[userID]
	s.mu.Unlock()
	if ok && now.Sub(hit.at) < s.ttl {
		return hit.model, hit.instructions, nil
	}

	model, instructionsAppend, err = s.reg.Overrides(userID)
	if err != nil {
		if ok {
			// Stale beats absent: keep answering with what we knew.
			return hit.model, hit.instructions, nil
		}
		return "", "", err
	}
	s.mu.Lock()
	if len(s.cache) >= maxCachedSettings {
		s.cache = map[string]cachedSettings{}
	}
	s.cache[userID] = cachedSettings{model: model, instructions: instructionsAppend, at: now}
	s.mu.Unlock()
	return model, instructionsAppend, nil
}
