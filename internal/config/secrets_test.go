package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/core/credentials"
)

func TestParseSecretRef(t *testing.T) {
	cases := []struct {
		raw     string
		literal string
		env     string
		def     string
		hasDef  bool
		cred    string
	}{
		{"plainvalue", "plainvalue", "", "", false, ""},
		{"${OPENAI_API_KEY}", "", "OPENAI_API_KEY", "", false, ""},
		{"${REGION:-us-east-1}", "", "REGION", "us-east-1", true, ""},
		{"${EMPTY:-}", "", "EMPTY", "", true, ""},
		{"cred:github_token", "", "", "", false, "github_token"},
		{"key-${SUFFIX}", "key-${SUFFIX}", "", "", false, ""}, // composite = literal
		{"", "", "", "", false, ""},
	}
	for _, c := range cases {
		ref := ParseSecretRef(c.raw)
		if ref.Literal != c.literal || ref.Env != c.env || ref.EnvDefault != c.def ||
			ref.HasDefault != c.hasDef || ref.Cred != c.cred {
			t.Fatalf("ParseSecretRef(%q) = %+v", c.raw, ref)
		}
	}
}

func TestCredentialNameAndOpaqueMarker(t *testing.T) {
	if got := CredentialName("${FOO}"); got != "FOO" {
		t.Fatalf("CredentialName env: %q", got)
	}
	if got := CredentialName("cred:svc"); got != "cred:svc" {
		t.Fatalf("CredentialName cred: %q", got)
	}
	m, ok := OpaqueMarker("${FOO}").(map[string]any)
	if !ok || m["env"] != "FOO" {
		t.Fatalf("OpaqueMarker env: %v", OpaqueMarker("${FOO}"))
	}
	m, ok = OpaqueMarker("cred:svc").(map[string]any)
	if !ok || m["cred"] != "svc" {
		t.Fatalf("OpaqueMarker cred: %v", OpaqueMarker("cred:svc"))
	}
	if got := OpaqueMarker("literal"); got != "literal" {
		t.Fatalf("OpaqueMarker literal: %v", got)
	}
}

// testStore opens a throwaway credential store.
func testStore(t *testing.T, key string) *credentials.Store {
	t.Helper()
	s, err := credentials.Open(filepath.Join(t.TempDir(), "cred.db"), key, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestResolverOrder: process env beats the store; the store (under the empty
// engine-wide user UUID) backs ${VAR} fallbacks and cred: references;
// literals pass through; ${VAR:-default} uses the default when unset and
// never consults the store.
func TestResolverOrder(t *testing.T) {
	ctx := context.Background()
	store := testStore(t, "master")
	if err := store.Put(ctx, "", "STORED_KEY", "api", "store-value", "", ""); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ENV_KEY", "env-value")

	r := &Resolver{Store: store}
	cases := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{"literal passes through", "abc", "abc", true},
		{"env wins", "${ENV_KEY}", "env-value", true},
		{"store backs env ref", "${STORED_KEY}", "store-value", true},
		{"cred ref", "cred:STORED_KEY", "store-value", true},
		{"default when unset", "${NOPE:-fallback}", "fallback", true},
		{"env beats default", "${ENV_KEY:-fallback}", "env-value", true},
		{"unresolvable", "${DEFINITELY_MISSING_VAR}", "", false},
		{"unresolvable cred", "cred:MISSING_SVC", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, ok := r.Resolve(ctx, c.raw)
			if ok != c.ok || (ok && v != c.want) {
				t.Fatalf("Resolve(%q) = %q, %v; want %q, %v", c.raw, v, ok, c.want, c.ok)
			}
		})
	}

	// Store absent: cred refs and env fallbacks are unresolvable, literals
	// and defaults still work.
	nr := &Resolver{}
	if v, ok := nr.Resolve(ctx, "cred:STORED_KEY"); ok || v != "" {
		t.Fatalf("cred with nil store must be unresolvable, got %q %v", v, ok)
	}
	if v, ok := nr.Resolve(ctx, "${STORED_KEY}"); ok || v != "" {
		t.Fatalf("env fallback with nil store must be unresolvable, got %q %v", v, ok)
	}
	if v, ok := nr.Resolve(ctx, "${NOPE:-d}"); !ok || v != "d" {
		t.Fatalf("default must survive a nil store, got %q %v", v, ok)
	}
	if v, ok := nr.Resolve(ctx, "lit"); !ok || v != "lit" {
		t.Fatalf("literal must survive a nil store, got %q %v", v, ok)
	}
}

// TestLoadDirSecretFieldsDeferred: on the configdir path, registry secret
// fields keep their raw reference while every other field expands at load —
// exactly the legacy expansion semantics for non-secrets.
func TestLoadDirSecretFieldsDeferred(t *testing.T) {
	t.Setenv("EXPAND_ME", "expanded")
	dir := writeDir(t, map[string]string{
		"system.yaml": `
version: "1"
models:
  default:
    provider: openai
    model: gpt-4o-mini
    base_url: http://127.0.0.1:11434/v1
    api_key: ${MODEL_KEY}
search:
  engines:
    youtube: { api_key: "${SEARCH_KEY}" }
media:
  backend: s3
  s3: { bucket: b, region: r, access_key: "${S3_AK}", secret_key: literal-secret }
profiles:
  shell:
    box: { provider: ssh, host: h, user: u, password: "${BOX_PW}" }
gateway:
  listen: ":0"
`,
		"channels.yaml": `
channels:
  - { name: tg, type: telegram, agent: bot, token: "${TG_TOKEN}" }
  - { name: wh, type: webhook, agent: bot, path: /h/ }
`,
		"profiles/bot.yaml": `
name: bot
loop: builtin:per_chat
`,
		"triggers/t.yaml": `
triggers:
  - { name: tick, every: 5m, target: { profile: bot }, payload: { region: "${EXPAND_ME}" } }
`,
	})
	cfg, err := LoadDir(dir, discardLog())
	if err != nil {
		t.Fatal(err)
	}

	// Secret registry fields: raw, unresolved.
	if got := cfg.Models["default"].APIKey; got != "${MODEL_KEY}" {
		t.Fatalf("model api_key must stay raw, got %q", got)
	}
	if got := cfg.Search.Engines["youtube"].APIKey; got != "${SEARCH_KEY}" {
		t.Fatalf("search api_key must stay raw, got %q", got)
	}
	if got := cfg.Media.S3.AccessKey; got != "${S3_AK}" {
		t.Fatalf("s3 access_key must stay raw, got %q", got)
	}
	if got := cfg.Media.S3.SecretKey; got != "literal-secret" {
		t.Fatalf("literal s3 secret_key must pass through, got %q", got)
	}
	if got := cfg.Profiles.Shell["box"].Password; got != "${BOX_PW}" {
		t.Fatalf("shell password must stay raw, got %q", got)
	}
	if got := cfg.Gateway.Channels[0].Token; got != "${TG_TOKEN}" {
		t.Fatalf("channel token must stay raw, got %q", got)
	}

	// Non-secret fields: expanded at load, as today.
	if got := cfg.Triggers[0].Payload["region"]; got != "expanded" {
		t.Fatalf("non-secret ${VAR} must expand at load, got %v", got)
	}

	// Boot succeeds with every secret unresolvable: components degrade at
	// construction, validation never requires the secret material.
}

// TestExpandEnvStringMatchesByteExpansion: the structured non-secret
// expansion is byte-identical to the legacy whole-document expansion.
func TestExpandEnvStringMatchesByteExpansion(t *testing.T) {
	t.Setenv("A", "alpha")
	t.Setenv("EMPTYVAR", "")
	in := `x=${A} y=${MISSING:-dflt} z=${MISSING} e=${EMPTYVAR} tail=${A}end`
	want := string(expandEnv([]byte(in)))
	got := expandEnvString(in)
	if got != want {
		t.Fatalf("structured expansion diverged:\n got %q\nwant %q", got, want)
	}
}

// TestLegacyLoadStillByteExpands: the single-file path is unchanged — the
// whole document expands at load, so a secret field holds the expanded value
// (or "" when missing), never a raw reference.
func TestLegacyLoadStillByteExpands(t *testing.T) {
	t.Setenv("LEGACY_KEY", "legacy-value")
	dir := t.TempDir()
	p := filepath.Join(dir, "agentflow.yaml")
	if err := os.WriteFile(p, []byte(`
version: "1"
models:
  default: { provider: openai, model: m, api_key: ${LEGACY_KEY}, base_url: ${MISSING_BASE:-http://localhost/v1} }
agents:
  bot: { loop: builtin:per_chat, model: default }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Models["default"].APIKey; got != "legacy-value" {
		t.Fatalf("legacy api_key must be expanded at load, got %q", got)
	}
	if got := cfg.Models["default"].BaseURL; got != "http://localhost/v1" {
		t.Fatalf("legacy default expansion changed, got %q", got)
	}
	if !strings.Contains(string(expandEnv([]byte("${A:-b}"))), "b") {
		t.Fatal("sanity: expandEnv default semantics")
	}
}
