package supervisor

import (
	"log/slog"
	"time"

	"agentflow/internal/core/scheduler"
	"agentflow/internal/core/session"
)

// schedulerAdapter adapts the scheduler.Service to the session.SchedulerService
// interface. Timer fires are delivered as core-stamped timer messages into the
// owning session's mailbox.
type schedulerAdapter struct {
	svc *scheduler.Service
	sup *Supervisor
	log *slog.Logger
}

func newSchedulerAdapter(svc *scheduler.Service, sup *Supervisor, log *slog.Logger) *schedulerAdapter {
	return &schedulerAdapter{svc: svc, sup: sup, log: log}
}

func (a *schedulerAdapter) deliver(owner string, timerID scheduler.TimerID, scope session.SchedulerScope) {
	principal := scope.Principal
	if principal == "" {
		principal = "system:scheduler"
	}
	msg := session.Message{
		ID:   "timer:" + owner + ":" + string(timerID),
		Type: "timer",
		From: "system:scheduler",
		Ts:   time.Now().Unix(),
		Payload: map[string]any{
			"timer_id": string(timerID),
		},
		// Kind "timer" is deliberately NOT a maintenance provenance. A timer
		// scheduled by a tenant turn must run as that tenant, not with the
		// all-scope access engine triggers have. It carries the tenant and
		// membership captured when it was registered.
		Provenance: &session.Provenance{
			Kind:           "timer",
			Principal:      principal,
			UserUUID:       scope.UserUUID,
			MembershipUUID: scope.MembershipUUID,
		},
	}
	a.sup.mu.Lock()
	actor, ok := a.sup.sessions[owner]
	a.sup.mu.Unlock()
	if !ok {
		// Session exited; the scheduler's CancelOwner should have removed the
		// timer, but a fire may still be in flight. Drop it.
		return
	}
	select {
	case actor.Mailbox <- msg:
	default:
		a.log.Warn("timer mailbox full; dropping tick", "session", owner)
	}
}

func (a *schedulerAdapter) Every(owner string, intervalSec float64) (string, error) {
	return a.EveryScoped(owner, intervalSec, session.SchedulerScope{})
}

func (a *schedulerAdapter) EveryScoped(owner string, intervalSec float64, scope session.SchedulerScope) (string, error) {
	deliver := func(owner string, id scheduler.TimerID) { a.deliver(owner, id, scope) }
	id, err := a.svc.Every(owner, time.Duration(intervalSec*float64(time.Second)), deliver)
	if err != nil {
		return "", err
	}
	return string(id), nil
}

func (a *schedulerAdapter) After(owner string, delaySec float64) (string, error) {
	return a.AfterScoped(owner, delaySec, session.SchedulerScope{})
}

func (a *schedulerAdapter) AfterScoped(owner string, delaySec float64, scope session.SchedulerScope) (string, error) {
	deliver := func(owner string, id scheduler.TimerID) { a.deliver(owner, id, scope) }
	id, err := a.svc.After(owner, time.Duration(delaySec*float64(time.Second)), deliver)
	if err != nil {
		return "", err
	}
	return string(id), nil
}

func (a *schedulerAdapter) Cron(owner string, expr string) (string, error) {
	return a.CronScoped(owner, expr, session.SchedulerScope{})
}

func (a *schedulerAdapter) CronScoped(owner string, expr string, scope session.SchedulerScope) (string, error) {
	deliver := func(owner string, id scheduler.TimerID) { a.deliver(owner, id, scope) }
	id, err := a.svc.Cron(owner, expr, deliver)
	if err != nil {
		return "", err
	}
	return string(id), nil
}

func (a *schedulerAdapter) Cancel(owner string, timerID string) error {
	return a.svc.Cancel(scheduler.TimerID(timerID))
}

// CancelOwner cancels all timers owned by a session. Called by the lifecycle
// cleanup when a session exits.
func (a *schedulerAdapter) CancelOwner(owner string) {
	a.svc.CancelOwner(owner)
}

// Ensure the adapter satisfies both interfaces.
var _ session.SchedulerService = (*schedulerAdapter)(nil)
var _ session.ScopedSchedulerService = (*schedulerAdapter)(nil)
