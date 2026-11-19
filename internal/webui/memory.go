package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"agentflow/internal/core/studio"
)

// memoryResponse flattens the memory surface and adds the outcome of a write.
// Agents open their stores at boot, so a saved edit is durable but not live
// until the next agentflow start.
type memoryResponse struct {
	studio.MemoryList
	OK              bool   `json:"ok"`
	RestartRequired bool   `json:"restart_required"`
	SavedNote       string `json:"saved_note,omitempty"`
}

func (u *UI) memoryReady(w http.ResponseWriter) (*studio.Memory, bool) {
	if u.deps.Memory == nil {
		writeErr(w, http.StatusServiceUnavailable, "the studio memory API is not enabled on this instance")
		return nil, false
	}
	return u.deps.Memory, true
}

func memoryResult(list studio.MemoryList, note string) memoryResponse {
	restart := false
	for _, p := range list.Profiles {
		if !p.Live {
			restart = true
		}
	}
	for _, b := range list.Backends {
		if !b.Live && !b.Implicit {
			restart = true
		}
	}
	return memoryResponse{MemoryList: list, OK: true, RestartRequired: restart, SavedNote: note}
}

func (u *UI) writeMemoryErr(w http.ResponseWriter, err error) {
	if errors.Is(err, studio.ErrMemoryConflict) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeErr(w, http.StatusBadRequest, err.Error())
}

// handleStudioMemory lists the persisted memory backends and profiles.
func (u *UI) handleStudioMemory(w http.ResponseWriter, r *http.Request) {
	m, ok := u.memoryReady(w)
	if !ok {
		return
	}
	list, err := m.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, memoryResult(list, ""))
}

// memoryValidateRequest names one draft. Kind is "profile" or "backend".
type memoryValidateRequest struct {
	Kind     string                     `json:"kind"`
	Creating bool                       `json:"creating"`
	Profile  *studio.MemoryProfileDraft `json:"profile,omitempty"`
	Backend  *studio.MemoryBackendDraft `json:"backend,omitempty"`
}

// handleStudioMemoryValidate dry-runs one profile or backend and returns
// every error, so the form can show them together.
func (u *UI) handleStudioMemoryValidate(w http.ResponseWriter, r *http.Request) {
	m, ok := u.memoryReady(w)
	if !ok {
		return
	}
	var req memoryValidateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var errs []string
	switch req.Kind {
	case "profile":
		if req.Profile == nil {
			writeErr(w, http.StatusBadRequest, "profile draft is required")
			return
		}
		errs = m.ValidateProfile(*req.Profile, req.Creating)
	case "backend":
		if req.Backend == nil {
			writeErr(w, http.StatusBadRequest, "backend draft is required")
			return
		}
		errs = m.ValidateBackend(*req.Backend, req.Creating)
	default:
		writeErr(w, http.StatusBadRequest, "kind must be profile or backend")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": len(errs) == 0, "errors": errs})
}

type memoryProfileRequest struct {
	Profile studio.MemoryProfileDraft `json:"profile"`
	MTime   int64                     `json:"mtime"`
}

func (u *UI) handleStudioMemoryProfileCreate(w http.ResponseWriter, r *http.Request) {
	u.saveMemoryProfile(w, r, true, "")
}

// handleStudioMemoryProfilePut replaces one profile. The name is immutable,
// since agents refer to the profile by it.
func (u *UI) handleStudioMemoryProfilePut(w http.ResponseWriter, r *http.Request) {
	u.saveMemoryProfile(w, r, false, r.PathValue("name"))
}

func (u *UI) saveMemoryProfile(w http.ResponseWriter, r *http.Request, creating bool, name string) {
	m, ok := u.memoryReady(w)
	if !ok {
		return
	}
	var req memoryProfileRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if name != "" {
		if req.Profile.Name != "" && req.Profile.Name != name {
			writeErr(w, http.StatusBadRequest, "a memory profile name cannot be changed")
			return
		}
		req.Profile.Name = name
	}
	list, err := m.SaveProfile(req.Profile, creating, req.MTime)
	if err != nil {
		u.writeMemoryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, memoryResult(list,
		fmt.Sprintf("memory profile %q saved; restart agentflow to apply it", req.Profile.Name)))
}

// handleStudioMemoryProfileDelete removes a profile; refused while an agent or
// spawn template names it.
func (u *UI) handleStudioMemoryProfileDelete(w http.ResponseWriter, r *http.Request) {
	m, ok := u.memoryReady(w)
	if !ok {
		return
	}
	mtime, _ := strconv.ParseInt(r.URL.Query().Get("mtime"), 10, 64)
	list, err := m.DeleteProfile(r.PathValue("name"), mtime)
	if err != nil {
		u.writeMemoryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, memoryResult(list, "memory profile deleted; restart agentflow to drop it"))
}

type memoryBackendRequest struct {
	Backend studio.MemoryBackendDraft `json:"backend"`
	MTime   int64                     `json:"mtime"`
}

func (u *UI) handleStudioMemoryBackendCreate(w http.ResponseWriter, r *http.Request) {
	u.saveMemoryBackend(w, r, true, "")
}

// handleStudioMemoryBackendPut replaces one backend. The name is immutable,
// since stores refer to the backend by it.
func (u *UI) handleStudioMemoryBackendPut(w http.ResponseWriter, r *http.Request) {
	u.saveMemoryBackend(w, r, false, r.PathValue("name"))
}

func (u *UI) saveMemoryBackend(w http.ResponseWriter, r *http.Request, creating bool, name string) {
	m, ok := u.memoryReady(w)
	if !ok {
		return
	}
	var req memoryBackendRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if name != "" {
		if req.Backend.Name != "" && req.Backend.Name != name {
			writeErr(w, http.StatusBadRequest, "a memory backend name cannot be changed")
			return
		}
		req.Backend.Name = name
	}
	list, err := m.SaveBackend(req.Backend, creating, req.MTime)
	if err != nil {
		u.writeMemoryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, memoryResult(list,
		fmt.Sprintf("memory backend %q saved; restart agentflow to open it", req.Backend.Name)))
}

// handleStudioMemoryBackendDelete removes a backend; refused while a profile
// or an agent's stores are on it.
func (u *UI) handleStudioMemoryBackendDelete(w http.ResponseWriter, r *http.Request) {
	m, ok := u.memoryReady(w)
	if !ok {
		return
	}
	mtime, _ := strconv.ParseInt(r.URL.Query().Get("mtime"), 10, 64)
	list, err := m.DeleteBackend(r.PathValue("name"), mtime)
	if err != nil {
		u.writeMemoryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, memoryResult(list, "memory backend deleted; restart agentflow to close it"))
}
