package adminapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	_ "agentflow/internal/builtins"
	"agentflow/internal/config"
	"agentflow/internal/core/studio"
	"agentflow/internal/luacode"
)

func TestStudioPluginsAPI(t *testing.T) {
	t.Cleanup(func() {
		luacode.SetPluginDir("")
		luacode.SetPackages(nil)
	})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "system.yaml"), []byte("version: \"1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg, err := config.LoadDir(dir, log)
	if err != nil {
		t.Fatal(err)
	}
	luacode.SetPluginDir(cfg.Plugins.Dir)
	luacode.SetPackages(cfg.Packages)

	// Nil Plugins answers 503 cleanly.
	disabledUI := New(Deps{ConfigDir: dir, Cfg: cfg})
	if rec := do(t, disabledUI.API(), http.MethodGet, "/admin/api/studio/plugins", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled GET status = %d, want 503", rec.Code)
	}

	var supportCalls, packageCalls int
	pStore := studio.NewPlugins(cfg, dir, "", studio.PluginHooks{
		OnReloadSupport:  func() { supportCalls++ },
		OnReloadPackages: func() { packageCalls++ },
	}, log)
	ui := New(Deps{ConfigDir: dir, Cfg: cfg, Plugins: pStore})

	decode := func(body []byte) studio.PluginsList {
		t.Helper()
		var out studio.PluginsList
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode: %v\n%s", err, body)
		}
		return out
	}

	rec := do(t, ui.API(), http.MethodGet, "/admin/api/studio/plugins", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	list := decode(rec.Body.Bytes())
	if !list.Editable || len(list.Builtins) == 0 {
		t.Fatalf("unexpected initial plugins list: %+v", list)
	}

	// Validate invalid source returns ok:false with errors.
	rec = do(t, ui.API(), http.MethodPost, "/admin/api/studio/plugins/validate", map[string]any{
		"kind": "builtin", "name": "token_budget", "source": "const x =",
	})
	var v struct {
		OK     bool     `json:"ok"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.OK || len(v.Errors) == 0 {
		t.Fatalf("validate invalid: %d %s", rec.Code, rec.Body.String())
	}

	// PUT builtin saves shadow file and triggers reload.
	custom := "// af-prelude-version: 2\nfunction token_budget(m, b) { return m; }\n"
	rec = do(t, ui.API(), http.MethodPut, "/admin/api/studio/plugins/builtins/token_budget", map[string]any{
		"source": custom, "mtime": 0,
	})
	if rec.Code != http.StatusOK || supportCalls != 1 {
		t.Fatalf("PUT builtin: %d %s (supportCalls=%d)", rec.Code, rec.Body.String(), supportCalls)
	}
	list = decode(rec.Body.Bytes())
	var tbMTime int64
	for _, b := range list.Builtins {
		if b.Name == "token_budget" {
			tbMTime = b.MTime
			if !b.Shadowed {
				t.Fatal("expected token_budget shadowed after PUT")
			}
		}
	}

	// Stale mtime returns 409 Conflict.
	rec = do(t, ui.API(), http.MethodPut, "/admin/api/studio/plugins/builtins/token_budget", map[string]any{
		"source": custom, "mtime": tbMTime - 1,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale PUT builtin status = %d, want 409: %s", rec.Code, rec.Body.String())
	}

	// DELETE builtin reverts shadow.
	rec = do(t, ui.API(), http.MethodDelete, "/admin/api/studio/plugins/builtins/token_budget?mtime="+strconv.FormatInt(tbMTime, 10), nil)
	if rec.Code != http.StatusOK || supportCalls != 2 {
		t.Fatalf("DELETE builtin: %d %s", rec.Code, rec.Body.String())
	}

	// Package create -> save file -> delete file -> delete package.
	pkgSrc := "// af-prelude-version: 2\nvar M = {};\n"
	rec = do(t, ui.API(), http.MethodPost, "/admin/api/studio/plugins/packages", map[string]any{
		"name": "helpers", "file": "init.ts", "source": pkgSrc,
	})
	if rec.Code != http.StatusOK || packageCalls != 1 {
		t.Fatalf("POST package: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, ui.API(), http.MethodPut, "/admin/api/studio/plugins/packages/helpers/files/extra.ts", map[string]any{
		"source": pkgSrc, "mtime": 0,
	})
	if rec.Code != http.StatusOK || packageCalls != 2 {
		t.Fatalf("PUT package file: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, ui.API(), http.MethodDelete, "/admin/api/studio/plugins/packages/helpers/files/extra.ts?mtime=0", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE package file: %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, ui.API(), http.MethodDelete, "/admin/api/studio/plugins/packages/helpers", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE package: %d %s", rec.Code, rec.Body.String())
	}
}
