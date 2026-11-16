package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// On the shared listener a minted token is not a secret, so local-only mode
// refuses every non-health route to a remote client, and to a local one that
// arrived through a proxy. Health and readiness stay open for load balancers.
func TestSharedHandlerLocalOnly(t *testing.T) {
	s := NewAdminServer("", "tok", NewRegistry(), nil)
	s.SetReady(true)
	call := func(h http.Handler, path, remote string, hdr map[string]string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = remote
		req.Header.Set("Authorization", "Bearer tok")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	local := s.SharedHandler(true)
	if c := call(local, "/metrics", "203.0.113.9:4000", nil); c != http.StatusForbidden {
		t.Fatalf("remote /metrics in local-only mode = %d, want 403", c)
	}
	if c := call(local, "/metrics", "127.0.0.1:4000", map[string]string{"X-Forwarded-For": "203.0.113.9"}); c != http.StatusForbidden {
		t.Fatalf("proxied /metrics in local-only mode = %d, want 403", c)
	}
	if c := call(local, "/metrics", "[::1]:4000", nil); c != http.StatusOK {
		t.Fatalf("local /metrics = %d, want 200", c)
	}
	for _, p := range []string{"/healthz", "/readyz"} {
		if c := call(local, p, "203.0.113.9:4000", nil); c != http.StatusOK {
			t.Fatalf("remote %s in local-only mode = %d, want 200", p, c)
		}
	}
	// A configured token: the bearer check alone guards remote clients.
	open := s.SharedHandler(false)
	if c := call(open, "/metrics", "203.0.113.9:4000", nil); c != http.StatusOK {
		t.Fatalf("remote /metrics with a configured token = %d, want 200", c)
	}
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "203.0.113.9:4000"
	rec := httptest.NewRecorder()
	open.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("remote /metrics without a token = %d, want 401", rec.Code)
	}
}
