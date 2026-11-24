package hosthttp

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/core/credentials"
	"agentflow/internal/core/netguard"
	"agentflow/internal/core/session"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestHostAllowedPatterns(t *testing.T) {
	patterns := []string{"api.github.com", "*.slack.com"}
	cases := []struct {
		host string
		want bool
	}{
		{"api.github.com", true},
		{"API.GITHUB.COM", true},
		{"gist.github.com", false},
		{"hooks.slack.com", true},
		{"a.b.slack.com", true},
		{"slack.com", false},
		{"evilslack.com", false},
	}
	for _, tc := range cases {
		if got := HostAllowed(tc.host, patterns); got != tc.want {
			t.Errorf("HostAllowed(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestClientEnforcesAllowedHostsIncludingRedirects(t *testing.T) {
	disallowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "should-not-reach")
	}))
	defer disallowed.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, disallowed.URL+"/leak", http.StatusFound)
	}))
	defer redirector.Close()

	c := New(testLogger(), nil, netguard.Policy{AllowPrivate: true})

	// Initial host outside allowed_hosts -> refused.
	if _, err := c.Do(context.Background(), Request{
		URL:          redirector.URL + "/start",
		AllowedHosts: []string{"api.example.com"},
	}); err == nil || !strings.Contains(err.Error(), "not in allowed_hosts") {
		t.Fatalf("expected allowed_hosts error, got %v", err)
	}

	// Initial host matches 127.0.0.1, redirect jumps to localhost -> refused at redirect hop.
	disallowedLocalhost := strings.Replace(disallowed.URL, "127.0.0.1", "localhost", 1)
	redirectorToLocalhost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, disallowedLocalhost+"/leak", http.StatusFound)
	}))
	defer redirectorToLocalhost.Close()

	if _, err := c.Do(context.Background(), Request{
		URL:          redirectorToLocalhost.URL + "/start",
		AllowedHosts: []string{"127.0.0.1"},
	}); err == nil || !strings.Contains(err.Error(), "redirect host") {
		t.Fatalf("expected redirect host refusal, got %v", err)
	}
}

func TestClientCredentialAllowlistAndInjection(t *testing.T) {
	store, err := credentials.Open(filepath.Join(t.TempDir(), "creds.db"), "test-master-key", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := session.WithPersonalIdentity(context.Background(), "u1")
	if err := store.Put(ctx, "u1", "github", "api_key", "ghp_secret_123", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, "u1", "openai", "api_key", "sk_secret_999", "", ""); err != nil {
		t.Fatal(err)
	}

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	c := New(testLogger(), store, netguard.Policy{AllowPrivate: true})

	// Disallowed credential service refused when RequireCredentialAllowlist is set.
	if _, err := c.Do(ctx, Request{
		URL:                        srv.URL,
		AuthService:                "openai",
		AllowedCredentials:         []string{"github"},
		RequireCredentialAllowlist: true,
	}); err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("expected credential allowlist refusal, got %v", err)
	}
	if _, err := c.Do(ctx, Request{
		URL:                        srv.URL,
		AuthService:                "github",
		RequireCredentialAllowlist: true,
	}); err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("empty credential allowlist must refuse AuthService when required, got %v", err)
	}

	// Permitted credential injected on host cleanly.
	resp, err := c.Do(ctx, Request{
		URL:                        srv.URL,
		AuthService:                "github",
		AllowedCredentials:         []string{"github"},
		RequireCredentialAllowlist: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != 200 || gotAuth != "Bearer ghp_secret_123" {
		t.Fatalf("status=%d auth=%q", resp.Status, gotAuth)
	}
}

func TestClientRefusesPrivateWithoutAllowPrivate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "nope")
	}))
	defer srv.Close()

	c := New(testLogger(), nil, netguard.Policy{})
	_, err := c.Do(context.Background(), Request{URL: srv.URL})
	if err == nil || !netguard.IsBlocked(err) {
		t.Fatalf("expected netguard block, got %v", err)
	}
}

func TestClientStripsSecretsAcrossHostRedirectsAndForwardsIdempotencyKey(t *testing.T) {
	var leakedAuth, leakedCookie, leakedAPIKey, gotIdem string
	finalSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leakedAuth = r.Header.Get("Authorization")
		leakedCookie = r.Header.Get("Cookie")
		leakedAPIKey = r.Header.Get("X-Api-Key")
		_, _ = io.WriteString(w, "1234567890")
	}))
	defer finalSrv.Close()

	// Cross-host hop using localhost while initial request uses 127.0.0.1.
	finalLocalhost := strings.Replace(finalSrv.URL, "127.0.0.1", "localhost", 1)
	startSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIdem = r.Header.Get("Idempotency-Key")
		http.Redirect(w, r, finalLocalhost+"/next", http.StatusFound)
	}))
	defer startSrv.Close()

	c := New(testLogger(), nil, netguard.Policy{AllowPrivate: true})
	resp, err := c.Do(context.Background(), Request{
		URL: startSrv.URL,
		Headers: map[string]string{
			"Authorization": "Bearer secret",
			"Cookie":        "sid=abc",
			"X-Api-Key":     "topsecret",
		},
		IdempotencyKey: "idem-http-1",
		MaxBodyBytes:   4,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotIdem != "idem-http-1" {
		t.Fatalf("Idempotency-Key = %q, want %q", gotIdem, "idem-http-1")
	}
	if leakedAuth != "" || leakedCookie != "" || leakedAPIKey != "" {
		t.Fatalf("secret headers leaked on cross-host redirect: auth=%q cookie=%q key=%q", leakedAuth, leakedCookie, leakedAPIKey)
	}
	if !resp.Truncated || resp.Body != "1234" {
		t.Fatalf("expected truncated body \"1234\", got truncated=%v body=%q", resp.Truncated, resp.Body)
	}
}
