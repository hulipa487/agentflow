package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/core/accounting"
	"agentflow/internal/core/identity"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/runtime"
	"agentflow/internal/drivers/llm"
)

// settingsConsole builds a console over a real profile store with a models
// manager wired, which is what the settings route validates against.
func settingsConsole(t *testing.T) (*httptest.Server, *identity.Registry, string) {
	t.Helper()
	dir := t.TempDir()
	reg, err := identity.Open(filepath.Join(dir, "identity.db"), quiet())
	if err != nil {
		t.Fatalf("open identity: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	store, err := runtime.OpenSQLite(filepath.Join(dir, "runtime.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	p, err := reg.CreateProfile("Oscar", "oscar@example.com")
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}

	ui := New(Deps{
		Models: llm.NewManager(map[string]config.Model{
			"default": {Provider: "openai", Model: "gpt-4o-mini"},
			"small":   {Provider: "openai", Model: "gpt-4o-mini"},
		}, quiet()),
		Users: UserDeps{
			Identities: reg,
			Store:      store,
			Events:     store,
			Journal:    store,
			Quota:      accounting.New(store, reg.LimitFor, 0),
		},
	})
	srv := httptest.NewServer(ui.API())
	t.Cleanup(srv.Close)
	return srv, reg, p.UserID
}

func putSettings(t *testing.T, srv *httptest.Server, id string, body any) (int, map[string]any) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPut, srv.URL+"/admin/api/users/"+id+"/settings", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return resp.StatusCode, v
}

// TestUserSettings: a profile carries a model and an instruction layer, and this
// is the only way to set them — which model a person runs on is a cost decision,
// not a self-service one. The route had no test at all.
func TestUserSettings(t *testing.T) {
	srv, reg, id := settingsConsole(t)

	// An unknown model is refused rather than stored: a name that does not
	// resolve would fail every future turn at the provider call, long after this
	// request returned 200.
	status, body := putSettings(t, srv, id, map[string]any{"model": "typo"})
	if status != http.StatusBadRequest {
		t.Fatalf("unknown model: got %d (%v); want 400", status, body)
	}

	// A registered one is accepted, and lands on the profile.
	if status, body = putSettings(t, srv, id, map[string]any{"model": "small"}); status != http.StatusOK {
		t.Fatalf("valid model: got %d (%v)", status, body)
	}
	p, _, err := reg.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if p.Model != "small" {
		t.Fatalf("profile model = %q; want %q", p.Model, "small")
	}

	// The instruction layer is stored alongside, and the two are independent.
	if status, body = putSettings(t, srv, id, map[string]any{"instructions_append": "Be terse."}); status != http.StatusOK {
		t.Fatalf("instructions: got %d (%v)", status, body)
	}
	p, _, _ = reg.Get(id)
	if p.InstructionsAppend != "Be terse." {
		t.Fatalf("instructions_append = %q", p.InstructionsAppend)
	}
	if p.Model != "small" {
		t.Fatalf("setting instructions cleared the model: %q", p.Model)
	}

	// An empty string clears a field, which is why emptiness has to be
	// distinguishable from absence.
	if status, _ = putSettings(t, srv, id, map[string]any{"model": ""}); status != http.StatusOK {
		t.Fatalf("clearing the model: got %d", status)
	}
	p, _, _ = reg.Get(id)
	if p.Model != "" {
		t.Fatalf("model = %q; an empty string should clear it", p.Model)
	}

	// Neither field set is a 400 rather than a no-op that reports success.
	if status, body = putSettings(t, srv, id, map[string]any{}); status != http.StatusBadRequest {
		t.Fatalf("nothing to set: got %d (%v); want 400", status, body)
	}
}

// TestUserSettingsWithoutModels: the route validates a model name against the
// live registry, so it needs one; a console wired without a manager must say so
// rather than accept a name it cannot check.
func TestUserSettingsWithoutModels(t *testing.T) {
	dir := t.TempDir()
	reg, err := identity.Open(filepath.Join(dir, "identity.db"), quiet())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	store, err := runtime.OpenSQLite(filepath.Join(dir, "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	p, _ := reg.CreateProfile("Oscar", "")

	ui := New(Deps{Users: UserDeps{
		Identities: reg, Store: store, Events: store, Journal: store,
		Quota: accounting.New(store, reg.LimitFor, 0),
	}})
	srv := httptest.NewServer(ui.API())
	defer srv.Close()

	status, body := putSettings(t, srv, p.UserID, map[string]any{"model": "default"})
	if status != http.StatusServiceUnavailable {
		t.Fatalf("no models manager: got %d (%v); want 503", status, body)
	}
}

// TestUserRoutesRequireTheAdminToken: every other test in this package mounts
// ui.API() bare, so nothing exercised the wrapper these routes are published
// behind. A route registered without it would be open to anything that could
// reach the admin port — and these are the routes that set a person's quota, the
// model they run on, and whether their account can still be reached at all.
func TestUserRoutesRequireTheAdminToken(t *testing.T) {
	_, _, id := settingsConsole(t)

	admin := metrics.NewAdminServer("127.0.0.1:0", "tok123", metrics.NewRegistry(), nil)
	admin.Mount("/admin/api/", New(Deps{}).API(), true)
	guarded := httptest.NewServer(admin.Handler())
	defer guarded.Close()

	routes := []struct{ method, path string }{
		{http.MethodGet, "/admin/api/users"},
		{http.MethodGet, "/admin/api/users/" + id},
		{http.MethodPost, "/admin/api/users/" + id + "/limit"},
		{http.MethodPut, "/admin/api/users/" + id + "/settings"},
		{http.MethodPost, "/admin/api/users/" + id + "/unlink"},
		{http.MethodPost, "/admin/api/users/invites"},
	}
	for _, r := range routes {
		req, err := http.NewRequest(r.method, guarded.URL+r.path, bytes.NewReader([]byte(`{}`)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without a token: got %d; want 401", r.method, r.path, resp.StatusCode)
		}
	}

	// And the same routes are reachable with it, so the refusals above are the
	// wrapper rather than a registration mistake.
	req, _ := http.NewRequest(http.MethodGet, guarded.URL+"/admin/api/users", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatal("the token was rejected; the mount is misconfigured")
	}
}
