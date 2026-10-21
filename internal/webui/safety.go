package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"agentflow/internal/core/studio"
)

// safetyResponse flattens the safety surface and adds the outcome of a write.
// Profiles bind when a dispatcher is built, so a saved edit is durable but not
// live until the next agentflow start.
type safetyResponse struct {
	studio.SafetyList
	OK              bool   `json:"ok"`
	RestartRequired bool   `json:"restart_required"`
	SavedNote       string `json:"saved_note,omitempty"`
}

func (u *UI) safetyReady(w http.ResponseWriter) (*studio.Safety, bool) {
	if u.deps.Safety == nil {
		writeErr(w, http.StatusServiceUnavailable, "the studio safety API is not enabled on this instance")
		return nil, false
	}
	return u.deps.Safety, true
}

func safetyResult(list studio.SafetyList, note string) safetyResponse {
	restart := false
	for _, p := range list.Profiles {
		if !p.Live {
			restart = true
			break
		}
	}
	if !restart {
		for _, c := range list.Classifiers {
			if !c.Live {
				restart = true
				break
			}
		}
	}
	return safetyResponse{SafetyList: list, OK: true, RestartRequired: restart, SavedNote: note}
}

func (u *UI) writeSafetyErr(w http.ResponseWriter, err error) {
	if errors.Is(err, studio.ErrSafetyConflict) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeErr(w, http.StatusBadRequest, err.Error())
}

// handleStudioSafety lists the persisted safety profiles and classifiers.
func (u *UI) handleStudioSafety(w http.ResponseWriter, r *http.Request) {
	s, ok := u.safetyReady(w)
	if !ok {
		return
	}
	list, err := s.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, safetyResult(list, ""))
}

// safetyValidateRequest names one draft. Kind is "profile" or "classifier".
type safetyValidateRequest struct {
	Kind       string                     `json:"kind"`
	Creating   bool                       `json:"creating"`
	Profile    *studio.SafetyProfileDraft `json:"profile,omitempty"`
	Classifier *studio.ClassifierDraft    `json:"classifier,omitempty"`
}

// handleStudioSafetyValidate dry-runs one profile or classifier. It returns
// every error rather than only the first so the form can show them all.
func (u *UI) handleStudioSafetyValidate(w http.ResponseWriter, r *http.Request) {
	s, ok := u.safetyReady(w)
	if !ok {
		return
	}
	var req safetyValidateRequest
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
		errs = s.ValidateProfile(*req.Profile, req.Creating)
	case "classifier":
		if req.Classifier == nil {
			writeErr(w, http.StatusBadRequest, "classifier draft is required")
			return
		}
		errs = s.ValidateClassifier(*req.Classifier, req.Creating)
	default:
		writeErr(w, http.StatusBadRequest, "kind must be profile or classifier")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     len(errs) == 0,
		"errors": errs,
	})
}

type safetyProfileRequest struct {
	Profile studio.SafetyProfileDraft `json:"profile"`
	MTime   int64                     `json:"mtime"`
}

// handleStudioSafetyProfileCreate appends a named profile.
func (u *UI) handleStudioSafetyProfileCreate(w http.ResponseWriter, r *http.Request) {
	u.saveSafetyProfile(w, r, true, "")
}

// handleStudioSafetyProfilePut replaces one profile. The name is immutable.
func (u *UI) handleStudioSafetyProfilePut(w http.ResponseWriter, r *http.Request) {
	u.saveSafetyProfile(w, r, false, r.PathValue("name"))
}

func (u *UI) saveSafetyProfile(w http.ResponseWriter, r *http.Request, creating bool, name string) {
	s, ok := u.safetyReady(w)
	if !ok {
		return
	}
	var req safetyProfileRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if name != "" {
		if req.Profile.Name != "" && req.Profile.Name != name {
			writeErr(w, http.StatusBadRequest, "a safety profile name cannot be changed")
			return
		}
		req.Profile.Name = name
	}
	list, err := s.SaveProfile(req.Profile, creating, req.MTime)
	if err != nil {
		u.writeSafetyErr(w, err)
		return
	}
	note := "saved; restart agentflow to bind the new filter chain"
	if !creating {
		note = "saved; restart agentflow to apply the edited filter chain"
	}
	writeJSON(w, http.StatusOK, safetyResult(list, note))
}

// handleStudioSafetyProfileDelete removes a profile. The store refuses while
// an agent or spawn profile still names it.
func (u *UI) handleStudioSafetyProfileDelete(w http.ResponseWriter, r *http.Request) {
	s, ok := u.safetyReady(w)
	if !ok {
		return
	}
	mtime, _ := strconv.ParseInt(r.URL.Query().Get("mtime"), 10, 64)
	list, err := s.DeleteProfile(r.PathValue("name"), mtime)
	if err != nil {
		u.writeSafetyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, safetyResult(list, "deleted; restart agentflow to drop the profile"))
}

type safetyClassifierRequest struct {
	Classifier studio.ClassifierDraft `json:"classifier"`
	MTime      int64                  `json:"mtime"`
}

// handleStudioSafetyClassifierCreate appends a model-backed classifier.
func (u *UI) handleStudioSafetyClassifierCreate(w http.ResponseWriter, r *http.Request) {
	u.saveSafetyClassifier(w, r, true, "")
}

// handleStudioSafetyClassifierPut replaces one classifier. The name is
// immutable so profiles that reference classifier:<name> stay valid.
func (u *UI) handleStudioSafetyClassifierPut(w http.ResponseWriter, r *http.Request) {
	u.saveSafetyClassifier(w, r, false, r.PathValue("name"))
}

func (u *UI) saveSafetyClassifier(w http.ResponseWriter, r *http.Request, creating bool, name string) {
	s, ok := u.safetyReady(w)
	if !ok {
		return
	}
	var req safetyClassifierRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if name != "" {
		if req.Classifier.Name != "" && req.Classifier.Name != name {
			writeErr(w, http.StatusBadRequest, "a safety classifier name cannot be changed")
			return
		}
		req.Classifier.Name = name
	}
	list, err := s.SaveClassifier(req.Classifier, creating, req.MTime)
	if err != nil {
		u.writeSafetyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, safetyResult(list,
		fmt.Sprintf("classifier %q saved; restart agentflow to apply it", req.Classifier.Name)))
}

// handleStudioSafetyClassifierDelete removes a classifier. The store refuses
// while a profile still references classifier:<name>.
func (u *UI) handleStudioSafetyClassifierDelete(w http.ResponseWriter, r *http.Request) {
	s, ok := u.safetyReady(w)
	if !ok {
		return
	}
	mtime, _ := strconv.ParseInt(r.URL.Query().Get("mtime"), 10, 64)
	list, err := s.DeleteClassifier(r.PathValue("name"), mtime)
	if err != nil {
		u.writeSafetyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, safetyResult(list, "classifier deleted; restart agentflow to drop it"))
}
