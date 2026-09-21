package caps

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agentflow/internal/core/accounting"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/session"
)

// LoopIdentity is what a loop may know about one of a person's handles.
type LoopIdentity struct {
	Channel  string `json:"channel"`
	Username string `json:"username,omitempty"`
	Name     string `json:"name,omitempty"`
}

// LoopProfile is the loop-visible projection of a profile: enough to greet
// someone and route to them, and nothing more. There is deliberately no email
// and no credential here — a loop learns a name and a channel.
//
// The types live in this package rather than in identity because caps is
// imported by the router's tests, and identity depends on router; importing
// identity here would close a cycle. The wiring in main does the projection.
type LoopProfile struct {
	UserID      string         `json:"id"`
	DisplayName string         `json:"display_name,omitempty"`
	Identities  []LoopIdentity `json:"identities"`
}

// currentView is user.current's payload: the projection plus the flag a loop
// uses to tell a registered user from a guest.
type currentView struct {
	Registered bool `json:"registered"`
	LoopProfile
}

// UserHandlers exposes the read-only user surface to loops: who the current
// turn belongs to, what they have spent today, whether they hold a named
// credential, and — for engine-fired maintenance loops only — the profile
// directory.
//
// The boundary is deliberate. Per-user credentials stay reachable only as the
// opaque auth={service=...} reference that Go resolves at request time, which
// is what keeps a prompt injection from becoming a key leak. A turn with no
// user (engine work, or a handle nobody has linked) gets registered=false
// rather than an error, so a loop can tell a guest from a broken call.
type UserHandlers struct {
	// Profile resolves a user id to its projection; Profiles lists them all.
	// Function values rather than the identity registry itself, for the
	// layering reason above.
	Profile  func(userID string) (LoopProfile, bool, error)
	Profiles func() ([]LoopProfile, error)

	Ledger *runtime.Store
	Quota  *accounting.Quota

	// HasCredential reports whether a user holds a credential for a service.
	// A function rather than the credential store, so no code path here could
	// read a secret even by accident.
	HasCredential func(userID, service string) bool
}

// Handlers returns the user.* op handlers.
func (o UserHandlers) Handlers() map[string]session.OpHandler {
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
	errMaintenance := fmt.Errorf("the user directory requires maintenance provenance (system or scheduler)")

	return map[string]session.OpHandler{
		// user.current -> {registered, id, display_name, identities}
		"user.current": func(ctx context.Context, op session.Op) (string, bool) {
			u := session.UserUUIDFromCtx(ctx)
			if u == "" || o.Profile == nil {
				return okJSON(map[string]any{"registered": false})
			}
			p, ok, err := o.Profile(u)
			if err != nil {
				return fail(err)
			}
			if !ok {
				// A linked handle always has a profile; a missing one means the
				// profile went away underneath the session.
				return fail(fmt.Errorf("no profile for %q", u))
			}
			return okJSON(currentView{Registered: true, LoopProfile: p})
		},

		// user.usage -> today's tokens for the caller, with their quota standing
		"user.usage": func(ctx context.Context, op session.Op) (string, bool) {
			u := session.UserUUIDFromCtx(ctx)
			if u == "" {
				return okJSON(map[string]any{"registered": false})
			}
			if o.Ledger == nil {
				return fail(fmt.Errorf("accounting is not enabled"))
			}
			totals, err := o.Ledger.UsageForDay(u, runtime.DayKey(time.Now()))
			if err != nil {
				return fail(err)
			}
			out := map[string]any{
				"registered": true,
				"input":      totals.Input,
				"output":     totals.Output,
				"cached":     totals.Cached,
				"reasoning":  totals.Reasoning,
				"calls":      totals.Calls,
				"failed":     totals.Failed,
				"billable":   totals.Billable(),
			}
			if o.Quota != nil {
				used, limit, inFlight, err := o.Quota.Status(u)
				if err != nil {
					return fail(err)
				}
				out["used"] = used
				out["in_flight"] = inFlight
				out["limit"] = limit
				// A limit of zero means the deployment set none: say so plainly
				// rather than reporting a remaining budget that does not exist.
				out["unlimited"] = limit == 0
				if limit > 0 {
					remaining := limit - used - inFlight
					if remaining < 0 {
						remaining = 0
					}
					out["remaining"] = remaining
				}
			}
			return okJSON(out)
		},

		// user.has_credential -> whether the caller holds one, never its value.
		"user.has_credential": func(ctx context.Context, op session.Op) (string, bool) {
			service := op.Service
			if service == "" {
				return fail(fmt.Errorf("service is required"))
			}
			present := false
			if u := session.UserUUIDFromCtx(ctx); u != "" && o.HasCredential != nil {
				present = o.HasCredential(u, service)
			}
			return okJSON(map[string]any{"service": service, "present": present})
		},

		// user.get(id) -> one profile. Maintenance only: reading another
		// person's account is not something a user's own turn may do.
		"user.get": func(ctx context.Context, op session.Op) (string, bool) {
			if !session.MaintenanceFromCtx(ctx) {
				return fail(errMaintenance)
			}
			if o.Profile == nil {
				return fail(fmt.Errorf("the identity layer is not enabled"))
			}
			if op.UserID == "" {
				return fail(fmt.Errorf("user_id is required"))
			}
			p, ok, err := o.Profile(op.UserID)
			if err != nil {
				return fail(err)
			}
			if !ok {
				return fail(fmt.Errorf("no such profile %q", op.UserID))
			}
			return okJSON(p)
		},

		// user.list() -> every profile. Maintenance only, for the agent's own
		// per-user loops (a distiller building per-user models).
		"user.list": func(ctx context.Context, op session.Op) (string, bool) {
			if !session.MaintenanceFromCtx(ctx) {
				return fail(errMaintenance)
			}
			if o.Profiles == nil {
				return fail(fmt.Errorf("the identity layer is not enabled"))
			}
			profiles, err := o.Profiles()
			if err != nil {
				return fail(err)
			}
			return okJSON(profiles)
		},
	}
}
