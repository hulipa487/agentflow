// Package scheduler owns session-scoped timers. Timers never invoke a Luau
// state directly; they enqueue normal session.Message{Type:"timer"} messages
// to the owning session's mailbox. This preserves the one-goroutine-per-state
// invariant and keeps cancellation deterministic.
package scheduler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// TimerID identifies a registered timer.
type TimerID string

// Service manages all session-owned timers.
type Service struct {
	mu      sync.Mutex
	timers  map[TimerID]*timer
	byOwner map[string]map[TimerID]bool
	log     *slog.Logger
}

type timer struct {
	id     TimerID
	owner  string
	kind   string // "every" | "after" | "cron"
	cancel context.CancelFunc
}

// Deliver is the callback the scheduler uses to enqueue a timer message.
// The supervisor provides this; it stamps provenance and routes to the mailbox.
// The timer id travels with every fire so a session running multiple timers
// can tell their messages apart.
type Deliver func(owner string, id TimerID)

func New(log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Service{
		timers:  map[TimerID]*timer{},
		byOwner: map[string]map[TimerID]bool{},
		log:     log.With("module", "scheduler"),
	}
}

// Every registers a repeating timer. The first fire happens after interval.
// The floor is 100ms, which is what the trigger service uses too — this comment
// said "1 second for production safety" while the check below enforced 100ms.
func (s *Service) Every(owner string, interval time.Duration, deliver Deliver) (TimerID, error) {
	if interval < 100*time.Millisecond {
		return "", fmt.Errorf("scheduler.every: interval must be >= 100ms, got %v", interval)
	}
	id := TimerID("timer-" + uuid.NewString()[:8])
	_, err := s.registerWithID(id, owner, "every", func(ctx context.Context) {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				deliver(owner, id)
			case <-ctx.Done():
				return
			}
		}
	})
	return id, err
}

// After registers a one-shot timer.
func (s *Service) After(owner string, delay time.Duration, deliver Deliver) (TimerID, error) {
	if delay < 0 {
		return "", fmt.Errorf("scheduler.after: delay must be >= 0, got %v", delay)
	}
	id := TimerID("timer-" + uuid.NewString()[:8])
	_, err := s.registerWithID(id, owner, "after", func(ctx context.Context) {
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-t.C:
			deliver(owner, id)
			s.removeSelf(id)
		case <-ctx.Done():
			return
		}
	})
	return id, err
}

// Cron registers a cron-expression timer. Phase 4b supports a simple subset:
// "*/N * * * *" (every N minutes) and fixed 5-field minute-hour expressions.
// A full cron parser can replace this later; the interface stays the same.
func (s *Service) Cron(owner string, expr string, deliver Deliver) (TimerID, error) {
	schedule, err := parseCron(expr)
	if err != nil {
		return "", err
	}
	id := TimerID("timer-" + uuid.NewString()[:8])
	_, err = s.registerWithID(id, owner, "cron", func(ctx context.Context) {
		for {
			next := schedule.next(time.Now())
			if next.IsZero() {
				return
			}
			t := time.NewTimer(time.Until(next))
			select {
			case <-t.C:
				t.Stop()
				deliver(owner, id)
			case <-ctx.Done():
				t.Stop()
				return
			}
		}
	})
	return id, err
}

// Cancel removes a single timer.
func (s *Service) Cancel(id TimerID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.timers[id]
	if !ok {
		return fmt.Errorf("unknown timer %q", id)
	}
	t.cancel()
	delete(s.timers, id)
	if owners, ok := s.byOwner[t.owner]; ok {
		delete(owners, id)
		if len(owners) == 0 {
			delete(s.byOwner, t.owner)
		}
	}
	return nil
}

// CancelOwner removes every timer owned by a session. Called by the lifecycle
// cleanup when a session exits.
func (s *Service) CancelOwner(owner string) {
	s.mu.Lock()
	ids := make([]TimerID, 0, len(s.byOwner[owner]))
	for id := range s.byOwner[owner] {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		_ = s.Cancel(id)
	}
}

// Pending returns the total number of live timers (for metrics/tests).
func (s *Service) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.timers)
}

func (s *Service) registerWithID(id TimerID, owner string, kind string, run func(context.Context)) (TimerID, error) {
	if owner == "" {
		return "", fmt.Errorf("scheduler: owner is required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &timer{id: id, owner: owner, kind: kind, cancel: cancel}
	s.mu.Lock()
	s.timers[id] = t
	if s.byOwner[owner] == nil {
		s.byOwner[owner] = map[TimerID]bool{}
	}
	s.byOwner[owner][id] = true
	s.mu.Unlock()
	go run(ctx)
	s.log.Debug("timer registered", "id", id, "owner", owner, "kind", kind)
	return id, nil
}

func (s *Service) removeSelf(id TimerID) {
	s.mu.Lock()
	t, ok := s.timers[id]
	if ok {
		delete(s.timers, id)
		if owners, ok := s.byOwner[t.owner]; ok {
			delete(owners, id)
			if len(owners) == 0 {
				delete(s.byOwner, t.owner)
			}
		}
	}
	s.mu.Unlock()
}

// --- cron subset ---

type cronSchedule struct {
	minute, hour string
}

func parseCron(expr string) (*cronSchedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron: expected 5 fields, got %d", len(fields))
	}
	// Only minute and hour are honoured, so the other three must be "*". The
	// bound used to start at index 3, which checked day-of-month and month and
	// skipped day-of-week entirely — so "0 9 * * 1" was accepted and then fired
	// every day.
	for i := 2; i < 5; i++ {
		if fields[i] != "*" {
			return nil, fmt.Errorf("cron: day-of-month, month and day-of-week must be \"*\" (got %q in field %d)", fields[i], i+1)
		}
	}
	hour := fields[1]
	if hour != "*" {
		h, err := strconv.Atoi(hour)
		if err != nil || h < 0 || h > 23 {
			return nil, fmt.Errorf("cron: hour %q must be \"*\" or 0-23", hour)
		}
	}
	minute := fields[0]
	if strings.HasPrefix(minute, "*/") {
		// A step cannot be anchored to an hour, so next() discards the hour for
		// this form. Refuse the pair rather than firing around the clock: that
		// is what "*/15 9-17 * * *" used to do.
		if hour != "*" {
			return nil, fmt.Errorf("cron: minute %q needs hour \"*\" (got %q) — a step cannot be anchored to an hour; use a fixed minute", minute, hour)
		}
		if n, err := strconv.Atoi(minute[2:]); err != nil || n <= 0 {
			return nil, fmt.Errorf("cron: minute step %q must be */N with N > 0", minute)
		}
	} else if _, err := strconv.Atoi(minute); err != nil {
		// Sscanf used to leave m at 0 for anything unparseable, so a list or a
		// range silently became "minute 0" instead of being refused.
		return nil, fmt.Errorf("cron: minute %q must be N or */N", minute)
	}
	return &cronSchedule{minute: minute, hour: hour}, nil
}

func (c *cronSchedule) next(now time.Time) time.Time {
	// Simplified: only "*/N" minute and "*" hour supported.
	if strings.HasPrefix(c.minute, "*/") {
		var n int
		fmt.Sscanf(c.minute[2:], "%d", &n)
		if n <= 0 {
			n = 1
		}
		return now.Add(time.Duration(n) * time.Minute)
	}
	// Fixed minute/hour: compute next occurrence today or tomorrow.
	var m, h int
	fmt.Sscanf(c.minute, "%d", &m)
	fmt.Sscanf(c.hour, "%d", &h)
	next := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}
