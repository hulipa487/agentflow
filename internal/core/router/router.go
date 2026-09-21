// Package router runs the gateway.route handler (plugin:per_chat by
// default) in a singleton Luau service state. Channel drivers submit inbound
// events; the Lua route handler computes a session key; the router resolves
// and forwards through the supervisor.
//
// A route handler has to be stateless, in the sense that matters for a
// deployment running more than one instance: this state is per process, and
// whichever instance receives an inbound is the one that runs the handler. Two
// instances routing the same message must reach the same session key, so a
// handler that remembers per-chat state in Lua will give each instance its own
// answer. The session is where state that has to persist belongs; the router's
// job is to decide where a message goes.
package router

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

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

	// triggers is the prebuilt runtime.triggers response (the same JSON the
	// agent-facing op returns). Route Lua calls runtime.triggers() to read the
	// deployment's trigger list instead of having event routes baked into
	// source. Empty (the default) answers an empty list.
	triggers string

	// Journal, when set, records every inbound event (routed or dropped) to
	// the core-owned message journal. Set once at boot; channel drivers never
	// see it, so a misbehaving channel cannot bypass the audit trail.
	Journal func(in Inbound, status string)
}

// New builds the router. triggersResp is the runtime.triggers op body shared
// with the agent surface (caps.TriggersResponse(cfg.Triggers)); pass "" when
// the deployment has no triggers.
func New(src, triggersResp string, sup *supervisor.Supervisor, log *slog.Logger) *Router {
	if triggersResp == "" {
		triggersResp = `{"ok":true,"triggers":[]}`
	}
	return &Router{
		src:      src,
		sup:      sup,
		mailbox:  make(chan Inbound, 256),
		triggers: triggersResp,
		log:      log.With("module", "router"),
	}
}

// Submit queues an inbound event. The message id is stamped here (the single
// ingress choke point) so the journal, the session, and Lua all see the same
// id — the item_id an audit lookup joins on. A full queue drops the event —
// the alternative is unbounded memory growth under flood; drops are logged
// and journaled.
func (r *Router) Submit(in Inbound) {
	if in.Message.ID == "" {
		in.Message.ID = uuid.NewString()
	}
	if in.Message.Channel == "" {
		in.Message.Channel = in.Channel
	}
	select {
	case r.mailbox <- in:
		if r.Journal != nil {
			r.Journal(in, "routed")
		}
	default:
		r.log.Warn("router queue full, dropping event", "channel", in.Channel)
		if r.Journal != nil {
			r.Journal(in, "dropped_queue")
		}
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
		case "runtime.triggers":
			// Same response shape as the agent-facing op, so route Lua and loop
			// Lua read the merged trigger list identically.
			resp = r.triggers
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
