package tools

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/netguard"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/fetch"
)

// permitting may reach the httptest servers below, which bind 127.0.0.1. The
// guard is exercised with the strict zero policy in its own tests.
func permitting() *fetch.Client { return fetch.New(netguard.Policy{AllowPrivate: true}) }

// invoking registers and exposes builtin:fetch as main.go does, then runs one
// call as agent "writer".
func invoking(t *testing.T, c *fetch.Client, log *slog.Logger, args map[string]any) (map[string]any, error) {
	t.Helper()
	r := NewRegistry()
	RegisterFetchBuiltins(r, c, log)
	as := r.Expose([]string{"builtin:fetch"}, config.ToolsPolicy{}, false)
	ctx := session.WithOwner(context.Background(), "writer")
	res, err := as.Invoke(ctx, "builtin:fetch", args)
	if err != nil {
		return nil, err
	}
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result type %T", res)
	}
	return m, nil
}

// quiet is a logger that logs nowhere, for tests not asserting on logs.
func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// echoServer answers with a small JSON body and records nothing.
func echoServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo", "yes")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestFetchNotConfigured: the tool needs no account or credential, but a nil
// client is still reported honestly rather than panicking.
func TestFetchNotConfigured(t *testing.T) {
	m, err := invoking(t, nil, quiet(), map[string]any{"url": "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if m["ok"] != false || m["unavailable"] != true {
		t.Fatalf("expected honest-unavailable, got %v", m)
	}
}

// TestFetchRequiresURL: a missing url is a correctable result, not a failure.
func TestFetchRequiresURL(t *testing.T) {
	m, err := invoking(t, permitting(), quiet(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if m["ok"] != false {
		t.Fatalf("expected ok=false, got %v", m)
	}
	if msg, _ := m["error"].(string); !strings.Contains(msg, "url is required") {
		t.Fatalf("error = %v", m["error"])
	}
}

// TestFetchResultMapping: the response shape the model reads.
func TestFetchResultMapping(t *testing.T) {
	srv := echoServer(t)
	m, err := invoking(t, permitting(), quiet(), map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if m["ok"] != true {
		t.Fatalf("ok = %v (%v)", m["ok"], m["error"])
	}
	if m["status"] != 200 || m["status_text"] != "OK" {
		t.Fatalf("status = %v %v", m["status"], m["status_text"])
	}
	if m["body"] != `{"ok":true}` {
		t.Fatalf("body = %v", m["body"])
	}
	if m["tls_verified"] != true {
		t.Fatalf("tls_verified = %v; a plain http request has nothing to skip", m["tls_verified"])
	}
	if _, has := m["truncated"]; has {
		t.Fatal("a short body must not be flagged truncated")
	}
	if _, has := m["redirects"]; has {
		t.Fatal("no redirect was followed, so none should be reported")
	}
	headers, _ := m["headers"].(map[string]string)
	if headers["X-Echo"] != "yes" {
		t.Fatalf("headers = %v", m["headers"])
	}
	if _, has := m["time_cost_ms"]; !has {
		t.Fatal("time_cost_ms must be reported")
	}
}

// TestFetchForwardsArguments: the curl surface reaches the wire. Values arrive
// as []any / map[string]any on the Lua path, which is what these use.
func TestFetchForwardsArguments(t *testing.T) {
	type seen struct {
		method, path, query, body, ct, ua string
	}
	got := make(chan seen, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			body: string(b), ct: r.Header.Get("Content-Type"), ua: r.Header.Get("User-Agent"),
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	_, err := invoking(t, permitting(), quiet(), map[string]any{
		"url":        srv.URL + "/things",
		"method":     "POST",
		"json":       map[string]any{"a": float64(1)},
		"query":      map[string]any{"page": "2"},
		"headers":    map[string]any{"X-Trace": "t1"},
		"user_agent": "agentflow-test",
		"timeout_ms": float64(5000),
		"max_bytes":  float64(1024),
	})
	if err != nil {
		t.Fatal(err)
	}
	s := <-got
	if s.method != "POST" || s.path != "/things" {
		t.Fatalf("saw %s %s", s.method, s.path)
	}
	if s.query != "page=2" {
		t.Fatalf("query = %q", s.query)
	}
	if s.ct != "application/json" || s.ua != "agentflow-test" {
		t.Fatalf("content-type = %q, user-agent = %q", s.ct, s.ua)
	}
	if !strings.Contains(s.body, `"a":1`) {
		t.Fatalf("body = %q", s.body)
	}
}

// TestFetchFollowDefaultAndOverride: follow defaults to true, and follow:false
// stops at the redirect.
func TestFetchFollowDefaultAndOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/end" {
			_, _ = io.WriteString(w, "arrived")
			return
		}
		http.Redirect(w, r, "/end", http.StatusFound)
	}))
	defer srv.Close()

	m, err := invoking(t, permitting(), quiet(), map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if m["body"] != "arrived" || m["redirects"] != 1 {
		t.Fatalf("follow must default on: body %v redirects %v", m["body"], m["redirects"])
	}

	m, err = invoking(t, permitting(), quiet(), map[string]any{"url": srv.URL, "follow": false})
	if err != nil {
		t.Fatal(err)
	}
	if m["status"] != 302 {
		t.Fatalf("status = %v; follow:false must return the redirect itself", m["status"])
	}
}

// TestFetchReportsTruncation: the body is capped and the cut is reported, with
// the true size — a model that cannot tell a short response from a clipped one
// will draw the wrong conclusion from it.
func TestFetchReportsTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("y", 5000))
	}))
	defer srv.Close()

	m, err := invoking(t, permitting(), quiet(), map[string]any{"url": srv.URL, "max_bytes": float64(64)})
	if err != nil {
		t.Fatal(err)
	}
	if m["truncated"] != true {
		t.Fatalf("truncated = %v", m["truncated"])
	}
	if body, _ := m["body"].(string); len(body) != 64 {
		t.Fatalf("body length = %d; want 64", len(body))
	}
	// argCount rather than a bare comparison: the driver reports these as
	// int64, while a literal is int, and the Lua path would deliver float64.
	// The helper is the same one the tool uses to read them back.
	if size := argCount(m["size"]); size != 65 {
		t.Fatalf("size = %v; want the cap plus one", m["size"])
	}
}

// TestFetchBlockedAddressIsAnErrorAndAnAlert is the security-relevant test on
// this path: a refusal must reach the model as an error naming the address, and
// it must be counted and logged rather than passing quietly.
func TestFetchBlockedAddressIsAnErrorAndAnAlert(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request may reach the server")
	}))
	defer srv.Close()

	counter, ok := metrics.Global().Get("agentflow_http_private_blocked")
	if !ok {
		t.Fatal("agentflow_http_private_blocked must be registered in DefaultCounters")
	}
	before := counter.Value()

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	// The strict policy, unlike permitting().
	_, err := invoking(t, fetch.New(netguard.Policy{}), log, map[string]any{"url": srv.URL})
	if err == nil {
		t.Fatal("a loopback fetch must fail")
	}
	if !strings.Contains(err.Error(), "refusing to connect") {
		t.Fatalf("err must name the refusal: %v", err)
	}

	if after := counter.Value(); after != before+1 {
		t.Fatalf("counter went %d -> %d; want +1", before, after)
	}
	out := buf.String()
	if !strings.Contains(out, "address guard") || !strings.Contains(out, "writer") {
		t.Fatalf("the refusal must be logged against the agent: %s", out)
	}
}

// TestFetchInsecureAlerts: the toggle is allowed but never quiet — it is
// counted, logged against the agent, and flagged in the result.
func TestFetchInsecureAlerts(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "tls ok")
	}))
	defer srv.Close()

	counter, ok := metrics.Global().Get("agentflow_http_insecure_tls")
	if !ok {
		t.Fatal("agentflow_http_insecure_tls must be registered in DefaultCounters")
	}
	before := counter.Value()

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	// Without the toggle a self-signed certificate fails, and that is not an
	// alert — nothing was downgraded.
	if _, err := invoking(t, permitting(), log, map[string]any{"url": srv.URL}); err == nil {
		t.Fatal("a self-signed certificate must fail verification")
	}
	if counter.Value() != before {
		t.Fatal("a failed verification is not a skipped verification")
	}

	m, err := invoking(t, permitting(), log, map[string]any{"url": srv.URL, "insecure": true})
	if err != nil {
		t.Fatalf("insecure must succeed: %v", err)
	}
	if m["tls_verified"] != false {
		t.Fatalf("tls_verified = %v; want false", m["tls_verified"])
	}
	if after := counter.Value(); after != before+1 {
		t.Fatalf("counter went %d -> %d; want +1", before, after)
	}
	out := buf.String()
	if !strings.Contains(out, "tls verification skipped") || !strings.Contains(out, "writer") {
		t.Fatalf("the downgrade must be logged against the agent: %s", out)
	}
}

// TestFetchHTTPErrorStatusIsAResult: a 404 is a response the model should read,
// not a tool failure — `ok` describes the request, `status` describes the
// server's answer.
func TestFetchHTTPErrorStatusIsAResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "missing")
	}))
	defer srv.Close()

	m, err := invoking(t, permitting(), quiet(), map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("a 404 is not a transport failure: %v", err)
	}
	if m["ok"] != true || m["status"] != 404 || m["body"] != "missing" {
		t.Fatalf("result = %v", m)
	}
}

// TestFetchSchemaDeclaresURL: one required parameter, and the tool is
// autonomous like web_search — a deployment tightens it through
// tools.policy.overrides.
func TestFetchSchemaDeclaresURL(t *testing.T) {
	r := NewRegistry()
	RegisterFetchBuiltins(r, permitting(), quiet())
	spec, ok := r.tools["builtin:fetch"]
	if !ok {
		t.Fatal("builtin:fetch must be registered")
	}
	required, _ := spec.Parameters["required"].([]string)
	if len(required) != 1 || required[0] != "url" {
		t.Fatalf("required = %v; want exactly [url]", required)
	}
	if !spec.Autonomous {
		t.Fatal("fetching is a read-only action and should be autonomous by default")
	}
	props, _ := spec.Parameters["properties"].(map[string]any)
	for _, key := range []string{"method", "headers", "body", "json", "query", "timeout_ms", "follow", "max_redirects", "user_agent", "max_bytes", "insecure"} {
		if _, has := props[key]; !has {
			t.Fatalf("the curl option %q must be exposed", key)
		}
	}
}
