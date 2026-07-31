package metrics

import (
	"net/http"
	"strings"
	"testing"
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
