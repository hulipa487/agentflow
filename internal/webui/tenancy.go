// The tenancy admin API: groups, memberships, pools and grants (C10). These
// are the endpoints the control plane (F22) drives — AgentFlow exposes the
// management surface and enforces the invariants (exactly one owner, mastership
// rules, immutable retention); the decisions of who joins what stay outside.
package webui

import (
	"encoding/json"
	"io"
	"net/http"

	"agentflow/internal/core/tenancy"
)

func (u *UI) tenancyOrUnavailable(w http.ResponseWriter) *tenancy.Registry {
	if u.deps.Tenancy == nil {
		writeErr(w, http.StatusServiceUnavailable, "tenancy is not wired on this instance")
		return nil
	}
	return u.deps.Tenancy
}

func decodeBody(w http.ResponseWriter, r *http.Request, into any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read body")
		return false
	}
	if err := json.Unmarshal(body, into); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON body: "+err.Error())
		return false
	}
	return true
}

// --- groups ------------------------------------------------------------------

func (u *UI) handleGroupsList(w http.ResponseWriter, r *http.Request) {
	reg := u.tenancyOrUnavailable(w)
	if reg == nil {
		return
	}
	groups, err := reg.ListGroups(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if groups == nil {
		groups = []tenancy.Group{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "groups": groups})
}

func (u *UI) handleGroupCreate(w http.ResponseWriter, r *http.Request) {
	reg := u.tenancyOrUnavailable(w)
	if reg == nil {
		return
	}
	var body struct {
		Name    string `json:"name"`
		Creator string `json:"creator"` // the creating tenant's personal uuid; becomes master
	}
	if !decodeBody(w, r, &body) {
		return
	}
	g, err := reg.CreateGroup(r.Context(), body.Name, body.Creator)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "group": g})
}

func (u *UI) handleGroupGet(w http.ResponseWriter, r *http.Request) {
	reg := u.tenancyOrUnavailable(w)
	if reg == nil {
		return
	}
	g, ok, err := reg.Group(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "no such group")
		return
	}
	members, err := reg.Members(r.Context(), g.UUID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if members == nil {
		members = []tenancy.Membership{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "group": g, "members": members})
}

func (u *UI) handleGroupAddMember(w http.ResponseWriter, r *http.Request) {
	reg := u.tenancyOrUnavailable(w)
	if reg == nil {
		return
	}
	var body struct {
		User string `json:"user"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	m, err := reg.AddMember(r.Context(), r.PathValue("id"), body.User)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "membership": m})
}

func (u *UI) handleGroupRemoveMember(w http.ResponseWriter, r *http.Request) {
	reg := u.tenancyOrUnavailable(w)
	if reg == nil {
		return
	}
	if err := reg.RemoveMember(r.Context(), r.PathValue("id"), r.PathValue("user")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (u *UI) handleGroupSetMaster(w http.ResponseWriter, r *http.Request) {
	reg := u.tenancyOrUnavailable(w)
	if reg == nil {
		return
	}
	var body struct {
		From string `json:"from"` // must be the current master
		User string `json:"user"` // must be an active member
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if err := reg.SetMaster(r.Context(), r.PathValue("id"), body.From, body.User); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (u *UI) handleGroupDisband(w http.ResponseWriter, r *http.Request) {
	reg := u.tenancyOrUnavailable(w)
	if reg == nil {
		return
	}
	var body struct {
		By string `json:"by"` // must be the current master
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if err := reg.Disband(r.Context(), r.PathValue("id"), body.By); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": "access ends now; rows are deleted when the disband retention elapses"})
}

// --- pools -------------------------------------------------------------------

func (u *UI) handlePoolsList(w http.ResponseWriter, r *http.Request) {
	reg := u.tenancyOrUnavailable(w)
	if reg == nil {
		return
	}
	pools, err := reg.ListPools(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if pools == nil {
		pools = []tenancy.Pool{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pools": pools})
}

func (u *UI) handlePoolCreate(w http.ResponseWriter, r *http.Request) {
	reg := u.tenancyOrUnavailable(w)
	if reg == nil {
		return
	}
	var body struct {
		Name         string `json:"name"`
		Creator      string `json:"creator"`
		OwnerTenant  string `json:"owner_tenant"`
		OwnerGroup   string `json:"owner_group"`
		RetentionDays int   `json:"retention_days"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	p, err := reg.CreatePool(r.Context(), body.Name, body.Creator, body.OwnerTenant, body.OwnerGroup, body.RetentionDays)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"pool": p,
		"note": "retention is immutable (C12); grant carves before booting agents that bind this pool",
	})
}

func (u *UI) handlePoolGrants(w http.ResponseWriter, r *http.Request) {
	reg := u.tenancyOrUnavailable(w)
	if reg == nil {
		return
	}
	grants, err := reg.GrantsFor(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if grants == nil {
		grants = []tenancy.GrantRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "grants": grants})
}

func (u *UI) handlePoolSetGrant(w http.ResponseWriter, r *http.Request) {
	reg := u.tenancyOrUnavailable(w)
	if reg == nil {
		return
	}
	var body struct {
		Agent  string `json:"agent"`
		Tenant string `json:"tenant"` // "" = the member wildcard
		Mode   string `json:"mode"`   // "rw" | "r"
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if err := reg.SetGrant(r.Context(), r.PathValue("id"), body.Agent, body.Tenant, body.Mode); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (u *UI) handlePoolRemoveGrant(w http.ResponseWriter, r *http.Request) {
	reg := u.tenancyOrUnavailable(w)
	if reg == nil {
		return
	}
	tenant := r.URL.Query().Get("tenant")
	if err := reg.RemoveGrant(r.Context(), r.PathValue("id"), r.URL.Query().Get("agent"), tenant); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
