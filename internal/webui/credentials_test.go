package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/core/credentials"
)

func credentialAPIFixture(t *testing.T) (*UI, *credentials.Store) {
	t.Helper()
	store, err := credentials.Open(filepath.Join(t.TempDir(), "creds.db"), "test-master-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return New(Deps{Creds: store}), store
}

func decodeCredentialList(t *testing.T, rec *httptest.ResponseRecorder) credentialListResponse {
	t.Helper()
	var out credentialListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}
	return out
}

func TestStudioCredentialsAPI(t *testing.T) {
	ui, _ := credentialAPIFixture(t)

	rec := do(t, ui.API(), http.MethodGet, "/admin/api/credentials", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	list := decodeCredentialList(t, rec)
	if !list.Enabled || list.Bucket != "" || len(list.Credentials) != 0 {
		t.Fatalf("initial list = %+v", list)
	}
	if len(list.Users) != 1 || list.Users[0] != "" {
		t.Fatalf("initial buckets = %v", list.Users)
	}

	rec = do(t, ui.API(), http.MethodPost, "/admin/api/credentials", map[string]any{
		"service": "github", "kind": "api_key", "secret": "ghp_supersecret",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "ghp_supersecret") {
		t.Fatalf("API leaked the secret: %s", rec.Body.String())
	}
	list = decodeCredentialList(t, rec)
	if len(list.Credentials) != 1 {
		t.Fatalf("credentials = %+v", list.Credentials)
	}
	got := list.Credentials[0]
	if got.Service != "github" || got.Kind != "api_key" || got.Fingerprint != "…cret" {
		t.Fatalf("stored ref = %+v", got)
	}
	if got.Header != "Authorization" || got.Scheme != "Bearer" {
		t.Errorf("defaults not reported: %+v", got)
	}

	// An empty secret keeps the stored value; only the metadata changes.
	rec = do(t, ui.API(), http.MethodPost, "/admin/api/credentials", map[string]any{
		"service": "github", "kind": "token", "header": "X-Api-Key", "scheme": "",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("metadata update: %d %s", rec.Code, rec.Body.String())
	}
	list = decodeCredentialList(t, rec)
	if len(list.Credentials) != 1 || list.Credentials[0].Kind != "token" ||
		list.Credentials[0].Fingerprint != "…cret" {
		t.Fatalf("updated ref = %+v", list.Credentials)
	}

	// A per-user bucket is addressable and appears in the bucket list.
	rec = do(t, ui.API(), http.MethodPost, "/admin/api/credentials", map[string]any{
		"user": "u_1", "service": "stripe", "secret": "sk_live_1234",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("per-user put: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, ui.API(), http.MethodGet, "/admin/api/credentials?user=u_1", nil)
	list = decodeCredentialList(t, rec)
	if list.Bucket != "u_1" || len(list.Credentials) != 1 || list.Credentials[0].Service != "stripe" {
		t.Fatalf("user list = %+v", list)
	}
	if len(list.Users) != 2 || list.Users[0] != "" || list.Users[1] != "u_1" {
		t.Fatalf("buckets = %v", list.Users)
	}

	if rec := do(t, ui.API(), http.MethodPost, "/admin/api/credentials", map[string]any{
		"service": "empty",
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty secret for a new service: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, ui.API(), http.MethodPost, "/admin/api/credentials", map[string]any{
		"service": "bad name", "secret": "x",
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("service with a space: %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, ui.API(), http.MethodDelete, "/admin/api/credentials?service=github", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	list = decodeCredentialList(t, rec)
	if len(list.Credentials) != 0 {
		t.Fatalf("credentials after delete = %+v", list.Credentials)
	}
}

func TestStudioCredentialsDisabled(t *testing.T) {
	ui := New(Deps{})
	rec := do(t, ui.API(), http.MethodGet, "/admin/api/credentials", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET disabled: %d %s", rec.Code, rec.Body.String())
	}
	list := decodeCredentialList(t, rec)
	if list.Enabled || list.Note == "" {
		t.Fatalf("disabled list = %+v", list)
	}
	if rec := do(t, ui.API(), http.MethodPost, "/admin/api/credentials", map[string]any{
		"service": "x", "secret": "y",
	}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("put disabled: %d %s", rec.Code, rec.Body.String())
	}
}
