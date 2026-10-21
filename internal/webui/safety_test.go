package webui

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/core/studio"
)

func safetyAPIFixture(t *testing.T) (*UI, string) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("system.yaml", `
version: "1"
models:
  default: { provider: openai, model: gpt-4o-mini, base_url: http://127.0.0.1:11434/v1 }
profiles:
  safety:
    base:
      filters: [source-attribution, affect-guard]
`)
	write("profiles/base.yaml", "name: base\nloop: ./base.lua\nsafety: base\n")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg, err := config.LoadDir(dir, log)
	if err != nil {
		t.Fatal(err)
	}
	st := studio.NewSafety(cfg, dir, "", log)
	ui := New(Deps{ConfigDir: dir, Cfg: cfg, Safety: st})
	return ui, dir
}

type safetyListResponse struct {
	studio.SafetyList
	RestartRequired bool `json:"restart_required"`
}

func decodeSafety(t *testing.T, body []byte) safetyListResponse {
	t.Helper()
	var out safetyListResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	return out
}

func TestStudioSafetyAPI(t *testing.T) {
	ui, dir := safetyAPIFixture(t)

	rec := do(t, ui.API(), http.MethodGet, "/admin/api/studio/safety", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	list := decodeSafety(t, rec.Body.Bytes())
	if !list.Editable || list.Mode != "configdir" || len(list.Profiles) != 1 {
		t.Fatalf("initial list = %+v", list)
	}
	if list.RestartRequired {
		t.Fatalf("an untouched config should be live: %+v", list)
	}

	rec = do(t, ui.API(), http.MethodPost, "/admin/api/studio/safety/profiles", map[string]any{
		"profile": map[string]any{"name": "strict", "filters": []string{"source-attribution", "affect-guard"}},
		"mtime":   list.MTime,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	list = decodeSafety(t, rec.Body.Bytes())
	if len(list.Profiles) != 2 || !list.RestartRequired {
		t.Fatalf("created list = %+v", list)
	}
	if _, err := config.LoadDir(dir, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("configdir does not load after create: %v", err)
	}

	rec = do(t, ui.API(), http.MethodPost, "/admin/api/studio/safety/validate", map[string]any{
		"kind":     "profile",
		"creating": true,
		"profile":  map[string]any{"name": "bad", "filters": []string{"nope"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("validate: %d %s", rec.Code, rec.Body.String())
	}
	var verdict struct {
		OK     bool     `json:"ok"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &verdict); err != nil {
		t.Fatal(err)
	}
	if verdict.OK || len(verdict.Errors) == 0 {
		t.Fatalf("unknown filter verdict = %+v", verdict)
	}

	rec = do(t, ui.API(), http.MethodPut, "/admin/api/studio/safety/profiles/strict", map[string]any{
		"profile": map[string]any{"name": "renamed", "filters": []string{}},
		"mtime":   list.MTime,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body.String())
	}

	// A stale mtime is a conflict, not a silent overwrite.
	rec = do(t, ui.API(), http.MethodPost, "/admin/api/studio/safety/profiles", map[string]any{
		"profile": map[string]any{"name": "other", "filters": []string{}},
		"mtime":   list.MTime - 1,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflict: %d %s", rec.Code, rec.Body.String())
	}

	// base is referenced by an agent, so deletion is refused with a reason.
	rec = do(t, ui.API(), http.MethodDelete, "/admin/api/studio/safety/profiles/base?mtime=0", nil)
	if rec.Code != http.StatusBadRequest || rec.Body.Len() == 0 {
		t.Fatalf("delete in-use profile: %d %s", rec.Code, rec.Body.String())
	}
}

func TestStudioSafetyClassifierAPI(t *testing.T) {
	ui, _ := safetyAPIFixture(t)
	rec := do(t, ui.API(), http.MethodPost, "/admin/api/studio/safety/classifiers", map[string]any{
		"classifier": map[string]any{
			"name":       "moderation",
			"model":      "default",
			"categories": map[string]string{"safe": "allow", "harm": "block"},
			"threshold":  0.75,
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create classifier: %d %s", rec.Code, rec.Body.String())
	}
	list := decodeSafety(t, rec.Body.Bytes())
	if len(list.Classifiers) != 1 || list.Classifiers[0].Name != "moderation" {
		t.Fatalf("classifiers = %+v", list.Classifiers)
	}
	if list.Classifiers[0].Threshold == nil || *list.Classifiers[0].Threshold != 0.75 {
		t.Errorf("threshold = %v", list.Classifiers[0].Threshold)
	}

	rec = do(t, ui.API(), http.MethodPut, "/admin/api/studio/safety/classifiers/moderation", map[string]any{
		"classifier": map[string]any{
			"name": "moderation", "model": "default",
			"categories": map[string]string{"safe": "allow", "harm": "review"},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update classifier: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, ui.API(), http.MethodDelete, "/admin/api/studio/safety/classifiers/moderation?mtime=0", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete classifier: %d %s", rec.Code, rec.Body.String())
	}
}
