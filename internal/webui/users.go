package webui

import (
	"encoding/json"
	"net/http"
	"time"

	"agentflow/internal/core/accounting"
	"agentflow/internal/core/files"
	"agentflow/internal/core/identity"
	"agentflow/internal/core/runtime"
)

// UserDeps is the per-user console surface: the profile store, the runtime
// store (usage ledger and message journal), the quota, and the file store. Any
// of them may be nil when the corresponding subsystem is disabled, and each
// handler degrades with a named reason rather than an empty answer.
type UserDeps struct {
	Identities *identity.Registry
	Store      *runtime.Store
	Quota      *accounting.Quota
	Files      *files.Manager
}

func (u *UI) usersAvailable(w http.ResponseWriter) bool {
	if u.deps.Users.Identities == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "the identity layer is not enabled (runtime.identity.enabled)",
		})
		return false
	}
	return true
}

// handleUsers lists every profile with today's usage and quota standing.
func (u *UI) handleUsers(w http.ResponseWriter, r *http.Request) {
	if !u.usersAvailable(w) {
		return
	}
	profiles, err := u.deps.Users.Identities.List()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	day := runtime.DayKey(time.Now())
	out := make([]map[string]any, 0, len(profiles))
	for _, p := range profiles {
		row := map[string]any{
			"user_id":        p.UserID,
			"display_name":   p.DisplayName,
			"email":          p.Email,
			"tokens_per_day": p.TokensPerDay,
			"created_at":     p.CreatedAt,
			"updated_at":     p.UpdatedAt,
			"identities":     len(p.Identities),
		}
		if u.deps.Users.Store != nil {
			if totals, err := u.deps.Users.Store.UsageForDay(p.UserID, day); err == nil {
				row["usage_today"] = totals
			}
		}
		if u.deps.Users.Quota != nil {
			if used, limit, inFlight, err := u.deps.Users.Quota.Status(p.UserID); err == nil {
				row["quota"] = map[string]any{"used": used, "limit": limit, "in_flight": inFlight}
			}
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"day": day, "users": out})
}

// handleUser is the profile detail: who they are, what they spent, what they
// own, and what the audit trail says they did.
func (u *UI) handleUser(w http.ResponseWriter, r *http.Request) {
	if !u.usersAvailable(w) {
		return
	}
	id := r.PathValue("id")
	p, ok, err := u.deps.Users.Identities.Get(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such profile"})
		return
	}

	day := runtime.DayKey(time.Now())
	out := map[string]any{"profile": p, "day": day}

	if u.deps.Users.Store != nil {
		if totals, err := u.deps.Users.Store.UsageForDay(id, day); err == nil {
			out["usage_today"] = totals
		}
		// The per-call detail exists only when usage.events is enabled; an
		// empty list is the honest answer otherwise.
		if events, err := u.deps.Users.Store.UsageEvents(id, time.Now().AddDate(0, 0, -7), 50); err == nil {
			out["recent_calls"] = events
		}
		if audit, err := u.deps.Users.Store.ListMessages(r.Context(), runtime.JournalFilter{
			UserUUID: id, Limit: 50,
		}); err == nil {
			out["audit"] = audit
		}
	}
	if u.deps.Users.Quota != nil {
		if used, limit, inFlight, err := u.deps.Users.Quota.Status(id); err == nil {
			out["quota"] = map[string]any{"used": used, "limit": limit, "in_flight": inFlight}
		}
	}
	if u.deps.Users.Files != nil {
		if projects, err := u.deps.Users.Files.Projects(r.Context(), "user:"+id); err == nil {
			out["projects"] = projects
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleUserLimit sets a profile's own daily token limit (0 = inherit the
// deployment default).
func (u *UI) handleUserLimit(w http.ResponseWriter, r *http.Request) {
	if !u.usersAvailable(w) {
		return
	}
	var req struct {
		TokensPerDay *int64 `json:"tokens_per_day"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if req.TokensPerDay == nil || *req.TokensPerDay < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tokens_per_day must be >= 0 (0 = inherit the default)"})
		return
	}
	id := r.PathValue("id")
	if err := u.deps.Users.Identities.Update(id, nil, nil, req.TokensPerDay); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	p, _, _ := u.deps.Users.Identities.Get(id)
	writeJSON(w, http.StatusOK, map[string]any{"profile": p})
}

// handleUserUnlink detaches a handle from a profile. The store refuses to
// remove the last one: that would leave the account unreachable.
func (u *UI) handleUserUnlink(w http.ResponseWriter, r *http.Request) {
	if !u.usersAvailable(w) {
		return
	}
	var req struct {
		IdentityID string `json:"identity_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if req.IdentityID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "identity_id is required"})
		return
	}
	id := r.PathValue("id")
	if err := u.deps.Users.Identities.Unlink(id, req.IdentityID); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	p, _, _ := u.deps.Users.Identities.Get(id)
	writeJSON(w, http.StatusOK, map[string]any{"profile": p})
}

// handleInviteIssue mints a registration invite, for the invite-only mode where
// the operator hands out codes instead of opening registration.
func (u *UI) handleInviteIssue(w http.ResponseWriter, r *http.Request) {
	if !u.usersAvailable(w) {
		return
	}
	code, err := u.deps.Users.Identities.IssueInvite(7 * 24 * time.Hour)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"invite":     code,
		"expires_in": "168h",
		"hint":       "the user redeems this at POST /v1/users/register with {\"invite\": \"...\"}",
	})
}
