package webui

import (
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

	"agentflow/internal/config"
	"agentflow/internal/core/credentials"
	"agentflow/internal/core/metrics"
	"agentflow/internal/drivers/llm"
)

const testConfig = `# a comment outside the models section that must survive persist
version: "2"
models:
  default:
    provider: openai
    model: gpt-4o-mini
    api_key: sk-testsecret1234
agents:
  bot:
    loop: ./loop.lua
`

// fixture builds a UI against a temp config file and returns the pieces the
// tests poke at.
type fixture struct {
	ui     *UI
	models *llm.Manager
	cfgDir string
	cfgPath string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agentflow.yaml")
	if err := os.WriteFile(path, []byte(testConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("fixture config does not load: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := llm.NewManager(cfg.Models, log)
	ui := New(Deps{
		ConfigPath: path,
		Cfg:        cfg,
		Models:     mgr,
		StartedAt:  time.Now(),
		Version:    "test",
	})
	return &fixture{ui: ui, models: mgr, cfgDir: dir, cfgPath: path}
}

func do(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("response not JSON: %v\n%s", err, rec.Body.String())
	}
	return v
}

// --- static + auth ---------------------------------------------------------

func TestStaticServesSPA(t *testing.T) {
	f := newFixture(t)
	h := f.ui.Static()
	for _, p := range []string{"/", "/app.js", "/styles.css"} {
		rec := do(t, h, http.MethodGet, p, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d", p, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Fatalf("%s: empty body", p)
		}
	}
	rec := do(t, h, http.MethodGet, "/../../etc/passwd", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("traversal: got %d", rec.Code)
	}
	rec = do(t, h, http.MethodGet, "/nonexistent", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path: got %d", rec.Code)
	}
}

func TestAPIRequiresTokenWhenMounted(t *testing.T) {
	f := newFixture(t)
	reg := metrics.NewRegistry()
	admin := metrics.NewAdminServer("127.0.0.1:0", "tok123", reg, nil)
	admin.Mount("/admin/api/", f.ui.API(), true)
	admin.Mount("/", f.ui.Static(), false)
	srv := httptest.NewServer(admin.Handler())
	defer srv.Close()

	// No token: API rejected, static allowed (it is inert without the API).
	resp, err := http.Get(srv.URL + "/admin/api/state")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthed API: got %d", resp.StatusCode)
	}
	resp, err = http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("static: got %d", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/admin/api/state", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authed API: got %d", resp.StatusCode)
	}
}

// --- models ----------------------------------------------------------------

func TestModelsListMasksSecrets(t *testing.T) {
	f := newFixture(t)
	rec := do(t, f.ui.API(), http.MethodGet, "/admin/api/models", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "sk-testsecret1234") {
		t.Fatal("list leaked the API key")
	}
	if !strings.Contains(body, "…1234") {
		t.Fatalf("list missing last-4 fingerprint: %s", body)
	}
	v := decode(t, rec)
	models := v["models"].([]any)
	m := models[0].(map[string]any)
	if m["in_runtime"] != true || m["in_file"] != true || m["drift"] != false {
		t.Fatalf("fresh state should be in sync: %v", m)
	}
}

func TestModelUpsertHotAppliesAndDrifts(t *testing.T) {
	f := newFixture(t)
	rec := do(t, f.ui.API(), http.MethodPut, "/admin/api/models/fast", map[string]any{
		"provider": "anthropic", "model": "claude-haiku-4-5", "api_key": "sk-ant-9999",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert: got %d %s", rec.Code, rec.Body.String())
	}
	cfg, err := f.models.Get("fast")
	if err != nil || cfg.Provider != "anthropic" {
		t.Fatalf("hot apply failed: %v %v", cfg, err)
	}
	// New runtime-only entry shows drift; existing untouched entry does not.
	rec = do(t, f.ui.API(), http.MethodGet, "/admin/api/models", nil)
	v := decode(t, rec)
	for _, mAny := range v["models"].([]any) {
		m := mAny.(map[string]any)
		if m["name"] == "fast" && (m["drift"] != true || m["in_file"] != false) {
			t.Fatalf("fast should be drifted runtime-only: %v", m)
		}
		if m["name"] == "default" && m["drift"] != false {
			t.Fatalf("default should be clean: %v", m)
		}
	}
}

func TestModelUpsertBlankKeyKeepsExisting(t *testing.T) {
	f := newFixture(t)
	rec := do(t, f.ui.API(), http.MethodPut, "/admin/api/models/default", map[string]any{
		"provider": "openai", "model": "gpt-5", // no api_key
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert: got %d", rec.Code)
	}
	cfg, _ := f.models.Get("default")
	if cfg.APIKey != "sk-testsecret1234" {
		t.Fatalf("blank api_key wiped the secret: %q", cfg.APIKey)
	}
	if cfg.Model != "gpt-5" {
		t.Fatalf("other fields must still update: %+v", cfg)
	}
}

func TestModelUpsertValidation(t *testing.T) {
	f := newFixture(t)
	api := f.ui.API()
	if rec := do(t, api, http.MethodPut, "/admin/api/models/x", map[string]any{
		"provider": "bogus", "model": "m",
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad provider: got %d", rec.Code)
	}
	if rec := do(t, api, http.MethodPut, "/admin/api/models/x", map[string]any{
		"provider": "openai",
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing model: got %d", rec.Code)
	}
	if rec := do(t, api, http.MethodPut, "/admin/api/models/x", map[string]any{
		"provider": "openai", "model": "m", "timeout": "not-a-duration",
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad timeout: got %d", rec.Code)
	}
}

func TestModelRemove(t *testing.T) {
	f := newFixture(t)
	do(t, f.ui.API(), http.MethodDelete, "/admin/api/models/default", nil)
	if _, err := f.models.Get("default"); err == nil {
		t.Fatal("remove did not apply to the manager")
	}
}

func TestModelsPersistRoundTrip(t *testing.T) {
	f := newFixture(t)
	do(t, f.ui.API(), http.MethodPut, "/admin/api/models/fast", map[string]any{
		"provider": "gemini", "model": "gemini-3-flash",
	})
	rec := do(t, f.ui.API(), http.MethodPost, "/admin/api/models/persist", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("persist: got %d %s", rec.Code, rec.Body.String())
	}

	// The file now loads with both models, and the unrelated comment survived.
	raw, err := os.ReadFile(f.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "a comment outside the models section") {
		t.Fatalf("persist clobbered unrelated content:\n%s", raw)
	}
	cfg, err := config.Load(f.cfgPath)
	if err != nil {
		t.Fatalf("persisted file does not load: %v", err)
	}
	if _, ok := cfg.Models["fast"]; !ok {
		t.Fatalf("persisted file missing fast: %v", cfg.Models)
	}
	if _, err := os.Stat(f.cfgPath + ".bak"); err != nil {
		t.Fatalf("no .bak written: %v", err)
	}

	// Drift is cleared after persist.
	rec = do(t, f.ui.API(), http.MethodGet, "/admin/api/models", nil)
	v := decode(t, rec)
	for _, mAny := range v["models"].([]any) {
		m := mAny.(map[string]any)
		if m["drift"] != false {
			t.Fatalf("post-persist drift should be clean: %v", m)
		}
	}
}

func TestModelsRevert(t *testing.T) {
	f := newFixture(t)
	do(t, f.ui.API(), http.MethodPut, "/admin/api/models/default", map[string]any{
		"provider": "openai", "model": "gpt-5",
	})
	rec := do(t, f.ui.API(), http.MethodPost, "/admin/api/models/revert", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("revert: got %d %s", rec.Code, rec.Body.String())
	}
	cfg, _ := f.models.Get("default")
	if cfg.Model != "gpt-4o-mini" {
		t.Fatalf("revert did not restore file state: %+v", cfg)
	}
}

func TestModelTestUnknownModel(t *testing.T) {
	f := newFixture(t)
	rec := do(t, f.ui.API(), http.MethodPost, "/admin/api/models/nope/test", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d", rec.Code)
	}
}

// --- config ----------------------------------------------------------------

func TestConfigGetMasksView(t *testing.T) {
	f := newFixture(t)
	rec := do(t, f.ui.API(), http.MethodGet, "/admin/api/config", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	v := decode(t, rec)
	if !strings.Contains(v["raw"].(string), "sk-testsecret1234") {
		t.Fatal("raw editor must carry the true file content")
	}
	view, _ := json.Marshal(v["view"])
	if strings.Contains(string(view), "sk-testsecret1234") {
		t.Fatal("structured view leaked the API key")
	}
	if !strings.Contains(string(view), "••••") {
		t.Fatalf("view not masked: %s", view)
	}
	if v["mtime"].(float64) == 0 {
		t.Fatal("mtime missing")
	}
}

func TestConfigValidate(t *testing.T) {
	f := newFixture(t)
	api := f.ui.API()
	rec := do(t, api, http.MethodPost, "/admin/api/config/validate", map[string]any{"raw": testConfig})
	if v := decode(t, rec); v["ok"] != true {
		t.Fatalf("valid config rejected: %v", v)
	}
	bad := strings.Replace(testConfig, "loop: ./loop.lua", "loop: ./loop.lua\n    bogus_field: 1", 1)
	rec = do(t, api, http.MethodPost, "/admin/api/config/validate", map[string]any{"raw": bad})
	v := decode(t, rec)
	if v["ok"] != false || !strings.Contains(v["error"].(string), "bogus_field") {
		t.Fatalf("strict validation did not name the field: %v", v)
	}
}

func TestConfigSaveConflict(t *testing.T) {
	f := newFixture(t)
	// Grab the current mtime, then touch the file so the saved mtime is stale.
	rec0 := do(t, f.ui.API(), http.MethodGet, "/admin/api/config", nil)
	mtime := int64(decode(t, rec0)["mtime"].(float64))
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(f.cfgPath, future, future); err != nil {
		t.Fatal(err)
	}
	rec := do(t, f.ui.API(), http.MethodPost, "/admin/api/config/save", map[string]any{
		"raw": testConfig, "mtime": mtime,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale mtime: got %d %s", rec.Code, rec.Body.String())
	}
}

func TestConfigSaveWritesBak(t *testing.T) {
	f := newFixture(t)
	rec0 := do(t, f.ui.API(), http.MethodGet, "/admin/api/config", nil)
	mtime := int64(decode(t, rec0)["mtime"].(float64))
	updated := strings.Replace(testConfig, "gpt-4o-mini", "gpt-5", 1)
	rec := do(t, f.ui.API(), http.MethodPost, "/admin/api/config/save", map[string]any{
		"raw": updated, "mtime": mtime,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("save: got %d %s", rec.Code, rec.Body.String())
	}
	bak, err := os.ReadFile(f.cfgPath + ".bak")
	if err != nil || !strings.Contains(string(bak), "gpt-4o-mini") {
		t.Fatalf("backup missing original content: %v", err)
	}
	cur, _ := os.ReadFile(f.cfgPath)
	if !strings.Contains(string(cur), "gpt-5") {
		t.Fatal("file not updated")
	}
}

func TestConfigSaveRejectsInvalid(t *testing.T) {
	f := newFixture(t)
	rec0 := do(t, f.ui.API(), http.MethodGet, "/admin/api/config", nil)
	mtime := int64(decode(t, rec0)["mtime"].(float64))
	rec := do(t, f.ui.API(), http.MethodPost, "/admin/api/config/save", map[string]any{
		"raw": "not: [valid", "mtime": mtime,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid config saved: got %d", rec.Code)
	}
}

// --- metrics + logs + credentials ------------------------------------------

func TestMetricsHandler(t *testing.T) {
	f := newFixture(t)
	reg := metrics.NewRegistry()
	c := metrics.NewCounter("test_counter", "test")
	reg.Register(c)
	c.Add(7)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.ui.deps.History = reg.StartSampler(ctx, 10*time.Millisecond, 10)
	time.Sleep(50 * time.Millisecond)

	rec := do(t, f.ui.API(), http.MethodGet, "/admin/api/metrics", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	v := decode(t, rec)
	if v["latest"].(map[string]any)["test_counter"].(float64) != 7 {
		t.Fatalf("latest wrong: %v", v["latest"])
	}
	pts := v["series"].(map[string]any)["test_counter"].([]any)
	if len(pts) < 2 {
		t.Fatalf("sampler did not accumulate points: %v", pts)
	}
}

func TestLogRingAndTee(t *testing.T) {
	ring := NewLogRing(3)
	ring.Push("one")
	ring.Push("two")
	ring.Push("three")
	ring.Push("four") // evicts "one"
	recent := ring.Recent()
	if len(recent) != 3 || recent[0] != "two" {
		t.Fatalf("ring did not trim: %v", recent)
	}

	ch, unsub := ring.Subscribe()
	ring.Push("five")
	select {
	case line := <-ch:
		if line != "five" {
			t.Fatalf("subscriber got %q", line)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber received nothing")
	}
	unsub()

	// The tee handler formats one line and forwards downstream.
	ring2 := NewLogRing(10)
	var captured []string
	down := &captureHandler{lines: &captured}
	h := NewTeeHandler(down, ring2)
	log := slog.New(h)
	log.Info("hello\nworld", "key", "val")
	if len(captured) != 1 {
		t.Fatalf("downstream not called: %v", captured)
	}
	got := ring2.Recent()
	if len(got) != 1 || !strings.Contains(got[0], "hello world") || strings.Contains(got[0], "\n") {
		t.Fatalf("bad tee line: %q", got)
	}
	if !strings.Contains(got[0], "INF") || !strings.Contains(got[0], "key=val") {
		t.Fatalf("tee line missing level/attrs: %q", got[0])
	}
}

type captureHandler struct{ lines *[]string }

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	*h.lines = append(*h.lines, r.Message)
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func TestLogsSSE(t *testing.T) {
	f := newFixture(t)
	ring := NewLogRing(10)
	ring.Push("boot line")
	f.ui.deps.Logs = ring
	srv := httptest.NewServer(f.ui.API())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/admin/api/logs", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type: %q", ct)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		ring.Push("live line")
	}()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	got := string(buf[:n])
	if !strings.Contains(got, "data: boot line") {
		t.Fatalf("recent lines not replayed: %q", got)
	}
	n, _ = resp.Body.Read(buf)
	got = string(buf[:n])
	if !strings.Contains(got, "data: live line") {
		t.Fatalf("live lines not streamed: %q", got)
	}
}

func TestCredentialUsers(t *testing.T) {
	f := newFixture(t)
	store, err := credentials.Open(filepath.Join(f.cfgDir, "creds.db"), "master-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Put(ctx, "u_1", "openai", "api_key", "sk-live-9999", "", ""); err != nil {
		t.Fatal(err)
	}
	f.ui.deps.Creds = store

	rec := do(t, f.ui.API(), http.MethodGet, "/admin/api/credentials/users", nil)
	v := decode(t, rec)
	users := v["users"].([]any)
	if len(users) != 1 || users[0] != "u_1" {
		t.Fatalf("users: %v", users)
	}
}

func TestStateNoSecrets(t *testing.T) {
	f := newFixture(t)
	rec := do(t, f.ui.API(), http.MethodGet, "/admin/api/state", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "sk-testsecret1234") {
		t.Fatal("state leaked the API key")
	}
	v := decode(t, rec)
	if v["agents"].([]any)[0].(map[string]any)["name"] != "bot" {
		t.Fatalf("agents missing: %v", v)
	}
}
