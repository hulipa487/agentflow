package metrics

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAdminCORS: a preflight from an allow-listed origin is answered before
// the bearer check (browsers send it without the header), an unlisted origin
// is refused, and an actual request still carries the CORS headers.
func TestAdminCORS(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewAdminServer("127.0.0.1:0", "tok", NewRegistry(), log)
	s.SetCORS([]string{"http://127.0.0.1:5173"})
	h := s.Handler()

	preflight := httptest.NewRequest(http.MethodOptions, "/metrics", nil)
	preflight.Header.Set("Origin", "http://127.0.0.1:5173")
	preflight.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, preflight)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("allowed preflight = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://127.0.0.1:5173" {
		t.Fatalf("allow-origin = %q", got)
	}

	bad := httptest.NewRequest(http.MethodOptions, "/metrics", nil)
	bad.Header.Set("Origin", "http://evil.example")
	bad.Header.Set("Access-Control-Request-Method", "GET")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, bad)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unlisted origin preflight = %d, want 403", rec.Code)
	}

	// A real request without the token is still rejected; the CORS header is
	// present so the browser surfaces the 401 to the app rather than a CORS
	// error.
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Origin", "http://127.0.0.1:5173")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://127.0.0.1:5173" {
		t.Fatalf("allow-origin on 401 = %q", got)
	}
}
