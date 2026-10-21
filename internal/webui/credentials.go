package webui

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode"

	"agentflow/internal/core/credentials"
)

// credentialListResponse is the write-only credential surface for Studio: the
// buckets a credential can belong to, and the metadata of the selected one.
// Values never appear; the fingerprint is the last four characters.
type credentialListResponse struct {
	OK          bool                     `json:"ok"`
	Enabled     bool                     `json:"enabled"`
	Bucket      string                   `json:"bucket"`
	Users       []string                 `json:"users"`
	Credentials []credentials.ServiceRef `json:"credentials"`
	Note        string                   `json:"note,omitempty"`
}

func (u *UI) credentialStore(w http.ResponseWriter) (*credentials.Store, bool) {
	if u.deps.Creds == nil {
		writeErr(w, http.StatusServiceUnavailable, "credentials are not enabled on this instance")
		return nil, false
	}
	return u.deps.Creds, true
}

// credentialBuckets lists the selectable buckets: engine-wide ("") first, then
// every user UUID that already holds a credential.
func (u *UI) credentialBuckets(r *http.Request) ([]string, error) {
	out := []string{""}
	users, err := u.deps.Creds.ListUsers(r.Context())
	if err != nil {
		return nil, err
	}
	for _, user := range users {
		if user != "" {
			out = append(out, user)
		}
	}
	return out, nil
}

func (u *UI) credentialListResponse(r *http.Request, bucket string) (credentialListResponse, error) {
	buckets, err := u.credentialBuckets(r)
	if err != nil {
		return credentialListResponse{}, err
	}
	refs, err := u.deps.Creds.List(r.Context(), bucket)
	if err != nil {
		return credentialListResponse{}, err
	}
	return credentialListResponse{
		OK:          true,
		Enabled:     true,
		Bucket:      bucket,
		Users:       buckets,
		Credentials: refs,
	}, nil
}

// handleStudioCredentials lists credential metadata for one bucket. An empty
// user selects the engine-wide bucket that cred:<service> resolves against.
func (u *UI) handleStudioCredentials(w http.ResponseWriter, r *http.Request) {
	if u.deps.Creds == nil {
		writeJSON(w, http.StatusOK, credentialListResponse{
			OK:          true,
			Enabled:     false,
			Bucket:      "",
			Users:       []string{""},
			Credentials: []credentials.ServiceRef{},
			Note:        "The credential store is disabled. Set runtime.credentials.enabled: true and CREDENTIALS_MASTER_KEY to manage secrets here.",
		})
		return
	}
	out, err := u.credentialListResponse(r, r.URL.Query().Get("user"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type credentialPutRequest struct {
	User    string `json:"user"`
	Service string `json:"service"`
	Kind    string `json:"kind"`
	Secret  string `json:"secret"`
	Header  string `json:"header"`
	Scheme  string `json:"scheme"`
}

// validCredentialService keeps a service name usable in credential.get and in
// a config reference. It deliberately allows : / @ . - because names like
// "oauth/token" and "smtp@host" are common.
func validCredentialService(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// handleStudioCredentialPut creates or replaces one credential. An empty
// secret on an existing service keeps the stored value, so metadata and the
// kind can be edited without retyping a key. The value is never returned.
func (u *UI) handleStudioCredentialPut(w http.ResponseWriter, r *http.Request) {
	store, ok := u.credentialStore(w)
	if !ok {
		return
	}
	var req credentialPutRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.User = strings.TrimSpace(req.User)
	req.Service = strings.TrimSpace(req.Service)
	req.Kind = strings.TrimSpace(req.Kind)
	req.Header = strings.TrimSpace(req.Header)
	req.Scheme = strings.TrimSpace(req.Scheme)
	if !validCredentialService(req.Service) {
		writeErr(w, http.StatusBadRequest, "service must be 1-128 characters with no spaces")
		return
	}
	if req.Kind == "" {
		req.Kind = "api_key"
	}
	if req.Secret == "" {
		existing, found, err := store.Get(r.Context(), req.User, req.Service)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !found {
			writeErr(w, http.StatusBadRequest, "a secret is required for a new credential")
			return
		}
		req.Secret = existing.Value
		if req.Header == "" {
			req.Header = existing.Header
		}
		if req.Scheme == "" {
			req.Scheme = existing.Scheme
		}
	}
	if err := store.Put(r.Context(), req.User, req.Service, req.Kind, req.Secret, req.Header, req.Scheme); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := u.credentialListResponse(r, req.User)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleStudioCredentialDelete revokes one credential:
// DELETE /admin/api/credentials?service=<name>&user=<uuid>. Idempotent.
func (u *UI) handleStudioCredentialDelete(w http.ResponseWriter, r *http.Request) {
	store, ok := u.credentialStore(w)
	if !ok {
		return
	}
	service := strings.TrimSpace(r.URL.Query().Get("service"))
	user := r.URL.Query().Get("user")
	if service == "" {
		writeErr(w, http.StatusBadRequest, "missing service")
		return
	}
	if err := store.Delete(r.Context(), user, service); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out, err := u.credentialListResponse(r, user)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}
