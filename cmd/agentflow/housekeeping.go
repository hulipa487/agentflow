// Housekeeping arbitration helpers: the lease the scheduled jobs claim, and
// the ticker that repeats them.
package main

import (
	"context"
	"log/slog"
	"time"

	"agentflow/internal/core/lease"
)

// housekeepingSlack is added to a job's own interval when it claims its lease.
// The lease must outlive the gap between two ticks of the same job, or a peer
// takes over mid-interval and the job runs twice per interval — the exact thing
// the lease exists to prevent. Five minutes covers scheduling jitter and a slow
// boot without keeping a crashed holder's lease alive for a meaningful extra
// time on a daily job.
const housekeepingSlack = 5 * time.Minute

// housekeepingTTL is the lease a job with the given interval claims.
func housekeepingTTL(interval time.Duration) time.Duration {
	if interval <= 0 {
		return lease.DefaultTTL
	}
	return interval + housekeepingSlack
}

// claimAndRun runs fn only while this instance holds name for ttl. It reports
// whether fn ran. A store that cannot answer is a store that cannot arbitrate:
// the work is skipped rather than run by everyone.
func claimAndRun(ctx context.Context, leases *lease.Manager, name string, ttl time.Duration, log *slog.Logger, fn func()) bool {
	ok, err := leases.AcquireFor(ctx, name, ttl)
	if err != nil {
		log.Warn("scheduled work skipped: lease unavailable", "name", name, "err", err)
		return false
	}
	if !ok {
		log.Debug("scheduled work skipped: another instance holds it", "name", name)
		return false
	}
	fn()
	return true
}

// every runs fn on a ticker until ctx is cancelled. The first run is the
// caller's boot run, not this loop's: a job that has just run at startup should
// not run again one tick later because the ticker started at the same moment.
func every(ctx context.Context, d time.Duration, fn func()) {
	go func() {
		tick := time.NewTicker(d)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				fn()
			}
		}
	}()
}
