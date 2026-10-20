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
	spec, err := buildHTTPTool(def, &http.Client{Transport: netguard.Policy{AllowPrivate: true}.Transport()}, store, log)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "http:crm_lookup" {
		t.Fatalf("name = %q", spec.Name)
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
	spec, err := buildHTTPTool(def, &http.Client{}, nil, slog.Default())
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
