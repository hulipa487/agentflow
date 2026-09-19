package caps

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/core/credentials"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/netguard"
	"agentflow/internal/core/pool"
	"agentflow/internal/core/session"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recordBodyServer returns an httptest server that records every request body
// and answers with a minimal text/event-stream "data: [DONE]" reply — the
// common stand-in for a streaming chat-completions endpoint in this package's
// tests.
func recordBodyServer(t *testing.T, bodies *[][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, b)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// permissive is the policy every test in this file needs, because httptest
// servers bind 127.0.0.1 and the real default refuses loopback. The guard
// itself is exercised against the strict zero Policy in
// TestHTTPRefusesPrivateAddresses; everything else here is testing something
// else and must not trip over it.
var permissive = netguard.Policy{AllowPrivate: true}

// runHTTP spins up the handler map and invokes http.request.
//
// The policy is permissive because every httptest server in this file binds
// 127.0.0.1, which the real default refuses. The guard itself is tested
// through runHTTPPolicy, which takes the policy explicitly.
func runHTTP(t *testing.T, op session.Op) (map[string]any, bool) {
	t.Helper()
	return runHTTPPolicy(t, discardLogger(), permissive, op)
}

// runHTTPPolicy runs one op under a given policy and logger, so the address
// guard can be exercised against the same fixture every other test uses.
func runHTTPPolicy(t *testing.T, log *slog.Logger, policy netguard.Policy, op session.Op) (map[string]any, bool) {
	t.Helper()
	h := HTTPHandlers(log, nil, policy)
	ctx := session.WithOwner(context.Background(), "session-1")
	resp, ok := h["http.request"](ctx, op)
	if !ok {
		return map[string]any{"_raw": resp}, false
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatalf("decode response: %v\nraw: %s", err, resp)
	}
	return out, true
}

func TestHTTPGet(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		w.Header().Set("X-Echo", "yes")
		w.WriteHeader(200)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	res, ok := runHTTP(t, session.Op{Type: "http.request", Method: "GET", URL: srv.URL + "/foo"})
	if !ok {
		t.Fatalf("request failed: %v", res)
	}
	if gotMethod != "GET" || gotPath != "/foo" {
		t.Fatalf("server saw %s %s", gotMethod, gotPath)
	}
	if gotQuery != "" {
		t.Fatalf("unexpected query %q", gotQuery)
	}
	if res["status"].(float64) != 200 {
		t.Fatalf("status = %v", res["status"])
	}
	if res["body"] != `{"ok":true}` {
		t.Fatalf("body = %v", res["body"])
	}
	hdrs, _ := res["headers"].(map[string]any)
	if hdrs["X-Echo"] != "yes" {
		t.Fatalf("response header not surfaced: %v", hdrs)
	}
	if res["ok"] != true {
		t.Fatal("expected ok=true")
	}
}

func TestHTTPQueryAndHeaders(t *testing.T) {
	var gotAuth, gotAccept, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotQuery = r.URL.Query().Get("city")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	res, ok := runHTTP(t, session.Op{
		Type: "http.request",
		URL:  srv.URL + "/forecast",
		Headers: map[string]string{
			"Authorization": "Bearer secret-token",
			"Accept":        "application/json",
		},
		QueryParams: map[string]string{"city": "Tokyo"},
	})
	if !ok {
		t.Fatalf("request failed: %v", res)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("auth header not sent: %q", gotAuth)
	}
	if gotAccept != "application/json" {
		t.Fatalf("accept header not sent: %q", gotAccept)
	}
	if gotQuery != "Tokyo" {
		t.Fatalf("query not sent: %q", gotQuery)
	}
}

func TestHTTPPostJSON(t *testing.T) {
	var gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(201)
	}))
	defer srv.Close()

	res, ok := runHTTP(t, session.Op{
		Type:   "http.request",
		Method: "POST",
		URL:    srv.URL + "/things",
		Json:   map[string]any{"name": "widget", "count": 3},
	})
	if !ok {
		t.Fatalf("request failed: %v", res)
	}
	if gotCT != "application/json" {
		t.Fatalf("auto content-type = %q, want application/json", gotCT)
	}
	if !strings.Contains(gotBody, `"name":"widget"`) || !strings.Contains(gotBody, `"count":3`) {
		t.Fatalf("json body not sent: %q", gotBody)
	}
	if res["status"].(float64) != 201 {
		t.Fatalf("status = %v", res["status"])
	}
}

func TestHTTPPostRawBodyDoesNotAutoSetContentType(t *testing.T) {
	var gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	_, ok := runHTTP(t, session.Op{
		Type:   "http.request",
		Method: "POST",
		URL:    srv.URL + "/raw",
		Body:   "hello world",
	})
	if !ok {
		t.Fatal("post failed")
	}
	if gotCT != "" {
		t.Fatalf("raw body should not auto-set content-type, got %q", gotCT)
	}
	if gotBody != "hello world" {
		t.Fatalf("raw body = %q", gotBody)
	}
}

func TestHTTPRejectsNonHTTPScheme(t *testing.T) {
	_, ok := runHTTP(t, session.Op{Type: "http.request", URL: "file:///etc/passwd"})
	if ok {
		t.Fatal("expected file: scheme to be rejected")
	}
	_, ok = runHTTP(t, session.Op{Type: "http.request", URL: "gopher://example.com"})
	if ok {
		t.Fatal("expected gopher: scheme to be rejected")
	}
}

func TestHTTPMissingURL(t *testing.T) {
	_, ok := runHTTP(t, session.Op{Type: "http.request"})
	if ok {
		t.Fatal("expected missing-url error")
	}
}

// TestHTTPRefusesPrivateAddresses is the regression test for the SSRF hole in
// this op: it validated the scheme but never the address, so any agent — net.http
// is in the default capability set and the handler is merged in unconditionally
// — could reach cloud metadata, this process's own admin server, or anything
// else on the host's network.
//
// The guard sits in the dialer, so these cases exercise the real path rather
// than a check that only inspects the URL the loop typed.
func TestHTTPRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request may reach the server")
	}))
	defer srv.Close()

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	targets := map[string]string{
		srv.URL: "loopback",
		"http://169.254.169.254/latest/meta-data/": "cloud metadata",
		"http://10.0.0.1/":                         "RFC1918",
		"http://192.168.1.1/":                      "RFC1918",
		"http://[::1]:8080/":                       "v6 loopback",
		"http://[fc00::1%25en0]/":                  "zoned unique-local",
	}
	for target, what := range targets {
		res, ok := runHTTPPolicy(t, log, netguard.Policy{}, session.Op{
			Type: "http.request", Method: "GET", URL: target,
		})
		if ok {
			t.Fatalf("%s (%s) must be refused, got %v", target, what, res)
		}
		// Distinguish a guard refusal from an ordinary dial failure: without
		// this, an unroutable address would pass the test for the wrong reason.
		raw, _ := res["_raw"].(string)
		if !strings.Contains(raw, "refusing to connect") {
			t.Fatalf("%s (%s): expected a guard refusal, got %s", target, what, raw)
		}
	}

	if out := buf.String(); !strings.Contains(out, "address guard") {
		t.Fatalf("a refusal must be warned about: %s", out)
	}
}

// TestHTTPGuardRefusalReachesTheLoop drives a real Luau loop, so the refusal is
// proven to survive the VM bridge rather than only existing in the Go handler.
// What the model finally sees is what matters, and it sees a raised error — the
// prelude's `op` raises when the handler reports failure.
func TestHTTPGuardRefusalReachesTheLoop(t *testing.T) {
	handlers := map[string]session.OpHandler{}
	for k, h := range HTTPHandlers(discardLogger(), nil, netguard.Policy{}) {
		handlers[k] = h
	}
	gw := &schemaGW{}
	a := session.New("main|test",
		session.Identity{SessionID: "main|test", Agent: "main", Capabilities: map[string]bool{"net.http": true}},
		&session.Info{Name: "main", HistoryBudget: 100},
		gw, nil, nil, nil, nil, handlers, pool.New(1), discardLogger())
	a.LoopSrc = `
function loop()
  local msg = session.inbox()
  local ok, res = pcall(http.request, { url = "http://169.254.169.254/latest/meta-data/" })
  if ok then session.send("resolved: " .. tostring(res.status)) else session.send("refused: " .. tostring(res)) end
end
`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	a.Mailbox <- session.Message{ID: "m1", Type: "user", From: "u", Text: "go", Channel: "webhook", ReplyTo: "1"}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		gw.mu.Lock()
		n := len(gw.sends)
		gw.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	gw.mu.Lock()
	sends := append([]string(nil), gw.sends...)
	gw.mu.Unlock()
	if len(sends) == 0 {
		t.Fatal("the loop produced no reply")
	}
	if !strings.Contains(sends[0], "refused") {
		t.Fatalf("the loop reached cloud metadata: %s", sends[0])
	}
	if !strings.Contains(sends[0], "refusing to connect") {
		t.Fatalf("the refusal must name itself: %s", sends[0])
	}
}

// TestHTTPPrivateRefusalIsCounted: the alert is a metric plus a log line, the
// house pattern for a non-message security event. The counter is registered in
// DefaultCounters — metrics.Inc is a no-op for an unregistered name, so a
// missing registration would look exactly like "it never happened".
func TestHTTPPrivateRefusalIsCounted(t *testing.T) {
	counter, ok := metrics.Global().Get("agentflow_http_private_blocked")
	if !ok {
		t.Fatal("agentflow_http_private_blocked must be registered in DefaultCounters")
	}
	before := counter.Value()

	_, ok = runHTTPPolicy(t, discardLogger(), netguard.Policy{}, session.Op{
		Type: "http.request", Method: "GET", URL: "http://169.254.169.254/",
	})
	if ok {
		t.Fatal("expected a refusal")
	}
	if after := counter.Value(); after != before+1 {
		t.Fatalf("counter went %d -> %d; want +1", before, after)
	}
}

// TestHTTPAllowPrivateReachesLoopback: the documented escape hatch works, so a
// deployment that legitimately calls an internal service is not broken by the
// guard. Same request as the refusal test, opposite policy.
func TestHTTPAllowPrivateReachesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	res, ok := runHTTPPolicy(t, discardLogger(), netguard.Policy{AllowPrivate: true}, session.Op{
		Type: "http.request", Method: "GET", URL: srv.URL,
	})
	if !ok {
		t.Fatalf("allow_private must permit a loopback request: %v", res)
	}
	if res["status"] != float64(200) {
		t.Fatalf("status = %v", res["status"])
	}
}

func TestHTTPTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Slower than the per-call timeout.
		select {
		case <-r.Context().Done():
			return
		case <-make(chan struct{}):
		}
	}))
	defer srv.Close()

	_, ok := runHTTP(t, session.Op{Type: "http.request", URL: srv.URL, Timeout: 0.2})
	if ok {
		t.Fatal("expected timeout to fail the request")
	}
}

func TestHTTPStatusPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		io.WriteString(w, "not found")
	}))
	defer srv.Close()

	res, ok := runHTTP(t, session.Op{Type: "http.request", URL: srv.URL})
	if !ok {
		t.Fatal("a 4xx must still return ok=true with the status")
	}
	if res["status"].(float64) != 404 {
		t.Fatalf("status = %v", res["status"])
	}
	if res["body"] != "not found" {
		t.Fatalf("body = %v", res["body"])
	}
}

func TestHTTPSecretRedactionInError(t *testing.T) {
	// A server that hangs until the per-call timeout fires. The Authorization
	// header value must NOT appear in the error returned to Lua.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	h := HTTPHandlers(discardLogger(), nil, permissive)
	ctx := session.WithOwner(context.Background(), "session-1")
	resp, ok := h["http.request"](ctx, session.Op{
		Type:    "http.request",
		URL:     srv.URL,
		Timeout: 0.2,
		Headers: map[string]string{"Authorization": "super-secret-value-xyz"},
	})
	if ok {
		t.Fatal("expected timeout failure")
	}
	// resp is a JSON-encoded error string.
	var errStr string
	if json.Unmarshal([]byte(resp), &errStr) != nil {
		t.Fatalf("decode err: %s", resp)
	}
	if strings.Contains(errStr, "super-secret-value-xyz") {
		t.Fatalf("secret leaked into error: %s", errStr)
	}
}

func TestHTTPBodyCap(t *testing.T) {
	// maxBodyBytes is 10 MiB; return more and confirm truncation flag.
	big := strings.Repeat("x", 10*1024*1024+1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, big)
	}))
	defer srv.Close()

	res, ok := runHTTP(t, session.Op{Type: "http.request", URL: srv.URL})
	if !ok {
		t.Fatal("request failed")
	}
	if res["truncated"] != true {
		t.Fatal("expected truncated=true")
	}
	if len(res["body"].(string)) > 10*1024*1024 {
		t.Fatalf("body not capped: %d bytes", len(res["body"].(string)))
	}
}

func TestOSEnv(t *testing.T) {
	h := HTTPHandlers(discardLogger(), nil, permissive)
	ctx := context.Background()

	// Set a var, read it.
	os.Setenv("AGENTFLOW_TEST_ENV_VAR", "hello-from-env")
	t.Cleanup(func() { os.Unsetenv("AGENTFLOW_TEST_ENV_VAR") })

	resp, ok := h["os.env"](ctx, session.Op{Type: "os.env", EnvName: "AGENTFLOW_TEST_ENV_VAR"})
	if !ok {
		t.Fatalf("os.env failed: %s", resp)
	}
	var val string
	if err := json.Unmarshal([]byte(resp), &val); err != nil {
		t.Fatal(err)
	}
	if val != "hello-from-env" {
		t.Fatalf("got %q", val)
	}

	// Unset -> "".
	resp, ok = h["os.env"](ctx, session.Op{Type: "os.env", EnvName: "AGENTFLOW_DEFINITELY_UNSET_XYZ"})
	if !ok {
		t.Fatalf("os.env unset failed: %s", resp)
	}
	if err := json.Unmarshal([]byte(resp), &val); err != nil {
		t.Fatal(err)
	}
	if val != "" {
		t.Fatalf("expected empty, got %q", val)
	}
}

func TestOSEnvMissingName(t *testing.T) {
	h := HTTPHandlers(discardLogger(), nil, permissive)
	_, ok := h["os.env"](context.Background(), session.Op{Type: "os.env"})
	if ok {
		t.Fatal("expected error for missing name")
	}
}

// seededStore opens a temp credential store with one service provisioned
// under the given secret kind ("api_key" for HTTP auth headers, "password"
// for mail).
func seededStore(t *testing.T, user, service, kind, secret string) *credentials.Store {
	t.Helper()
	s, err := credentials.Open(filepath.Join(t.TempDir(), "creds.db"), "test-master-key", discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Put(context.Background(), user, service, kind, secret, "", ""); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestHTTPAuthInjectsHeader verifies the loop's auth={service} reference is
// resolved to the stored per-user secret and injected as a header.
func TestHTTPAuthInjectsHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	creds := seededStore(t, "u_oscar", "weather", "api_key", "sk-test-123")
	h := HTTPHandlers(discardLogger(), creds, permissive)
	ctx := session.WithUserUUID(context.Background(), "u_oscar")

	resp, ok := h["http.request"](ctx, session.Op{
		Type: "http.request",
		URL:  srv.URL,
		Auth: &session.CredentialRef{Service: "weather"},
	})
	if !ok {
		t.Fatalf("request failed: %s", resp)
	}
	if gotAuth != "Bearer sk-test-123" {
		t.Fatalf("resolved credential not injected: got header %q", gotAuth)
	}
}

// TestHTTPAuthNoUserFails: without a stamped user UUID, resolution must fail
// cleanly rather than reaching out.
func TestHTTPAuthNoUserFails(t *testing.T) {
	creds := seededStore(t, "u_oscar", "weather", "api_key", "sk-test-123")
	h := HTTPHandlers(discardLogger(), creds, permissive)

	_, ok := h["http.request"](context.Background(), session.Op{
		Type: "http.request",
		URL:  "http://example.invalid/",
		Auth: &session.CredentialRef{Service: "weather"},
	})
	if ok {
		t.Fatal("expected failure when no user is in context")
	}
}

// TestHTTPAuthUnknownServiceFails: a service the user hasn't provisioned must
// not resolve, and must not leak anything.
func TestHTTPAuthUnknownServiceFails(t *testing.T) {
	creds := seededStore(t, "u_oscar", "weather", "api_key", "sk-test-123")
	h := HTTPHandlers(discardLogger(), creds, permissive)
	ctx := session.WithUserUUID(context.Background(), "u_oscar")

	_, ok := h["http.request"](ctx, session.Op{
		Type: "http.request",
		URL:  "http://example.invalid/",
		Auth: &session.CredentialRef{Service: "nonexistent"},
	})
	if ok {
		t.Fatal("expected failure for unknown service")
	}
}

// TestHTTPAuthTenantIsolation: user B cannot resolve user A's credential.
func TestHTTPAuthTenantIsolation(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(200)
	}))
	defer srv.Close()

	creds := seededStore(t, "u_a", "weather", "api_key", "sk-a")
	h := HTTPHandlers(discardLogger(), creds, permissive)
	ctx := session.WithUserUUID(context.Background(), "u_b")

	_, ok := h["http.request"](ctx, session.Op{
		Type: "http.request",
		URL:  srv.URL,
		Auth: &session.CredentialRef{Service: "weather"},
	})
	if ok {
		t.Fatal("expected user b's request to fail")
	}
	if hit {
		t.Fatal("request must not be sent when the credential does not resolve")
	}
}

// TestHTTPAuthRedactedOnError: a resolved credential must be redacted from an
// error string (same redaction as literal headers).
func TestHTTPAuthRedactedOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	creds := seededStore(t, "u_oscar", "weather", "api_key", "super-secret-value-xyz")
	h := HTTPHandlers(discardLogger(), creds, permissive)
	ctx := session.WithUserUUID(context.Background(), "u_oscar")

	resp, ok := h["http.request"](ctx, session.Op{
		Type:    "http.request",
		URL:     srv.URL,
		Timeout: 0.2,
		Auth:    &session.CredentialRef{Service: "weather"},
	})
	if ok {
		t.Fatal("expected timeout failure")
	}
	var errStr string
	if json.Unmarshal([]byte(resp), &errStr) != nil {
		t.Fatalf("decode err: %s", resp)
	}
	if strings.Contains(errStr, "super-secret-value-xyz") {
		t.Fatalf("resolved secret leaked into error: %s", errStr)
	}
}
