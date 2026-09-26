// Package limits holds the per-tenant resource bounds (E18) that are pure
// admission control: a rate limiter for egress, the shape a per-tenant
// ceiling takes when the accounting is in-memory by design.
package limits

import (
	"sync"
	"time"
)

// Rate is a fixed-window rate limiter keyed by string — the tenant uuid, for
// egress. Fixed-window over-allocates at window boundaries by up to 2x, which
// is the right trade here: the limit is a DoS backstop (D14), not a shape of
// traffic anyone is promised. State is in-memory and resets on restart, which
// is standard for rate limiting and wrong for nothing else.
//
// A zero Limit disables the limiter — every call is allowed — so a deployment
// opts in per bound rather than by construction.
type Rate struct {
	Limit  int
	Window time.Duration

	mu   sync.Mutex
	bins map[string]*windowBin
}

type windowBin struct {
	start time.Time
	count int
}

// Allow reports whether key may proceed in this window.
func (r *Rate) Allow(key string) bool {
	if r.Limit <= 0 || key == "" {
		return true // disabled, or engine-internal traffic with no tenant
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bins == nil {
		r.bins = map[string]*windowBin{}
	}
	b, ok := r.bins[key]
	if !ok || now.Sub(b.start) >= r.Window {
		// Lazily prune: a hostile fleet of distinct keys grows the map, so
		// dead bins are dropped whenever live ones roll over.
		if len(r.bins) > 4*r.Limit {
			for k, old := range r.bins {
				if now.Sub(old.start) >= r.Window {
					delete(r.bins, k)
				}
			}
		}
		b = &windowBin{start: now}
		r.bins[key] = b
	}
	if b.count >= r.Limit {
		return false
	}
	b.count++
	return true
}
