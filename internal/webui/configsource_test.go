package webui

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
	"agentflow/internal/drivers/llm"
)

// TestConfigDirDegradesWithANamedReason: an instance started with -configdir
// has several fragments rather than one file, so the Config tab and the model
// persist/revert paths — all of which swap a single file — must say so. Before
// this they silently pointed at the -config default ("agentflow.yaml"), which
// under -configdir is a file that usually does not exist: the Config tab 500ed,
// and a stray agentflow.yaml in the working directory would have been edited
// instead of the deployment's own config.
func TestConfigDirDegradesWithANamedReason(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	ui := New(Deps{ConfigDir: dir, Cfg: f.ui.deps.Cfg, Models: f.models, Version: "test"})

	cases := []struct {
		name       string
		method     string
		path       string
		body       any
		wantStatus int
	}{
		{"get", http.MethodGet, "/admin/api/config", nil, http.StatusOK},
		{"validate", http.MethodPost, "/admin/api/config/validate", map[string]any{"raw": "version: \"2\""}, http.StatusOK},
		{"save", http.MethodPost, "/admin/api/config/save", map[string]any{"raw": "version: \"2\"", "mtime": 0}, http.StatusOK},
		{"persist", http.MethodPost, "/admin/api/models/persist", nil, http.StatusBadRequest},
		{"revert", http.MethodPost, "/admin/api/models/revert", nil, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := do(t, ui.API(), c.method, c.path, c.body)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d; want %d (%s)", rec.Code, c.wantStatus, rec.Body.String())
			}
			v := decode(t, rec)
			if v["ok"] == true {
				t.Fatalf("reported ok=true for a configdir instance: %s", rec.Body.String())
			}
			msg, _ := v["error"].(string)
			if !strings.Contains(msg, dir) {
				t.Fatalf("error %q does not name the config directory %q", msg, dir)
			}
			if !strings.Contains(msg, "config directory") {
				t.Fatalf("error %q does not say why the pane is unavailable", msg)
			}
		})
	}
}

// TestConfigDirStillServesTheModelsTab: hot-applying a model to this process
// needs no file, so it keeps working under -configdir. Only the file-touching
// operations degrade; the list reports the file as unavailable rather than
// pretending the (nonexistent) config file has no models.
func TestConfigDirStillServesTheModelsTab(t *testing.T) {
	f := newFixture(t)
	ui := New(Deps{ConfigDir: t.TempDir(), Cfg: f.ui.deps.Cfg, Models: f.models, Version: "test"})

	rec := do(t, ui.API(), http.MethodGet, "/admin/api/models", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("models list: got %d", rec.Code)
	}
	v := decode(t, rec)
	if v["ok"] != true {
		t.Fatalf("models list not ok: %s", rec.Body.String())
	}
	if _, present := v["file_error"]; !present {
		t.Fatalf("expected file_error naming the reason, got %s", rec.Body.String())
	}
}

// TestMaskTreeRedactsStoreTargets: every key that only ever holds a secret is
// masked, including the four the original list missed, and a store target is
// masked in all four of its homes — a DSN can carry a password.
func TestMaskTreeRedactsStoreTargets(t *testing.T) {
	const secret = "s3cr3t-do-not-print"
	in := map[string]any{
		"api_key":      secret,
		"secret_key":   secret, // media/files S3
		"access_key":   secret, // media/files S3
		"api_token":    secret, // Cloudflare browser
		"secret_token": secret, // channel webhook auth
		"persistence":  "postgres://u:" + secret + "@db/x",
		"runtime": map[string]any{
			"credentials": map[string]any{"path": "postgres://u:" + secret + "@db/y"},
		},
		"memory": map[string]any{
			"backends": map[string]any{
				"hot": map[string]any{"config": map[string]any{"path": "/data/" + secret + ".db"}},
			},
		},
		// A placeholder reveals nothing and the operator needs to see it.
		"models": map[string]any{
			"default": map[string]any{"api_key": "${OPENAI_API_KEY}"},
		},
	}
	got := maskTree(in).(map[string]any)

	flat := map[string]any{
		"secret_key":   got["secret_key"],
		"access_key":   got["access_key"],
		"api_token":    got["api_token"],
		"secret_token": got["secret_token"],
		"persistence":  got["persistence"],
	}
	for k, v := range flat {
		if v != "••••" {
			t.Errorf("%s = %v; want it masked", k, v)
		}
	}
	credPath := got["runtime"].(map[string]any)["credentials"].(map[string]any)["path"]
	if credPath != "••••" {
		t.Errorf("runtime.credentials.path = %v; want it masked", credPath)
	}
	bePath := got["memory"].(map[string]any)["backends"].(map[string]any)["hot"].(map[string]any)["config"].(map[string]any)["path"]
	if bePath != "••••" {
		t.Errorf("memory.backends.hot.config.path = %v; want it masked", bePath)
	}
	placeholder := got["models"].(map[string]any)["default"].(map[string]any)["api_key"]
	if placeholder != "${OPENAI_API_KEY}" {
		t.Errorf("placeholder = %v; want it kept verbatim", placeholder)
	}

	// And the raw rendering must not contain the secret.
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("masked view still contains the secret:\n%s", encoded)
	}
}

// TestMaskTreeKeepsRoutePathsReadable: "path" is a store target in some homes
// and a route on the shared listener in gateway.channels[].path. Masking every
// "path" would blank the route an operator is auditing — including the
// unguessable prefix they are meant to check — in the one view they audit a
// config with.
func TestMaskTreeKeepsRoutePathsReadable(t *testing.T) {
	in := map[string]any{
		"gateway": map[string]any{
			"channels": []any{
				map[string]any{"type": "webhook", "path": "/webhook/bot/3f9a-uuid/"},
			},
		},
	}
	got := maskTree(in).(map[string]any)
	ch := got["gateway"].(map[string]any)["channels"].([]any)[0].(map[string]any)
	if ch["path"] != "/webhook/bot/3f9a-uuid/" {
		t.Fatalf("channel route path = %v; want it readable", ch["path"])
	}
}

// persistFixture writes a config whose only model sources its key from an env
// placeholder, loads it the way boot does, and returns a UI over it plus the
// live manager. config.Load expands ${VAR} across the raw bytes before decoding,
// so the manager holds the resolved secret — which is the whole reason persist
// has to be careful about what it writes back.
func persistFixture(t *testing.T) (*UI, *llm.Manager, string, string) {
	t.Helper()
	const secret = "sk-live-secret-value"
	t.Setenv("AGENTFLOW_TEST_KEY", secret)

	dir := t.TempDir()
	path := filepath.Join(dir, "agentflow.yaml")
	raw := `version: "2"
models:
  default:
    provider: openai
    model: gpt-4o-mini
    api_key: ${AGENTFLOW_TEST_KEY}
agents:
  bot:
    loop: ./loop.lua
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("fixture config: %v", err)
	}
	if cfg.Models["default"].APIKey != secret {
		t.Fatalf("fixture did not expand the placeholder: got %q", cfg.Models["default"].APIKey)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := llm.NewManager(cfg.Models, log)
	return New(Deps{ConfigPath: path, Cfg: cfg, Models: mgr, Version: "test"}), mgr, path, secret
}

func readConfig(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestPersistKeepsAPlaceholderOutOfTheFile: config.Load expands ${VAR} before
// decoding, so the live manager holds the literal secret. Writing the live set
// verbatim put that secret into the config file and silently lost the
// placeholder the operator used to keep it out — and moved the config epoch,
// which hashes the file before expansion precisely so rotating a secret does
// not move it.
func TestPersistKeepsAPlaceholderOutOfTheFile(t *testing.T) {
	ui, _, path, secret := persistFixture(t)

	if err := ui.persistModels(); err != nil {
		t.Fatalf("persist: %v", err)
	}
	after := readConfig(t, path)
	if strings.Contains(after, secret) {
		t.Fatalf("persist wrote the expanded secret into the config file:\n%s", after)
	}
	if !strings.Contains(after, "${AGENTFLOW_TEST_KEY}") {
		t.Fatalf("persist lost the placeholder:\n%s", after)
	}
}

// TestPersistWritesAKeyTheOperatorReplaced: the placeholder is only preserved
// while it still explains the live value. Once the operator sets a different key
// through the console, that value is what must be written — otherwise their edit
// would be silently reverted on the next restart.
func TestPersistWritesAKeyTheOperatorReplaced(t *testing.T) {
	ui, mgr, path, secret := persistFixture(t)

	mgr.Upsert("default", config.Model{
		Provider: "openai", Model: "gpt-4o-mini", APIKey: "sk-replaced-by-operator",
	})
	if err := ui.persistModels(); err != nil {
		t.Fatalf("persist: %v", err)
	}
	after := readConfig(t, path)
	if !strings.Contains(after, "sk-replaced-by-operator") {
		t.Fatalf("the operator's replacement key was not written:\n%s", after)
	}
	if strings.Contains(after, "${AGENTFLOW_TEST_KEY}") {
		t.Fatalf("the stale placeholder survived a replaced key:\n%s", after)
	}
	if strings.Contains(after, secret) {
		t.Fatalf("the old secret was written:\n%s", after)
	}
}

// TestValidateRawConfigResolvesAgainstTheConfigDirectory: config.Load rebases
// relative prompt file: paths against the config's own directory and refuses to
// boot when one cannot be read. Validating from the system temp directory
// therefore reported a false failure for a config that boots fine, and the
// console's Validate disagreed with its own Save about the same text.
func TestValidateRawConfigResolvesAgainstTheConfigDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "shared.md"), []byte("be helpful"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw := `version: "2"
prompts:
  shared:
    file: ./prompts/shared.md
models:
  default:
    provider: openai
    model: gpt-4o-mini
agents:
  bot:
    loop: ./loop.lua
`
	if err := validateRawConfig(raw, dir); err != nil {
		t.Fatalf("validating from the config's own directory: %v", err)
	}
	// The same text from an unrelated directory cannot resolve ./prompts —
	// which is exactly the false failure this fixes.
	if err := validateRawConfig(raw, t.TempDir()); err == nil {
		t.Fatal("expected an unresolvable prompt file to fail from a foreign directory")
	}
}
