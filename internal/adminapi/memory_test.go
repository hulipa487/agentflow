package adminapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/core/studio"
)

type memoryListResponse struct {
	studio.MemoryList
	RestartRequired bool `json:"restart_required"`
}

func TestStudioMemoryAPI(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "system.yaml"), []byte(`
version: "1"
models:
  default: { provider: openai, model: gpt-4o-mini, base_url: http://127.0.0.1:11434/v1 }
memory:
  backends:
    main_db: { provider: sqlite, config: { path: ./data/agentflow.db } }
    pg: { provider: postgres, config: { url: "postgres://app:s3cret@db:5432/mem" } }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg, err := config.LoadDir(dir, log)
	if err != nil {
		t.Fatal(err)
	}
	ui := New(Deps{ConfigDir: dir, Cfg: cfg, Memory: studio.NewMemory(cfg, dir, "", log)})
	decode := func(body []byte) memoryListResponse {
		t.Helper()
		var out memoryListResponse
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode: %v\n%s", err, body)
		}
		return out
	}

	rec := do(t, ui.API(), http.MethodGet, "/admin/api/studio/memory", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "s3cret") {
		t.Fatalf("the list leaked a backend password: %s", rec.Body.String())
	}
	list := decode(rec.Body.Bytes())
	if list.RestartRequired || len(list.Backends) != 2 {
		t.Fatalf("initial list = %+v", list)
	}

	rec = do(t, ui.API(), http.MethodPost, "/admin/api/studio/memory/validate", map[string]any{
		"kind": "profile", "creating": true,
		"profile": map[string]any{"name": "notes", "stores": []map[string]any{{"name": "facts", "backend": "missing"}}},
	})
	var v struct {
		OK     bool     `json:"ok"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.OK || len(v.Errors) != 1 {
		t.Fatalf("validate a bad profile: %s", rec.Body.String())
	}

	rec = do(t, ui.API(), http.MethodPost, "/admin/api/studio/memory/profiles", map[string]any{
		"profile": map[string]any{"name": "notes", "stores": []map[string]any{{"name": "facts", "backend": "pg", "table": "notes"}}},
		"mtime":   list.MTime,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create profile: %d %s", rec.Code, rec.Body.String())
	}
	list = decode(rec.Body.Bytes())
	if !list.RestartRequired || len(list.Profiles) != 1 {
		t.Fatalf("after create = %+v", list)
	}

	rec = do(t, ui.API(), http.MethodPut, "/admin/api/studio/memory/backends/pg", map[string]any{
		"backend": map[string]any{"name": "other", "provider": "postgres"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("renaming a backend: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, ui.API(), http.MethodDelete, "/admin/api/studio/memory/backends/pg", nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "notes") {
		t.Fatalf("deleting a backend in use: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, ui.API(), http.MethodDelete, "/admin/api/studio/memory/profiles/notes?mtime=1", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("a stale delete: %d %s", rec.Code, rec.Body.String())
	}
}
