// Package triggers schedules a deployment's every:/cron: triggers. It is
// engine infrastructure: firing a trigger enqueues an ordinary session.Message
// into the target profile's mailbox, exactly as an inbound channel event would
// be. No agent holds a capability for this and no loop has to run a timer loop
// of its own, so a configdir deployment can drop its user-space scheduler
// agent.
//
// A trigger with event: is not scheduled here — routes deliver those. The
// merged list keeps being served to Lua as read-only data (runtime.triggers())
// unchanged, so loops and routers that read schedules today keep working.
package triggers

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strconv"
	"sync"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/session"
)

// Deliver enqueues a fired trigger's message into the target session. main
// wires it to supervisor.Deliver.
type Deliver func(agent, key string, msg session.Message) error

// Lookup resolves a trigger's target.profile to the supervisor agent address
// that owns it: "pm" stays "pm" for a configured agent, or becomes "spawn:pm"
// for a spawn profile. ok=false means the profile is unknown.
type Lookup func(profile string) (agent string, ok bool)

// minEvery is the shortest accepted every: interval, mirroring scheduler.every
// — a sub-100ms scheduled task is a mistake, not a schedule.
const minEvery = 100 * time.Millisecond

// defaultPoll is how often a configdir's trigger files are re-read.
const defaultPoll = time.Second

// Service owns the live trigger set: one goroutine per scheduled trigger,
// started at boot and re-diffed on Reload.
type Service struct {
	lookup  Lookup
	deliver Deliver
	loc     *time.Location
	log     *slog.Logger
	poll    time.Duration

	mu   sync.Mutex
	ctx  context.Context
	jobs map[string]*job
	// list is the declaration set the last Reload applied (event triggers
	// included), used by the watcher to detect an unchanged file.
	list []config.Trigger
}

// job is one scheduled trigger and its live goroutine.
type job struct {
	trigger config.Trigger
	kind    string // "cron" | "every"
	agent   string // resolved supervisor agent address
	key     string // session key: the trigger name
	sched   *Schedule
	every   time.Duration
	cancel  context.CancelFunc
}

// New builds the service. offset is the instance UTC offset cron fields are
// matched in (0 = UTC); lookups and deliveries happen through the callbacks.
func New(lookup Lookup, deliver Deliver, offset time.Duration, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Service{
		lookup:  lookup,
		deliver: deliver,
		loc:     scheduleZone(offset),
		log:     log.With("module", "triggers"),
		poll:    defaultPoll,
		jobs:    map[string]*job{},
	}
}

// scheduleZone turns an instance UTC offset into the fixed zone cron matching
// happens in. It is deliberately a fixed offset and not a named zone: a
// deployment's "09:00" should not move when a region changes its DST rules.
func scheduleZone(offset time.Duration) *time.Location {
	if offset == 0 {
		return time.UTC
	}
	name := fmt.Sprintf("UTC%+g", offset.Hours())
	return time.FixedZone(name, int(offset.Seconds()))
}

// Start fixes the context job goroutines live under; cancelling it stops every
// trigger. Call it before Reload.
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
}

// Reload replaces the scheduled trigger set. The new list is diffed against the
// live one: an unchanged declaration keeps its running timer (it is never
// re-anchored), a changed or new one starts fresh, and a removed one is
// cancelled. A trigger that cannot be scheduled (bad expression, unknown
// target) is logged and skipped — it never fails the boot, because triggers
// are also plain data that loops may interpret themselves.
func (s *Service) Reload(list []config.Trigger) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ctx == nil {
		s.log.Error("triggers: Reload before Start; nothing scheduled")
		return
	}

	want := map[string]*job{}
	for _, tr := range list {
		if tr.Cron == "" && tr.Every == "" {
			continue // event triggers are routed, not scheduled
		}
		j := s.build(tr)
		if j == nil {
			continue // build logged the reason
		}
		want[tr.Name] = j
	}

	// Carry over unchanged declarations so their timers keep their anchor.
	for name, old := range s.jobs {
		keep, ok := want[name]
		if ok && keep.sameAs(old) {
			want[name] = old
			continue
		}
		old.cancel()
		if ok {
			s.log.Info("trigger rescheduled", "trigger", name)
		} else {
			s.log.Info("trigger removed", "trigger", name)
		}
	}

	var start []*job
	for name, j := range want {
		if old, live := s.jobs[name]; live && old == j {
			continue
		}
		start = append(start, j)
	}
	s.jobs = want
	s.list = list
	for _, j := range start {
		j.start(s)
	}
}

// Pending returns the number of live scheduled triggers.
func (s *Service) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.jobs)
}

// Stop cancels every scheduled trigger.
func (s *Service) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		j.cancel()
	}
	s.jobs = map[string]*job{}
}

// WatchConfigDir polls <dir>/triggers/*.yaml and reloads the scheduled set
// whenever the merged list changes, so an edit to a trigger file takes effect
// without a restart. Only the trigger list is re-read — the rest of a configdir
// still needs a restart. A malformed fragment keeps the running set and logs
// the error once per distinct message.
func (s *Service) WatchConfigDir(ctx context.Context, dir string) {
	s.mu.Lock()
	last := s.list
	s.mu.Unlock()

	var lastErr string
	tick := time.NewTicker(s.poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		list, err := config.LoadTriggers(dir, s.log)
		if err != nil {
			if msg := err.Error(); msg != lastErr {
				lastErr = msg
				s.log.Warn("trigger reload failed; keeping the running set", "dir", dir, "err", err)
			}
			continue
		}
		lastErr = ""
		if reflect.DeepEqual(list, last) {
			continue
		}
		last = list
		s.log.Info("triggers reloaded", "dir", dir, "triggers", len(list))
		s.Reload(list)
	}
}

// build parses one declaration into a job, logging and returning nil when it
// cannot be scheduled.
func (s *Service) build(tr config.Trigger) *job {
	switch {
	case tr.Every != "" && tr.Cron != "":
		s.log.Warn("trigger skipped: every and cron are mutually exclusive", "trigger", tr.Name)
		return nil
	case tr.Every != "":
		d, err := ParseEvery(tr.Every)
		if err != nil {
			s.log.Warn("trigger skipped: bad every interval", "trigger", tr.Name, "every", tr.Every, "err", err)
			return nil
		}
		if d < minEvery {
			s.log.Warn("trigger skipped: every interval is below the minimum",
				"trigger", tr.Name, "every", tr.Every, "min", minEvery.String())
			return nil
		}
		agent, ok := s.target(tr)
		if !ok {
			return nil
		}
		return &job{trigger: tr, kind: "every", agent: agent, key: tr.Name, every: d}
	case tr.Cron != "":
		sched, err := ParseCron(tr.Cron, s.loc)
		if err != nil {
			s.log.Warn("trigger skipped: bad cron expression", "trigger", tr.Name, "cron", tr.Cron, "err", err)
			return nil
		}
		agent, ok := s.target(tr)
		if !ok {
			return nil
		}
		return &job{trigger: tr, kind: "cron", agent: agent, key: tr.Name, sched: sched}
	}
	return nil
}

// target resolves the trigger's target.profile, warning when it names nothing
// the supervisor can deliver to.
func (s *Service) target(tr config.Trigger) (string, bool) {
	if tr.Target.Profile == "" {
		s.log.Warn("trigger skipped: no target profile", "trigger", tr.Name)
		return "", false
	}
	if s.lookup == nil {
		s.log.Warn("trigger skipped: no target lookup installed", "trigger", tr.Name)
		return "", false
	}
	agent, ok := s.lookup(tr.Target.Profile)
	if !ok {
		s.log.Warn("trigger skipped: target profile is neither a configured agent nor a spawn profile",
			"trigger", tr.Name, "profile", tr.Target.Profile)
		return "", false
	}
	return agent, true
}

// sameAs reports whether two jobs carry the same declaration (and therefore
// the same schedule), so Reload can keep the older one running.
func (j *job) sameAs(other *job) bool {
	return other != nil && j.agent == other.agent && reflect.DeepEqual(j.trigger, other.trigger)
}

func (j *job) start(s *Service) {
	ctx, cancel := context.WithCancel(s.ctx)
	j.cancel = cancel
	go s.run(ctx, j)
}

func (s *Service) run(ctx context.Context, j *job) {
	if j.kind == "every" {
		t := time.NewTicker(j.every)
		defer t.Stop()
		if j.trigger.RunOnBoot {
			s.fire(j)
		}
		for {
			select {
			case <-t.C:
				s.fire(j)
			case <-ctx.Done():
				return
			}
		}
	}

	if j.trigger.RunOnBoot {
		s.fire(j)
	}
	for {
		next := j.sched.Next(time.Now())
		if next.IsZero() {
			s.log.Warn("trigger has no future occurrence; stopping",
				"trigger", j.trigger.Name, "cron", j.sched.Expression())
			return
		}
		wait := time.Until(next)
		if wait < 0 {
			wait = 0
		}
		s.log.Debug("trigger scheduled", "trigger", j.trigger.Name, "cron", j.sched.Expression(),
			"next", next.Format(time.RFC3339), "in", wait.Round(time.Second).String())
		t := time.NewTimer(wait)
		select {
		case <-t.C:
			s.fire(j)
		case <-ctx.Done():
			t.Stop()
			return
		}
	}
}

// fire delivers one occurrence: a Message{type:"cron", payload:<trigger
// payload>} into the target session. The trigger name travels in the message
// id and provenance principal, never merged into the payload — a loop sees
// exactly the payload the deployment declared.
func (s *Service) fire(j *job) {
	now := time.Now()
	msg := session.Message{
		ID:      "cron:" + j.trigger.Name + ":" + strconv.FormatInt(now.Unix(), 10),
		Type:    "cron",
		From:    "system:scheduler",
		Payload: copyPayload(j.trigger.Payload),
		Ts:      now.Unix(),
		Provenance: &session.Provenance{
			Kind:      "scheduler",
			Principal: "trigger:" + j.trigger.Name,
		},
	}
	metrics.Inc("agentflow_trigger_fires")
	s.log.Info("trigger fired", "trigger", j.trigger.Name, "kind", j.kind, "agent", j.agent, "key", j.key)
	if s.deliver == nil {
		return
	}
	if err := s.deliver(j.agent, j.key, msg); err != nil {
		s.log.Warn("trigger delivery failed", "trigger", j.trigger.Name, "agent", j.agent, "err", err)
	}
}

// copyPayload hands every fire its own map so a consumer can never mutate the
// declaration (or another fire's message).
func copyPayload(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
