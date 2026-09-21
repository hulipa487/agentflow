package caps

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agentflow/internal/core/runtime"
	"agentflow/internal/core/session"
)

// Session state is a small durable key/value store scoped to one session: the
// place a loop puts what it must not forget — a cursor, a step count, the last
// thing it said — so it survives a restart, and a failover, without the loop
// inventing its own storage.
//
// It is the runtime store's generic row table with a session-scoped key, so it
// inherits durability and the fleet's shared store for free: a session that
// moves to another instance reads what it wrote on the first one. Values are
// JSON, written and returned as JSON, so a loop round-trips a table without a
// second encoding step.
//
// Declared like every other opt-in capability: a loop that wants this says
// `session.state` in its agent's `capabilities`, because state is a persistence
// surface (in a fleet, on a shared database) and not something to hand out by
// default.
const (
	// maxStateValue bounds one value. Small on purpose: this is a scratchpad
	// for a loop's bookkeeping, not a place to put a document — the file store
	// exists for that.
	maxStateValue = 64 << 10
	// maxStateKeys bounds a session's key count, so a loop with a bug cannot
	// grow the shared store without limit.
	maxStateKeys = 256
	// stateKeyPrefix namespaces these rows in the runtime store's key/value
	// table, which the file store also uses.
	stateKeyPrefix = "session"
)

// SessionStateHandlers serves the session.state.* ops. Store nil leaves them
// unavailable, so a deployment with no runtime store degrades rather than
// panicking.
type SessionStateHandlers struct {
	Store runtime.Store
}

// Handlers returns the session.state.* op handlers.
func (h SessionStateHandlers) Handlers() map[string]session.OpHandler {
	fail := func(err error) (string, bool) {
		b, _ := json.Marshal(err.Error())
		return string(b), false
	}
	return map[string]session.OpHandler{
		"session.state.set": func(ctx context.Context, op session.Op) (string, bool) {
			key, err := stateKey(ctx, op)
			if err != nil {
				return fail(err)
			}
			if op.Value == nil {
				return fail(fmt.Errorf("session.state.set needs a value"))
			}
			// The op carries the caller's Lua value, decoded from JSON, so
			// encoding it back is the storage form — and exactly what get
			// returns. A table round-trips without a second step in Lua.
			encoded, err := json.Marshal(op.Value)
			if err != nil {
				return fail(fmt.Errorf("session.state.set: %w", err))
			}
			if len(encoded) > maxStateValue {
				return fail(fmt.Errorf("session.state.set: value is %d bytes, over the %d-byte limit",
					len(encoded), maxStateValue))
			}
			value := string(encoded)
			row := stateRowKey(ctx, key)
			// A new key has to fit the session's budget; overwriting an
			// existing one does not, so a full session can still update what it
			// already has.
			if _, exists, err := h.Store.GetRow(ctx, row); err == nil && !exists {
				if n, err := h.count(ctx); err == nil && n >= maxStateKeys {
					return fail(fmt.Errorf("session.state.set: this session already holds %d keys", n))
				}
			}
			if err := h.Store.PutRow(ctx, row, value, time.Time{}); err != nil {
				return fail(err)
			}
			return "true", true
		},

		"session.state.get": func(ctx context.Context, op session.Op) (string, bool) {
			key, err := stateKey(ctx, op)
			if err != nil {
				return fail(err)
			}
			row, ok, err := h.Store.GetRow(ctx, stateRowKey(ctx, key))
			if err != nil {
				return fail(err)
			}
			if !ok {
				// A missing key is not an error: it is an unset value, and a
				// loop should not have to tell the two apart with pcall.
				return "null", true
			}
			return row.Value, true
		},

		"session.state.delete": func(ctx context.Context, op session.Op) (string, bool) {
			key, err := stateKey(ctx, op)
			if err != nil {
				return fail(err)
			}
			if err := h.Store.DeleteRow(ctx, stateRowKey(ctx, key)); err != nil {
				return fail(err)
			}
			return "true", true
		},

		"session.state.list": func(ctx context.Context, op session.Op) (string, bool) {
			skey := session.SessionKeyFromCtx(ctx)
			if skey == "" {
				return fail(fmt.Errorf("session.state.list: no session in context"))
			}
			rows, err := h.Store.ListRows(ctx, statePrefix(skey))
			if err != nil {
				return fail(err)
			}
			out := map[string]json.RawMessage{}
			for _, r := range rows {
				name := strings.TrimPrefix(r.Key, statePrefix(skey))
				out[name] = json.RawMessage(r.Value)
			}
			b, err := json.Marshal(out)
			if err != nil {
				return fail(err)
			}
			return string(b), true
		},
	}
}

// count reports how many keys the session holds.
func (h SessionStateHandlers) count(ctx context.Context) (int, error) {
	skey := session.SessionKeyFromCtx(ctx)
	if skey == "" {
		return 0, fmt.Errorf("no session in context")
	}
	rows, err := h.Store.ListRows(ctx, statePrefix(skey))
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

// statePrefix is the store-key prefix for one session's state.
func statePrefix(sessionKey string) string {
	return stateKeyPrefix + "|" + sessionKey + "|state|"
}

// stateRowKey is the store key for one entry.
func stateRowKey(ctx context.Context, key string) string {
	return statePrefix(session.SessionKeyFromCtx(ctx)) + key
}

// stateKey validates the caller's key. The charset excludes "|" so a key cannot
// forge a boundary in the composed store key and reach another session's rows,
// and the length bound keeps the key from being the payload.
func stateKey(ctx context.Context, op session.Op) (string, error) {
	if session.SessionKeyFromCtx(ctx) == "" {
		return "", fmt.Errorf("session.state: no session in context")
	}
	key := strings.TrimSpace(op.Key)
	if key == "" {
		return "", fmt.Errorf("session.state: a key is required")
	}
	if len(key) > 128 {
		return "", fmt.Errorf("session.state: key is longer than 128 characters")
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-' || c == ':':
		default:
			return "", fmt.Errorf("session.state: key %q may contain only letters, digits and . _ - :", key)
		}
	}
	return key, nil
}
