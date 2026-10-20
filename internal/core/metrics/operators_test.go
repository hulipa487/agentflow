package metrics

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOperatorTokenIsAttributed(t *testing.T) {
	s := NewAdminServer("", "break-glass", Global(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.SetOperators(map[string]string{"alice": "alice-token"})

	var got string
	h := s.auth(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Operator")
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/admin/api/state", nil)
	req.Header.Set("Authorization", "Bearer alice-token")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK || got != "alice" {
		t.Fatalf("status=%d operator=%q", rec.Code, got)
	}

	bad := httptest.NewRequest(http.MethodGet, "/admin/api/state", nil)
	bad.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h(rec, bad)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token status=%d", rec.Code)
	}
}
