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

// A fleet scrapes N instances, and the only thing that tells their series apart
// is a label. With none set the exposition is exactly what it always was, so a
// single-instance deployment's scrape does not change shape.
func TestRegistryConstantLabels(t *testing.T) {
	r := NewRegistry()
	c := NewCounter("foo_total", "foo help")
	c.Add(42)
	r.Register(c)

	if got := r.Labels(); len(got) != 0 {
		t.Fatalf("a fresh registry has labels: %v", got)
	}
	if out := r.PrometheusFormat(); !strings.Contains(out, "\nfoo_total 42\n") {
		t.Fatalf("an unlabelled registry must render the bare form:\n%s", out)
	}

	if err := r.SetLabels(map[string]string{"instance": "host-1-42"}); err != nil {
		t.Fatal(err)
	}
	out := r.PrometheusFormat()
	if !strings.Contains(out, `foo_total{instance="host-1-42"} 42`) {
		t.Fatalf("labels missing from the series:\n%s", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Fatal("the exposition must end with a newline")
	}

	// Names are sorted, so the line is stable across scrapes however the map
	// is iterated.
	if err := r.SetLabels(map[string]string{"zone": "eu", "instance": "host-1-42"}); err != nil {
		t.Fatal(err)
	}
	if out := r.PrometheusFormat(); !strings.Contains(out, `foo_total{instance="host-1-42",zone="eu"} 42`) {
		t.Fatalf("labels are not sorted:\n%s", out)
	}
	if got := r.Labels(); got["zone"] != "eu" {
		t.Fatalf("Labels() = %v", got)
	}
}

// A value that would otherwise change what the line means is escaped; a name
// Prometheus would reject is refused before anything is applied, because a
// series with a bad name is dropped at scrape time.
func TestRegistryLabelValidationAndEscaping(t *testing.T) {
	r := NewRegistry()
	if err := r.SetLabels(map[string]string{"bad-name": "x"}); err == nil {
		t.Fatal("a label name with a dash must be refused")
	}
	if err := r.SetLabels(map[string]string{"1st": "x"}); err == nil {
		t.Fatal("a label name starting with a digit must be refused")
	}
	if err := r.SetLabels(map[string]string{"": "x"}); err == nil {
		t.Fatal("an empty label name must be refused")
	}
	if got := r.Labels(); len(got) != 0 {
		t.Fatalf("a refused SetLabels must apply nothing: %v", got)
	}

	if err := r.SetLabels(map[string]string{"note": "a\"b\\c\nd"}); err != nil {
		t.Fatal(err)
	}
	c := NewCounter("foo_total", "foo help")
	r.Register(c)
	if out := r.PrometheusFormat(); !strings.Contains(out, `note="a\"b\\c\nd"`) {
		t.Fatalf("label value not escaped:\n%s", out)
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

func (m *mockResponseWriter) Header() http.Header         { return http.Header{} }
func (m *mockResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (m *mockResponseWriter) WriteHeader(s int)           { m.status = s }
