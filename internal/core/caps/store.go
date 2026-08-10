// Package caps adapts drivers to session op handlers.
package caps

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agentflow/internal/core/memory"
	"agentflow/internal/core/session"
)

// StoreHandlers returns store op handlers bound to one agent's memory profile.
func StoreHandlers(am *memory.AgentMemory, mgr *memory.Manager) map[string]session.OpHandler {
	fail := func(err error) (string, bool) {
		b, _ := json.Marshal(err.Error())
		return string(b), false
	}
	okJSON := func(v any) (string, bool) {
		b, err := json.Marshal(v)
		if err != nil {
			return fail(err)
		}
		return string(b), true
	}
	resolve := func(table string) (memory.BackendHandle, memory.StoreBinding, error) {
		if am == nil {
			return nil, memory.StoreBinding{}, fmt.Errorf("agent has no memory profile")
		}
		return mgr.BindForTable(*am, table)
	}

	return map[string]session.OpHandler{
		"store.put": func(ctx context.Context, op session.Op) (string, bool) {
			h, bind, err := resolve(op.Table)
			if err != nil {
				return fail(err)
			}
			opts := memory.PutOpts{TTL: time.Duration(op.TTL) * time.Second}
			if opts.TTL <= 0 && bind.Retention > 0 {
				opts.TTL = bind.Retention
			}
			if len(op.Vector) > 0 {
				// A vector sent to a non-vector backend must fail loudly:
				// storing without it would silently poison later recall.
				supported := false
				for _, f := range mgr.Features(bind.Backend) {
					if f == "vector" {
						supported = true
						break
					}
				}
				if !supported {
					return fail(fmt.Errorf("store %q: backend %q does not support vector storage", op.Table, bind.Backend))
				}
				opts.Vector = op.Vector
			}
			if err := h.Put(bind.Table, op.Key, op.Value, opts); err != nil {
				return fail(err)
			}
			return "true", true
		},

		"store.get": func(ctx context.Context, op session.Op) (string, bool) {
			h, bind, err := resolve(op.Table)
			if err != nil {
				return fail(err)
			}
			v, found, err := h.Get(bind.Table, op.Key)
			if err != nil {
				return fail(err)
			}
			return okJSON(map[string]any{"value": v, "found": found})
		},

		"store.delete": func(ctx context.Context, op session.Op) (string, bool) {
			h, bind, err := resolve(op.Table)
			if err != nil {
				return fail(err)
			}
			if err := h.Delete(bind.Table, op.Key); err != nil {
				return fail(err)
			}
			return "true", true
		},

		"store.query": func(ctx context.Context, op session.Op) (string, bool) {
			if am == nil {
				return fail(fmt.Errorf("agent has no memory profile"))
			}
			// Resolve logical table from query or fall back to the only store.
			table := op.Query.Table
			if table == "" && len(am.Stores) == 1 {
				for k := range am.Stores {
					table = k
				}
			}
			h, bind, err := resolve(table)
			if err != nil {
				return fail(err)
			}
			q := op.Query
			q.Table = bind.Table
			it, err := h.Query(bind.Table, q)
			if err != nil {
				return fail(err)
			}
			var recs []memory.Record
			for it.Next() {
				recs = append(recs, it.Record())
			}
			if err := it.Err(); err != nil {
				return fail(err)
			}
			return okJSON(map[string]any{"records": recs})
		},
	}
}
