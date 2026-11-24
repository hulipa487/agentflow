package tools

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/core/credentials"
	"agentflow/internal/core/execguard"
	"agentflow/internal/core/hosthttp"
	"agentflow/internal/core/netguard"
	"agentflow/internal/core/session"
)

func TestDeclarativeHTTPToolTemplatesAuthAndSelection(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("q")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"item":{"name":"Ada"}}`))
	}))
	defer srv.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := credentials.Open(filepath.Join(t.TempDir(), "cred.db"), "test-master", log)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Put(context.Background(), "u_test", "crm", "api", "sekret", "Authorization", "Bearer"); err != nil {
		t.Fatal(err)
	}

	def := config.HTTPTool{
		Name:        "crm_lookup",
		Description: "lookup",
		Method:      "GET",
		URL:         srv.URL + "/v1/items/{id}",
		Query:       map[string]string{"q": "{q}"},
		Auth:        &config.HTTPAuth{Service: "crm"},
		Params:      map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}},
		Response:    "item.name",
		Hosts:       []string{"127.0.0.1"},
	}
	client := hosthttp.New(log, store, netguard.Policy{AllowPrivate: true})
	spec, err := buildHTTPTool(def, client, log)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "http:crm_lookup" {
		t.Fatalf("name = %q", spec.Name)
	}
	if spec.Kind != execguard.ActionRead {
		t.Fatalf("kind = %q, want read", spec.Kind)
	}
	ctx := session.WithPersonalIdentity(context.Background(), "u_test")
	got, err := spec.Invoke(ctx, map[string]any{"id": "42", "q": "hello world"})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/items/42" || gotQuery != "hello+world" {
		t.Fatalf("request = path %q query %q", gotPath, gotQuery)
	}
	if gotAuth != "Bearer sekret" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if got != "Ada" {
		t.Fatalf("selected response = %#v", got)
	}
}

func TestDeclarativeHTTPToolHostAllowList(t *testing.T) {
	def := config.HTTPTool{
		Name: "blocked", Method: "GET", URL: "https://api.example.com/x",
		Hosts: []string{"other.example.com"},
	}
	spec, err := buildHTTPTool(def, hosthttp.New(slog.Default(), nil, netguard.Policy{}), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spec.Invoke(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "allow-list") {
		t.Fatalf("expected host allow-list refusal, got %v", err)
	}
}

func TestHTTPToolRenderPreservesTypes(t *testing.T) {
	got, err := renderMap(map[string]any{
		"id":   "{id}",
		"name": "user {name}",
		"n":    3,
	}, map[string]any{"id": map[string]any{"nested": true}, "name": "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if string(b) != `{"id":{"nested":true},"n":3,"name":"user Ada"}` {
		t.Fatalf("render = %s", b)
	}
}

func TestDeclarativeHTTPToolRedirectStrippingAndIdempotency(t *testing.T) {
	var leakedAuth, leakedCustom, gotIdem string
	offHostSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leakedAuth = r.Header.Get("Authorization")
		leakedCustom = r.Header.Get("X-Api-Key")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer offHostSrv.Close()

	// Cross-host redirect from 127.0.0.1 -> localhost must strip secrets and obey Hosts allowlist.
	offLocalhost := strings.Replace(offHostSrv.URL, "127.0.0.1", "localhost", 1)
	startSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIdem = r.Header.Get("Idempotency-Key")
		http.Redirect(w, r, offLocalhost+"/hop", http.StatusFound)
	}))
	defer startSrv.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := credentials.Open(filepath.Join(t.TempDir(), "cred.db"), "test-master", log)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Put(context.Background(), "u_test", "billing", "api", "tok123", "Authorization", "Bearer"); err != nil {
		t.Fatal(err)
	}

	// 1. Disallowed redirect host blocked by hosthttp even though initial host is allowed.
	def := config.HTTPTool{
		Name:   "charge",
		Method: "POST",
		URL:    startSrv.URL + "/v1/charges",
		Auth:   &config.HTTPAuth{Service: "billing"},
		Hosts:  []string{"127.0.0.1"},
	}
	spec, err := buildHTTPTool(def, hosthttp.New(log, store, netguard.Policy{AllowPrivate: true}), log)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Kind != execguard.ActionWrite {
		t.Fatalf("POST tool kind = %q, want write", spec.Kind)
	}
	if dest := spec.DestinationFn(context.Background(), nil); !strings.HasPrefix(dest, "http://127.0.0.1:") {
		t.Fatalf("destination = %q", dest)
	}
	ctx := execguard.WithIdempotencyKey(session.WithPersonalIdentity(context.Background(), "u_test"), "idem-tool-1")
	if _, err := spec.Invoke(ctx, map[string]any{"amount": 5}); err == nil || !strings.Contains(err.Error(), "redirect host") {
		t.Fatalf("expected redirect host refusal, got %v", err)
	}
	if gotIdem != "idem-tool-1" {
		t.Fatalf("Idempotency-Key = %q, want idem-tool-1", gotIdem)
	}

	// 2. When redirect target IS allowed (no Hosts allowlist), cross-host secret headers are stripped.
	defNoList := config.HTTPTool{
		Name:    "open_post",
		Method:  "POST",
		URL:     startSrv.URL + "/v1/open",
		Headers: map[string]string{"X-Api-Key": "custom-secret"},
		Auth:    &config.HTTPAuth{Service: "billing"},
	}
	spec2, err := buildHTTPTool(defNoList, hosthttp.New(log, store, netguard.Policy{AllowPrivate: true}), log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spec2.Invoke(ctx, map[string]any{"x": 1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if leakedAuth != "" || leakedCustom != "" {
		t.Fatalf("secrets leaked across redirect: auth=%q custom=%q", leakedAuth, leakedCustom)
	}
}
