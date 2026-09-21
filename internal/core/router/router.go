// Package router runs the gateway.route handler (plugin:per_chat by
// default) in a singleton Luau service state. Channel drivers submit inbound
// events; the Lua route handler computes a session key; the router resolves
// and forwards through the supervisor.
//
// The handler's Lua state is per process: whichever instance receives an
// inbound is the one that runs the handler, so anything it remembers there
// gives each instance its own answer, and a restart forgets it. What it needs
// to remember goes in route state instead — router.state, kept in the shared
// store (see the route state section below) — and everything else about a
// handler has to be a function of the message and the configuration. Two
// instances routing the same message must reach the same session key.
package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"agentflow/internal/core/runtime"
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

	// state is where route state lives. Nil — a deployment with no runtime
	// store — leaves route.state.* unavailable, which the ops report rather
	// than answering "null" for everything.
	state runtime.Store

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

// SetStateStore installs the store route state lives in. main calls it once the
// runtime store is open; without one the route.state.* ops fail with a clear
// message instead of answering "null" for everything.
func (r *Router) SetStateStore(st runtime.Store) { r.state = st }

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
	// The route handler's own surface, on top of the shared prelude: it exists
	// only in this state, so an agent loop never reaches an op that is not its
	// business.
	if err := st.Eval("@route_state", routeStateLua); err != nil {
		r.log.Warn("route state api load failed", "err", err)
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
			// route.state.set carries the handler's value through untouched and
			// an optional lifetime in seconds.
			Value      json.RawMessage `json:"value"`
			TTLSeconds *float64        `json:"ttl_seconds"`
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
		case "router.state.get":
			resp, ok = r.stateGet(ctx, op.Key)
		case "router.state.set":
			resp, ok = r.stateSet(ctx, op.Key, op.Value, op.TTLSeconds)
		case "router.state.delete":
			resp, ok = r.stateDelete(ctx, op.Key)
		case "router.state.list":
			resp, ok = r.stateList(ctx, op.Key)
		case "log":
			r.log.Log(ctx, levelOf(op.Level), op.Msg)
		default:
			resp, ok = failJSON(fmt.Errorf("unknown op %s", op.Type)), false
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

// Route state is the route handler's memory, and it lives in the shared store
// rather than in the handler's Lua state.
//
// A handler's Lua state is per process: whichever instance receives an inbound
// runs the handler, so a handler that remembers anything — which agent a chat
// was last routed to, how many turns it has seen — gives each instance its own
// answer, and a restart forgets. This is where that memory goes instead, so
// every instance reads the same value and the answer survives a failover.
//
// It is consistency, not serialization. Two instances can interleave a
// read-modify-write on one key; what the engine serializes is delivery, by the
// session lease, once the handler has decided where a message goes. A handler
// that needs a strict order should route the related messages to one session —
// the session is the unit of ordering here.
//
// State is bounded by time rather than by a key count: a key is kept for
// defaultRouteStateTTL after its last write, so a handler keyed by something
// high-cardinality (a message id, a timestamp) cannot grow the shared store
// without limit. A handler that wants a key kept for good passes ttl_seconds =
// 0.
const (
	routeStatePrefix     = "route|state|"
	maxRouteStateKeyLen  = 128
	maxRouteStateValue   = 64 << 10
	maxRouteStateList    = 1000
	defaultRouteStateTTL = 30 * 24 * time.Hour
)

// routeStateLua defines the route handler's own API. It is loaded into the
// router's service state on top of the shared prelude, so the namespace exists
// exactly where it belongs: an agent loop calling router.state gets a Lua error
// naming an undefined global, not an op the engine quietly refuses.
//
// A key may contain letters, digits and . _ - : (128 characters); a value is
// any JSON-encodable Lua value up to 64 KiB. get returns nil for a key that was
// never set (an unset key is not an error, so a handler needs no pcall).
const routeStateLua = `
router = {}
router.state = {}
function router.state.get(key)
  return af.op({ type = "router.state.get", key = key })
end
function router.state.set(key, value, opts)
  opts = opts or {}
  return af.op({ type = "router.state.set", key = key, value = value,
                 ttl_seconds = opts.ttl_seconds })
end
function router.state.delete(key)
  return af.op({ type = "router.state.delete", key = key })
end
function router.state.list(prefix)
  return af.op({ type = "router.state.list", key = prefix })
end
`

// stateGet reads one key's value as stored, or "null" when it was never set.
func (r *Router) stateGet(ctx context.Context, key string) (string, bool) {
	st, row, err := r.routeState(key)
	if err != nil {
		return failJSON(err), false
	}
	got, ok, err := st.GetRow(ctx, row)
	if err != nil {
		return failJSON(err), false
	}
	if !ok {
		return "null", true
	}
	return got.Value, true
}

// stateSet writes one key. The value crosses the bridge as JSON and is stored
// as it arrived, so a table round-trips without a second encoding step in Lua.
func (r *Router) stateSet(ctx context.Context, key string, value json.RawMessage, ttl *float64) (string, bool) {
	st, row, err := r.routeState(key)
	if err != nil {
		return failJSON(err), false
	}
	if len(value) == 0 || string(value) == "null" {
		return failJSON(fmt.Errorf("route.state.set needs a value; to unset a key use route.state.delete")), false
	}
	if len(value) > maxRouteStateValue {
		return failJSON(fmt.Errorf("route.state.set: value is %d bytes, over the %d-byte limit",
			len(value), maxRouteStateValue)), false
	}
	expires := time.Now().Add(defaultRouteStateTTL)
	if ttl != nil {
		switch {
		case *ttl < 0:
			return failJSON(fmt.Errorf("route.state.set: ttl_seconds cannot be negative")), false
		case *ttl == 0:
			expires = time.Time{} // kept until it is deleted
		default:
			expires = time.Now().Add(time.Duration(*ttl * float64(time.Second)))
		}
	}
	if err := st.PutRow(ctx, row, string(value), expires); err != nil {
		return failJSON(err), false
	}
	return "true", true
}

// stateDelete removes one key. Deleting a key that is not there is not an
// error: the handler asked for it to be gone, and it is.
func (r *Router) stateDelete(ctx context.Context, key string) (string, bool) {
	st, row, err := r.routeState(key)
	if err != nil {
		return failJSON(err), false
	}
	if err := st.DeleteRow(ctx, row); err != nil {
		return failJSON(err), false
	}
	return "true", true
}

// stateList returns the handler's keys under a prefix — or every key it holds,
// when the prefix is empty — as a JSON object keyed by the handler's own key.
// The bound is a guard on the bridge rather than on the store: a handler that
// lists a whole namespace crosses it in one response, and the loop should be
// told to narrow the prefix instead of the VM holding a megabyte of JSON.
func (r *Router) stateList(ctx context.Context, prefix string) (string, bool) {
	if r.state == nil {
		return failJSON(errNoRouteState), false
	}
	scan := routeStatePrefix
	if clean := strings.TrimSpace(prefix); clean != "" {
		k, err := routeStateKey(clean)
		if err != nil {
			return failJSON(err), false
		}
		scan += k
	}
	rows, err := r.state.ListRows(ctx, scan)
	if err != nil {
		return failJSON(err), false
	}
	if len(rows) > maxRouteStateList {
		return failJSON(fmt.Errorf("route.state.list: %d keys match; narrow the prefix (at most %d are returned)",
			len(rows), maxRouteStateList)), false
	}
	out := make(map[string]json.RawMessage, len(rows))
	for _, row := range rows {
		out[strings.TrimPrefix(row.Key, routeStatePrefix)] = json.RawMessage(row.Value)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return failJSON(err), false
	}
	return string(b), true
}

// routeState resolves a handler key to its store row, and checks that route
// state is available at all: a deployment with no runtime store has none, and
// says so rather than answering "null" for every key.
func (r *Router) routeState(key string) (runtime.Store, string, error) {
	if r.state == nil {
		return nil, "", errNoRouteState
	}
	row, err := routeStateRow(key)
	if err != nil {
		return nil, "", err
	}
	return r.state, row, nil
}

// errNoRouteState is what every route.state op reports when the deployment has
// no runtime store to keep state in.
var errNoRouteState = errors.New("route.state is unavailable: this deployment has no runtime store")

// routeStateRow composes the store row for a validated handler key.
func routeStateRow(key string) (string, error) {
	k, err := routeStateKey(key)
	if err != nil {
		return "", err
	}
	return routeStatePrefix + k, nil
}

// routeStateKey validates the handler's key. The charset excludes "|" so a key
// cannot forge a boundary in the store key, and the length bound keeps a key
// from being the payload.
func routeStateKey(key string) (string, error) {
	k := strings.TrimSpace(key)
	if k == "" {
		return "", fmt.Errorf("route.state: a key is required")
	}
	if len(k) > maxRouteStateKeyLen {
		return "", fmt.Errorf("route.state: key is longer than %d characters", maxRouteStateKeyLen)
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-' || c == ':':
		default:
			return "", fmt.Errorf("route.state: key %q may contain only letters, digits and . _ - :", k)
		}
	}
	return k, nil
}

// failJSON encodes an error the way the router's ops report one: as a JSON
// string, which the Lua side raises with.
func failJSON(err error) string {
	b, _ := json.Marshal(err.Error())
	return string(b)
}
