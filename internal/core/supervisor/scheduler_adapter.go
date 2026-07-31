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
	svc    *scheduler.Service
	sup    *Supervisor
	log    *slog.Logger
}

func newSchedulerAdapter(svc *scheduler.Service, sup *Supervisor, log *slog.Logger) *schedulerAdapter {
	return &schedulerAdapter{svc: svc, sup: sup, log: log}
}

func (a *schedulerAdapter) deliver(owner string) {
	msg := session.Message{
		ID:   "timer:" + owner,
		Type: "timer",
		From: "system:scheduler",
		Ts:   time.Now().Unix(),
		Provenance: &session.Provenance{
			Kind:      "scheduler",
			Principal: "system:scheduler",
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
	id, err := a.svc.Every(owner, time.Duration(intervalSec*float64(time.Second)), a.deliver)
	if err != nil {
		return "", err
	}
	return string(id), nil
}

func (a *schedulerAdapter) After(owner string, delaySec float64) (string, error) {
	id, err := a.svc.After(owner, time.Duration(delaySec*float64(time.Second)), a.deliver)
	if err != nil {
		return "", err
	}
	return string(id), nil
}

func (a *schedulerAdapter) Cron(owner string, expr string) (string, error) {
	id, err := a.svc.Cron(owner, expr, a.deliver)
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

// Ensure the adapter satisfies the interface.
var _ session.SchedulerService = (*schedulerAdapter)(nil)
