package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/core/credentials"
)

func TestCounter(t *testing.T) {
	c := NewCounter("test_total", "test counter")
	c.Inc()
	c.Add(5)
	if c.Value() != 6 {
		t.Fatalf("expected 6, got %d", c.Value())
	}
}

func TestRegistryPrometheusFormat(t *testing.T) {
	r := NewRegistry()
	c := NewCounter("foo_total", "foo help")
	c.Add(42)
	r.Register(c)
	out := r.PrometheusFormat()
	if !strings.Contains(out, "foo_total 42") {
		t.Fatalf("expected foo_total 42 in output, got:\n%s", out)
	}
	if !strings.Contains(out, "# HELP foo_total foo help") {
		t.Fatalf("missing HELP line")
	}
}

func TestDefaultCounters(t *testing.T) {
	counters := DefaultCounters()
	if len(counters) == 0 {
		t.Fatal("expected non-empty default counters")
	}
	if _, ok := counters["agentflow_llm_tokens"]; !ok {
		t.Fatal("missing llm_tokens counter")
	}
}

func TestAdminServerAuth(t *testing.T) {
	s := NewAdminServer(":0", "secret", NewRegistry(), nil)
	if s.token != "secret" {
		t.Fatal("token not set")
	}
	// Verify the auth wrapper rejects requests without the correct token.
	rr := &mockResponseWriter{}
	req := &http.Request{Header: http.Header{}}
	handler := s.auth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler(rr, req)
	if rr.status != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.status)
	}

	// With the correct token — should pass through.
	rr2 := &mockResponseWriter{}
	req2 := &http.Request{Header: http.Header{}}
	req2.Header.Set("Authorization", "Bearer secret")
	handler(rr2, req2)
	if rr2.status != http.StatusOK {
		t.Fatalf("expected 200 with token, got %d", rr2.status)
	}
}

type mockResponseWriter struct {
	status int
}

func (m *mockResponseWriter) Header() http.Header        { return http.Header{} }
func (m *mockResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (m *mockResponseWriter) WriteHeader(s int)           { m.status = s }

// TestAdminCredentialsProvisionAndRevoke exercises the /admin/credentials
// lifecycle through the real handler mux: POST provisions, GET lists names,
// DELETE revokes, and the stored secret is gone.
func TestAdminCredentialsProvisionAndRevoke(t *testing.T) {
	store, err := credentials.Open(filepath.Join(t.TempDir(), "creds.db"), "admin-test-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	s := NewAdminServer(":0", "tok", NewRegistry(), nil)
	s.SetCredentials(store)
	srv := httptest.NewServer(s.srv.Handler)
	defer srv.Close()

	authed := func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer tok")
	}

	// Provision.
	body := strings.NewReader(`{"user":"u_1","service":"weather","kind":"api_key","secret":"sk-abc"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/credentials", body)
	authed(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("provision status = %d", resp.StatusCode)
	}

	// List — names only, never values.
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/admin/credentials?user=u_1", nil)
	authed(req)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var listed struct {
		Credentials []credentials.ServiceRef `json:"credentials"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Credentials) != 1 || listed.Credentials[0].Service != "weather" {
		t.Fatalf("list = %+v", listed.Credentials)
	}
	if strings.Contains(fmt.Sprintf("%+v", listed.Credentials), "sk-abc") {
		t.Fatal("list leaked a secret value")
	}

	// Revoke.
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/admin/credentials/weather?user=u_1", nil)
	authed(req)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke status = %d", resp.StatusCode)
	}

	// Confirm gone.
	if _, ok, err := store.Get(context.Background(), "u_1", "weather"); err != nil || ok {
		t.Fatalf("expected gone after revoke: ok=%v err=%v", ok, err)
	}
}

// TestAdminCredentialsRequireAuth: the endpoints are behind the bearer token.
func TestAdminCredentialsRequireAuth(t *testing.T) {
	s := NewAdminServer(":0", "tok", NewRegistry(), nil)
	srv := httptest.NewServer(s.srv.Handler)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/admin/credentials?user=u_1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", resp.StatusCode)
	}
}
