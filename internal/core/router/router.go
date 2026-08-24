// Package router runs the gateway.route handler (builtin:per_chat by
// default) in a singleton Luau service state. Channel drivers submit inbound
// events; the Lua route handler computes a session key; the router resolves
// and forwards through the supervisor.
package router

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"agentflow/internal/core/session"
	"agentflow/internal/core/supervisor"
	"agentflow/internal/vm"
)

// Inbound is one event from a channel driver, before routing.
type Inbound struct {
	Channel string          `json:"channel"`
	Agent   string          `json:"agent"`
	Message session.Message `json:"message"`
}

// Sink is the inbound side of the router (implemented by Router).
type Sink interface {
	Submit(Inbound)
}

// Router owns the routing service state.
type Router struct {
	src     string
	sup     *supervisor.Supervisor
	mailbox chan Inbound
	log     *slog.Logger
}

func New(src string, sup *supervisor.Supervisor, log *slog.Logger) *Router {
	return &Router{
		src:     src,
		sup:     sup,
		mailbox: make(chan Inbound, 256),
		log:     log.With("module", "router"),
	}
}

// Submit queues an inbound event. A full queue drops the event — the
// alternative is unbounded memory growth under flood; drops are logged.
func (r *Router) Submit(in Inbound) {
	select {
	case r.mailbox <- in:
	default:
		r.log.Warn("router queue full, dropping event", "channel", in.Channel)
	}
}

// Run drives the routing loop until ctx is done; crashes restart the state
// (routing is stateless, so a fresh state loses nothing).
func (r *Router) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if crashed := r.runOnce(ctx); crashed {
			r.log.Warn("router crashed; restarting in 1s")
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

func (r *Router) runOnce(ctx context.Context) bool {
	st := vm.New(5_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		r.log.Warn("prelude load failed", "err", err)
		return true
	}

	status, msg := st.Start("loop", r.src)
	r.log.Info("router started")

	for {
		switch status {
		case vm.Finished:
			r.log.Info("router loop finished")
			return false
		case vm.Failed:
			r.log.Warn("router loop error", "err", msg)
			return true
		}

		var op struct {
			Type    string          `json:"type"`
			Level   string          `json:"level"`
			Msg     string          `json:"msg"`
			Agent   string          `json:"agent"`
			Key     string          `json:"key"`
			Message session.Message `json:"message"`
		}
		if err := json.Unmarshal([]byte(msg), &op); err != nil {
			r.log.Warn("bad op from router lua", "err", err, "raw", msg)
			return true
		}

		resp := "true"
		ok := true
		switch op.Type {
		case "inbox":
			select {
			case in := <-r.mailbox:
				b, _ := json.Marshal(map[string]any{"message": in.Message, "agent": in.Agent})
				resp = string(b)
			case <-ctx.Done():
				return false
			}
		case "deliver":
			// Delivery failures are logged but don't fail the op: a bad agent
			// name must not crash-loop the (shared, singleton) router state.
			if err := r.sup.Deliver(op.Agent, op.Key, op.Message); err != nil {
				r.log.Warn("deliver failed", "agent", op.Agent, "key", op.Key, "err", err)
			}
		case "log":
			r.log.Log(ctx, levelOf(op.Level), op.Msg)
		default:
			b, _ := json.Marshal("unknown op " + op.Type)
			resp, ok = string(b), false
		}

		status, msg = st.Resume(resp, ok)
	}
}

func levelOf(l string) slog.Level {
	switch l {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
