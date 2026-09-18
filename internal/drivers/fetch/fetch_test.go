package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentflow/internal/core/netguard"
)

// localClient may reach the httptest servers below, which all bind 127.0.0.1.
// The guard is exercised separately, in netguard's own tests and in
// TestBlockedAddressIsTyped.
func localClient() *Client { return New(netguard.Policy{AllowPrivate: true}) }

// recording captures what the server saw.
type recording struct {
	method  string
	path    string
	query   string
	headers http.Header
	body    []byte
}

func capture(t *testing.T) (*httptest.Server, *recording, chan struct{}) {
	t.Helper()
	rec := &recording{}
	done := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.method = r.Method
		rec.path = r.URL.Path
		rec.query = r.URL.RawQuery
		rec.headers = r.Header.Clone()
		rec.body = body
		_, _ = io.WriteString(w, `{"ok":true}`)
		done <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	return srv, rec, done
}

// TestRequestWiring covers the core curl surface: method, headers, body, query.
func TestRequestWiring(t *testing.T) {
	srv, rec, done := capture(t)

	res, err := localClient().Do(context.Background(), Request{
		URL:     srv.URL + "/api/things",
		Method:  "post", // lower case must be normalized
		Headers: map[string]string{"X-Trace": "abc", "Accept": "application/json"},
		Body:    `{"hello":"world"}`,
		Query:   map[string]string{"page": "2", "q": "x y"},
	})
	if err != nil {
		t.Fatal(err)
	}
	<-done

	if rec.method != "POST" {
		t.Fatalf("method = %q; want POST", rec.method)
	}
	if rec.path != "/api/things" {
		t.Fatalf("path = %q", rec.path)
	}
	if rec.headers.Get("X-Trace") != "abc" || rec.headers.Get("Accept") != "application/json" {
		t.Fatalf("headers = %v", rec.headers)
	}
	if string(rec.body) != `{"hello":"world"}` {
		t.Fatalf("body = %q", rec.body)
	}
	if !strings.Contains(rec.query, "page=2") || !strings.Contains(rec.query, "q=x+y") {
		t.Fatalf("query = %q", rec.query)
	}
	if res.Status != 200 || res.StatusText != "OK" {
		t.Fatalf("status = %d %q", res.Status, res.StatusText)
	}
	if res.FinalURL != srv.URL+"/api/things?page=2&q=x+y" {
		t.Fatalf("final url = %q", res.FinalURL)
	}
	if !res.TLSVerified {
		t.Fatal("a plain http request has no certificate to verify, so this stays true")
	}
}

// TestJSONBodyIsMarshalled: json sets the body and the content type.
func TestJSONBodyIsMarshalled(t *testing.T) {
	srv, rec, done := capture(t)
	if _, err := localClient().Do(context.Background(), Request{
		URL:    srv.URL,
		Method: "POST",
		JSON:   map[string]any{"a": 1},
	}); err != nil {
		t.Fatal(err)
	}
	<-done

	if ct := rec.headers.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.body, &got); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.body)
	}
	if got["a"] != float64(1) {
		t.Fatalf("body = %v", got)
	}
}

// TestExplicitContentTypeIsKept: the caller's header wins over the json default.
func TestExplicitContentTypeIsKept(t *testing.T) {
	srv, rec, done := capture(t)
	if _, err := localClient().Do(context.Background(), Request{
		URL:    srv.URL,
		Method: "POST",
		// Lower-case on purpose: HTTP header names are case-insensitive, so a
		// plain map index would miss this and wrongly inject the default.
		Headers: map[string]string{"content-type": "application/vnd.custom+json"},
		JSON:    map[string]any{"a": 1},
	}); err != nil {
		t.Fatal(err)
	}
	<-done

	if ct := rec.headers.Get("Content-Type"); ct != "application/vnd.custom+json" {
		t.Fatalf("Content-Type = %q; the caller's header must win", ct)
	}
}

// TestRedirectsFollowed counts hops and reports the final URL.
func TestRedirectsFollowed(t *testing.T) {
	var mux http.ServeMux
	mux.HandleFunc("/one", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/two", http.StatusFound)
	})
	mux.HandleFunc("/two", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/three", http.StatusFound)
	})
	mux.HandleFunc("/three", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "end")
	})
	srv := httptest.NewServer(&mux)
	defer srv.Close()

	res, err := localClient().Do(context.Background(), Request{URL: srv.URL + "/one"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Redirects != 2 {
		t.Fatalf("redirects = %d; want 2", res.Redirects)
	}
	if res.Body != "end" {
		t.Fatalf("body = %q", res.Body)
	}
	if !strings.HasSuffix(res.FinalURL, "/three") {
		t.Fatalf("final url = %q", res.FinalURL)
	}
}

// TestNoFollowReturnsTheRedirect: NoFollow hands back the 3xx itself.
func TestNoFollowReturnsTheRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()

	res, err := localClient().Do(context.Background(), Request{URL: srv.URL, NoFollow: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != http.StatusFound {
		t.Fatalf("status = %d; want 302", res.Status)
	}
	if res.Redirects != 0 {
		t.Fatalf("redirects = %d; want 0", res.Redirects)
	}
}

// TestFollowIsTheDefault: the zero Request must follow, not stop. This is why
// the field is the negated NoFollow — with a plain bool the zero value would
// silently mean the opposite of what the docs promise.
func TestFollowIsTheDefault(t *testing.T) {
	var mux http.ServeMux
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/end", http.StatusFound)
	})
	mux.HandleFunc("/end", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "arrived")
	})
	srv := httptest.NewServer(&mux)
	defer srv.Close()

	res, err := localClient().Do(context.Background(), Request{URL: srv.URL + "/start"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Body != "arrived" || res.Redirects != 1 {
		t.Fatalf("the zero Request must follow redirects, got status %d body %q redirects %d",
			res.Status, res.Body, res.Redirects)
	}
}

// TestRedirectCapIsEnforced: a redirect loop stops rather than spinning.
func TestRedirectCapIsEnforced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer srv.Close()

	_, err := localClient().Do(context.Background(), Request{URL: srv.URL, MaxRedirects: 3})
	if err == nil {
		t.Fatal("a redirect loop must stop")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("err = %v", err)
	}
}

// TestRedirectToForeignSchemeIsRefused. Every hop's address is checked by the
// dialer, but the dialer never sees the scheme — so this check is the only
// thing standing between a redirect and a protocol the client should not speak.
func TestRedirectToForeignSchemeIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "ftp://example.com/file")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	_, err := localClient().Do(context.Background(), Request{URL: srv.URL})
	if err == nil {
		t.Fatal("a redirect to a non-http scheme must be refused")
	}
	if !strings.Contains(err.Error(), "refusing redirect") {
		t.Fatalf("err = %v", err)
	}
}

// TestSchemeValidation: only http and https, checked before any dial.
func TestSchemeValidation(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "gopher://example.com", "ftp://example.com", "//example.com"} {
		if _, err := localClient().Do(context.Background(), Request{URL: raw}); err == nil {
			t.Fatalf("%s must be refused", raw)
		}
	}
}

// TestMethodValidation: the method reaches the request line, so it is
// restricted to the RFC 7230 token shape rather than passed through.
func TestMethodValidation(t *testing.T) {
	srv, _, _ := capture(t)
	for _, m := range []string{"GET\r\nX-Evil: 1", "GET POST", "GE T", "G3T"} {
		if _, err := localClient().Do(context.Background(), Request{URL: srv.URL, Method: m}); err == nil {
			t.Fatalf("method %q must be refused", m)
		}
	}
}

// TestBodyTruncation reports the true size alongside the cut body, so a model
// can tell a short response from a clipped one.
func TestBodyTruncation(t *testing.T) {
	const full = 5000
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Declared explicitly: past its internal buffer Go switches to chunked
		// encoding and ContentLength becomes -1, which would make the
		// assertion below depend on the stdlib's buffering.
		w.Header().Set("Content-Length", strconv.Itoa(full))
		_, _ = io.WriteString(w, strings.Repeat("x", full))
	}))
	defer srv.Close()

	res, err := localClient().Do(context.Background(), Request{URL: srv.URL, MaxBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Body) != 100 {
		t.Fatalf("body length = %d; want 100", len(res.Body))
	}
	if !res.Truncated {
		t.Fatal("truncated must be set")
	}
	if res.Size != 101 {
		t.Fatalf("size = %d; want 101 (the cap plus the one byte that proves there was more)", res.Size)
	}
	if res.ContentLength != full {
		t.Fatalf("content_length = %d; want %d", res.ContentLength, full)
	}
}

// TestNoTruncationWhenUnderCap keeps the flag honest.
func TestNoTruncationWhenUnderCap(t *testing.T) {
	srv, _, _ := capture(t)
	res, err := localClient().Do(context.Background(), Request{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if res.Truncated {
		t.Fatal("a small body must not be flagged truncated")
	}
}

// TestTimeoutIsEnforced: a hanging server returns a deadline error, not a hang.
func TestTimeoutIsEnforced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	start := time.Now()
	_, err := localClient().Do(context.Background(), Request{URL: srv.URL, Timeout: 200 * time.Millisecond})
	if err == nil {
		t.Fatal("a hanging request must time out")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout took %v; the per-request bound was not applied", elapsed)
	}
}

// TestBlockedAddressIsTyped: a guard refusal is distinguishable from an
// ordinary transport failure, which is what lets the caller count it as a
// security event rather than a network error. It must survive http.Client's
// wrapping in *url.Error.
func TestBlockedAddressIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request may reach the server")
	}))
	defer srv.Close()

	// The strict policy, unlike localClient.
	_, err := New(netguard.Policy{}).Do(context.Background(), Request{URL: srv.URL})
	if err == nil {
		t.Fatal("a loopback fetch must be refused")
	}
	if !netguard.IsBlocked(err) {
		t.Fatalf("expected a *netguard.BlockedError, got %T: %v", err, err)
	}
	var b *netguard.BlockedError
	if !errors.As(err, &b) || b.Reason == "" {
		t.Fatalf("the refusal must carry a reason: %v", err)
	}
}

// TestInsecureToggle: against a self-signed server the default fails and
// insecure succeeds, and the response says which happened so the caller can
// alert on it.
func TestInsecureToggle(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "tls ok")
	}))
	defer srv.Close()

	c := localClient()
	if _, err := c.Do(context.Background(), Request{URL: srv.URL}); err == nil {
		t.Fatal("a self-signed certificate must fail verification")
	}

	res, err := c.Do(context.Background(), Request{URL: srv.URL, Insecure: true})
	if err != nil {
		t.Fatalf("insecure must accept the self-signed certificate: %v", err)
	}
	if res.Body != "tls ok" {
		t.Fatalf("body = %q", res.Body)
	}
	if res.TLSVerified {
		t.Fatal("tls_verified must be false when verification was skipped")
	}
}

// TestInsecureOnPlainHTTPStaysVerified: there is no certificate to skip, so the
// flag must not report a downgrade that did not happen — the alert keys off
// this and must not cry wolf.
func TestInsecureOnPlainHTTPStaysVerified(t *testing.T) {
	srv, _, _ := capture(t)
	res, err := localClient().Do(context.Background(), Request{URL: srv.URL, Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TLSVerified {
		t.Fatal("an http URL has no certificate to skip")
	}
}

// TestInsecureStillGuardsAddresses: skipping the certificate check is a
// decision about who the peer claims to be, not about where the connection
// goes. Coupling them would turn one toggle into two.
func TestInsecureStillGuardsAddresses(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request may reach the server")
	}))
	defer srv.Close()

	_, err := New(netguard.Policy{}).Do(context.Background(), Request{URL: srv.URL, Insecure: true})
	if err == nil {
		t.Fatal("an insecure request to loopback must still be refused")
	}
	if !netguard.IsBlocked(err) {
		t.Fatalf("expected an address refusal, not a TLS error: %v", err)
	}
}

// TestRedactScrubsCredentials is a direct test of the helper. The end-to-end
// case is already covered by Go itself — net/http strips a URL password before
// building a *url.Error and never puts headers in one — so testing through a
// live request would assert the stdlib's behaviour rather than this package's.
// What matters here is the contract: a credential must not survive into a
// message the model or a log will see.
func TestRedactScrubsCredentials(t *testing.T) {
	secrets := secretValues(map[string]string{
		"Authorization": "Bearer super-secret",
		"X-Api-Key":     "key-123",
		"Accept":        "application/json", // not a secret header
	})
	if len(secrets) != 2 {
		t.Fatalf("secrets = %v; want the two credential-bearing headers", secrets)
	}

	err := redact(errors.New("dial failed for Authorization: Bearer super-secret and X-Api-Key: key-123"), secrets)
	if strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), "key-123") {
		t.Fatalf("credentials leaked: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Fatalf("err = %v", err)
	}

	// Nothing to scrub: the original error must come back unwrapped, so the
	// chain netguard.IsBlocked walks stays intact.
	sentinel := errors.New("plain failure")
	if got := redact(sentinel, secrets); got != sentinel {
		t.Fatalf("an untouched error must be returned as-is, got %v", got)
	}
}

// TestSecretHeaderDetection pins which header names count as credentials.
func TestSecretHeaderDetection(t *testing.T) {
	for _, name := range []string{"Authorization", "authorization", "X-Api-Key", "x-api-key", "Cookie", "X-Auth-Token", "Password"} {
		if len(secretValues(map[string]string{name: "v"})) != 1 {
			t.Fatalf("%s must be treated as a credential header", name)
		}
	}
	for _, name := range []string{"Accept", "Content-Type", "User-Agent", "X-Request-Id"} {
		if len(secretValues(map[string]string{name: "v"})) != 0 {
			t.Fatalf("%s must not be treated as a credential header", name)
		}
	}
}

// TestDefaultsAreBounds: an unset MaxBytes must not mean "unbounded", because a
// tool result is handed to the model whole and nothing downstream trims it.
func TestDefaultsAreBounds(t *testing.T) {
	tr := New(netguard.Policy{}).verified
	if tr == nil {
		t.Fatal("the client must build a transport")
	}
	if defaultMaxBytes <= 0 || defaultMaxBytes > ceilingMaxBytes {
		t.Fatalf("defaultMaxBytes = %d is not a sane bound", defaultMaxBytes)
	}
	if defaultMaxRedirects <= 0 || defaultMaxRedirects > ceilingMaxRedirects {
		t.Fatalf("defaultMaxRedirects = %d is not a sane bound", defaultMaxRedirects)
	}
	if defaultTimeout <= 0 || defaultTimeout > maxTimeout {
		t.Fatalf("defaultTimeout = %v is not a sane bound", defaultTimeout)
	}
}

// TestErrorNamesStatus keeps a 4xx/5xx distinguishable from a transport
// failure — the tool returns the body for any status, so the status itself is
// what the model reads.
func TestErrorNamesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "nope")
	}))
	defer srv.Close()

	res, err := localClient().Do(context.Background(), Request{URL: srv.URL})
	if err != nil {
		t.Fatalf("an HTTP error status is a response, not a failure: %v", err)
	}
	if res.Status != http.StatusTeapot || res.StatusText != "I'm a teapot" {
		t.Fatalf("status = %d %q", res.Status, res.StatusText)
	}
	if res.Body != "nope" {
		t.Fatalf("body = %q", res.Body)
	}
}

// TestResponseHeadersAreFlattened: multi-valued headers read as one line.
func TestResponseHeadersAreFlattened(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("X-Multi", "a")
		w.Header().Add("X-Multi", "b")
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	res, err := localClient().Do(context.Background(), Request{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Headers["X-Multi"]; got != "a, b" {
		t.Fatalf("X-Multi = %q", got)
	}
}

// TestURLWithoutHost is rejected before any dial, so a malformed argument is a
// clear error rather than a transport oddity.
func TestURLWithoutHost(t *testing.T) {
	if _, err := localClient().Do(context.Background(), Request{URL: "http:///path"}); err == nil {
		t.Fatal("a URL with no host must be refused")
	}
	if _, err := localClient().Do(context.Background(), Request{URL: "not a url at all\x7f"}); err == nil {
		t.Fatal("an unparseable URL must be refused")
	}
}
