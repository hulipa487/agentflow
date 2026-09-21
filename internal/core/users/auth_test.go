package users

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/files"
	"agentflow/internal/core/identity"
	"agentflow/internal/core/media"
	"agentflow/internal/core/oidc"
	"agentflow/internal/core/runtime"

	"github.com/golang-jwt/jwt/v5"
)

// --- helpers ----------------------------------------------------------------

// call sends a request with an optional bearer token.
func call(t *testing.T, method, url, token string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

// jwksEntry renders one RSA signing key as a JWKS entry.
func jwksEntry(kid string, pub *rsa.PublicKey) string {
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	return fmt.Sprintf(`{"kty":"RSA","kid":%q,"use":"sig","alg":"RS256","n":%q,"e":%q}`, kid, n, e)
}

// newAPIWith builds the API over a real identity store plus whatever optional
// handles a test needs.
func newAPIWith(t *testing.T, cfg config.UsersConfig, opts Options) (*httptest.Server, *identity.Registry) {
	t.Helper()
	reg, err := identity.Open(filepath.Join(t.TempDir(), "identity.db"), discardLogger())
	if err != nil {
		t.Fatalf("open identity: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	srv := httptest.NewServer(New(reg, cfg, discardLogger(), opts).Handler())
	t.Cleanup(srv.Close)
	return srv, reg
}

// provider stands in for the deployment's identity provider: a JWKS endpoint
// plus the ability to mint tokens for it.
type provider struct {
	key    *rsa.PrivateKey
	srv    *httptest.Server
	issuer string
}

func newProvider(t *testing.T) *provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	doc := `{"keys":[` + jwksEntry("k1", &key.PublicKey) + `]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, doc)
	}))
	t.Cleanup(srv.Close)
	return &provider{key: key, srv: srv, issuer: "https://auth.example.com"}
}

func (p *provider) verifier(t *testing.T, audience string) *oidc.Verifier {
	t.Helper()
	v, err := oidc.New(oidc.Config{Issuer: p.issuer, Audience: audience, JWKSURL: p.srv.URL}, discardLogger())
	if err != nil {
		t.Fatalf("build verifier: %v", err)
	}
	return v
}

func (p *provider) token(t *testing.T, sub, email, name, audience string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub": sub, "email": email, "name": name,
		"iss": p.issuer, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	}
	if audience != "" {
		claims["aud"] = audience
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(p.key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

// --- authentication ---------------------------------------------------------

// A per-profile API token still works, and is the only way in when no identity
// provider is configured.
func TestAPITokenAuth(t *testing.T) {
	srv, _ := newAPI(t, config.UsersConfig{Enabled: true})
	_, token := register(t, srv, nil)

	status, me := call(t, "GET", srv.URL+Prefix+"/me", token, nil)
	if status != http.StatusOK || me["user_id"] == nil {
		t.Fatalf("api token auth: %d %v", status, me)
	}
	if status, _ := call(t, "GET", srv.URL+Prefix+"/me", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("no token: %d", status)
	}
	if status, out := call(t, "GET", srv.URL+Prefix+"/me", "not-a-token", nil); status != http.StatusUnauthorized {
		t.Fatalf("unrecognized token shape: %d %v", status, out)
	}
	// An access token presented to a deployment with no provider is refused
	// with a reason, not treated as an API token.
	status, out := call(t, "GET", srv.URL+Prefix+"/me", "eyJhbGciOiJSUzI1NiJ9.e30.x", nil)
	if status != http.StatusUnauthorized || !strings.Contains(errString(out), "no identity provider") {
		t.Fatalf("jwt without a provider: %d %v", status, out)
	}
}

// The whole point: a verified identity-provider token resolves to a profile,
// and the first login creates one.
func TestProviderTokenProvisionsAndAuthenticates(t *testing.T) {
	p := newProvider(t)
	srv, reg := newAPIWith(t, config.UsersConfig{Enabled: true},
		Options{Verifier: p.verifier(t, "agentflow"), JITProvisioning: true})

	token := p.token(t, "user-42", "oscar@example.com", "Oscar", "agentflow")
	status, me := call(t, "GET", srv.URL+Prefix+"/me", token, nil)
	if status != http.StatusOK {
		t.Fatalf("provider login: %d %v", status, me)
	}
	userID, _ := me["user_id"].(string)
	if userID == "" {
		t.Fatalf("no profile returned: %v", me)
	}
	if me["display_name"] != "Oscar" || me["email"] != "oscar@example.com" {
		t.Fatalf("claims should populate the profile: %v", me)
	}
	// The login is a verified identity on the pseudo-channel, not a channel
	// handle: nothing to deliver to, nothing to link.
	ids, _ := me["identities"].([]any)
	if len(ids) != 1 {
		t.Fatalf("expected the login identity: %v", me["identities"])
	}
	first, _ := ids[0].(map[string]any)
	if first["channel"] != "oidc" || first["trust"] != "verified" {
		t.Fatalf("login identity: %v", first)
	}
	if first["deliverable"] == true || first["linkable"] == true {
		t.Fatalf("a login is neither deliverable nor linkable: %v", first)
	}
	// The subject is issuer-scoped, so the same sub on another issuer would be
	// a different person.
	if nf, _ := first["native_from"].(string); !strings.HasPrefix(nf, p.issuer+"|") {
		t.Fatalf("identity key should be issuer-scoped: %v", nf)
	}

	// A second login reuses the profile rather than provisioning another.
	status, again := call(t, "GET", srv.URL+Prefix+"/me", token, nil)
	if status != http.StatusOK || again["user_id"] != userID {
		t.Fatalf("second login should reuse the profile: %d %v", status, again)
	}
	if ps, _ := reg.List(); len(ps) != 1 {
		t.Fatalf("expected exactly one profile, got %d", len(ps))
	}

	// A token the issuer did not sign is refused.
	if status, _ := call(t, "GET", srv.URL+Prefix+"/me", "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.bad", nil); status != http.StatusUnauthorized {
		t.Fatalf("forged token: %d", status)
	}
}

func TestProviderTokenRespectsAudience(t *testing.T) {
	p := newProvider(t)
	srv, _ := newAPIWith(t, config.UsersConfig{Enabled: true},
		Options{Verifier: p.verifier(t, "agentflow"), JITProvisioning: true})

	// A token minted for another service must not be replayable here.
	other := p.token(t, "user-1", "", "", "some-other-service")
	if status, _ := call(t, "GET", srv.URL+Prefix+"/me", other, nil); status != http.StatusUnauthorized {
		t.Fatalf("a token for another audience must be refused: %d", status)
	}
}

// With JIT off, an unknown subject is refused and told why — the deployment
// wants profiles provisioned before anyone signs in.
func TestProviderTokenWithoutJITIsRefused(t *testing.T) {
	p := newProvider(t)
	srv, reg := newAPIWith(t, config.UsersConfig{Enabled: true},
		Options{Verifier: p.verifier(t, "agentflow"), JITProvisioning: false})

	token := p.token(t, "user-99", "", "", "agentflow")
	status, out := call(t, "GET", srv.URL+Prefix+"/me", token, nil)
	if status != http.StatusForbidden {
		t.Fatalf("unknown subject with jit off: %d %v", status, out)
	}
	if !strings.Contains(errString(out), "provisioned") {
		t.Fatalf("the refusal should say what is missing: %v", out)
	}
	if ps, _ := reg.List(); len(ps) != 0 {
		t.Fatalf("nothing may be provisioned: %d profiles", len(ps))
	}
}

// A provider login can do everything a token-authenticated caller can: the
// authentication method is not a second-class citizen.
func TestProviderLoginCanLinkAndRotate(t *testing.T) {
	p := newProvider(t)
	srv, reg := newAPIWith(t, config.UsersConfig{Enabled: true},
		Options{Verifier: p.verifier(t, "agentflow"), JITProvisioning: true,
			LinkableChannels: []string{"tg-main"}})
	// Boot installs the deployment's per-channel traits; a named channel is not
	// in the type-keyed defaults, so linking depends on this.
	if err := reg.SetChannelTraits(map[string]identity.ChannelTraits{
		"tg-main": identity.Traits("telegram"),
	}); err != nil {
		t.Fatalf("set channel traits: %v", err)
	}
	token := p.token(t, "user-7", "oscar@example.com", "Oscar", "agentflow")

	status, patch := call(t, "PATCH", srv.URL+Prefix+"/me", token, map[string]string{"display_name": "Renamed"})
	if status != http.StatusOK || patch["display_name"] != "Renamed" {
		t.Fatalf("provider login should write: %d %v", status, patch)
	}
	status, link := call(t, "POST", srv.URL+Prefix+"/me/links", token, map[string]string{"channel": "tg-main"})
	if status != http.StatusCreated {
		t.Fatalf("provider login should start a link: %d %v", status, link)
	}
	if link["code"] == nil {
		t.Fatalf("no challenge code: %v", link)
	}
	// Token rotation is about API tokens; a provider login has none to rotate,
	// so it gets a fresh one for scripts.
	status, rot := call(t, "POST", srv.URL+Prefix+"/me/token/rotate", token, nil)
	if status != http.StatusOK || rot["token"] == nil {
		t.Fatalf("rotate under a provider login: %d %v", status, rot)
	}
}

// --- CORS and config --------------------------------------------------------

func TestCORSAllowList(t *testing.T) {
	const allowed = "https://app.example.com"
	srv, _ := newAPI(t, config.UsersConfig{Enabled: true, CORSOrigins: []string{allowed}})

	get := func(origin string) *http.Response {
		t.Helper()
		req, err := http.NewRequest("GET", srv.URL+Prefix+"/config", nil)
		if err != nil {
			t.Fatal(err)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	resp := get(allowed)
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != allowed {
		t.Fatalf("allow-origin = %q, want %q", got, allowed)
	}
	// No ambient credentials travel with these requests, so the engine must not
	// invite the browser to send any.
	if resp.Header.Get("Access-Control-Allow-Credentials") != "" {
		t.Error("bearer-only requests must not allow credentials")
	}
	if !strings.Contains(resp.Header.Get("Vary"), "Origin") {
		t.Error("a per-origin answer must vary on Origin")
	}
	if got := get("https://evil.example").Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("an unknown origin must get no CORS answer, got %q", got)
	}

	pre, err := http.NewRequest("OPTIONS", srv.URL+Prefix+"/me/links", nil)
	if err != nil {
		t.Fatal(err)
	}
	pre.Header.Set("Origin", allowed)
	pre.Header.Set("Access-Control-Request-Method", "POST")
	presp, err := http.DefaultClient.Do(pre)
	if err != nil {
		t.Fatal(err)
	}
	presp.Body.Close()
	if presp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight status %d", presp.StatusCode)
	}
	if !strings.Contains(presp.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Errorf("preflight must allow Authorization, got %q", presp.Header.Get("Access-Control-Allow-Headers"))
	}
}

func TestCORSIsOffByDefault(t *testing.T) {
	srv, _ := newAPI(t, config.UsersConfig{Enabled: true})
	req, err := http.NewRequest("GET", srv.URL+Prefix+"/config", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://app.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("no origin may be allowed until configured")
	}
}

func TestConfigEndpoint(t *testing.T) {
	p := newProvider(t)
	srv, _ := newAPIWith(t, config.UsersConfig{Enabled: true, Registration: "invite"},
		Options{LinkableChannels: []string{"tg-main"}, Verifier: p.verifier(t, "agentflow")})
	status, out := call(t, "GET", srv.URL+Prefix+"/config", "", nil)
	if status != http.StatusOK {
		t.Fatalf("config: %d %v", status, out)
	}
	if out["registration"] != "invite" {
		t.Fatalf("registration mode: %v", out)
	}
	chans, _ := out["linkable_channels"].([]any)
	if len(chans) != 1 || chans[0] != "tg-main" {
		t.Fatalf("linkable channels: %v", out["linkable_channels"])
	}
	if out["identity_provider"] != true {
		t.Fatalf("a frontend must be told a provider is in play: %v", out)
	}
	// Without a provider, the flag says so.
	bare, _ := newAPIWith(t, config.UsersConfig{Enabled: true}, Options{})
	if _, out := call(t, "GET", bare.URL+Prefix+"/config", "", nil); out["identity_provider"] != false {
		t.Fatalf("no provider should be reported plainly: %v", out)
	}
}

// --- self-service views -----------------------------------------------------

func TestMeUsageAndProjects(t *testing.T) {
	dir := t.TempDir()
	store, err := runtime.Open(filepath.Join(dir, "runtime.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	blobs, err := media.Open(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("open blobs: %v", err)
	}
	fm := files.New(blobs, store, 0, 1<<20, slog.New(slog.NewTextHandler(io.Discard, nil)))

	srv, _ := newAPIWith(t, config.UsersConfig{Enabled: true}, Options{Store: store, Files: fm})
	userID, token := register(t, srv, nil)

	if err := store.RecordUsage(runtime.UsageRecord{
		UserID: userID, Agent: "bot", Model: "m", Kind: "chat",
		Input: 120, Output: 30, Cached: 64, OK: true, At: time.Now(),
	}, false); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	if _, err := fm.Put(t.Context(), "user:"+userID, "notes", "a.txt", strings.NewReader("x"), "text/plain"); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Somebody else's project stays invisible.
	if _, err := fm.Put(t.Context(), "user:u_someone_else", "secret", "s.txt", strings.NewReader("x"), "text/plain"); err != nil {
		t.Fatalf("put other: %v", err)
	}

	status, usage := call(t, "GET", srv.URL+Prefix+"/me/usage", token, nil)
	if status != http.StatusOK {
		t.Fatalf("usage: %d %v", status, usage)
	}
	today, _ := usage["today"].(map[string]any)
	if today["input"] != float64(120) || today["cached"] != float64(64) {
		t.Fatalf("usage today: %v", usage["today"])
	}
	if _, ok := usage["history"].([]any); !ok {
		t.Fatalf("usage should carry a history window: %v", usage)
	}

	status, projects := call(t, "GET", srv.URL+Prefix+"/me/projects", token, nil)
	if status != http.StatusOK {
		t.Fatalf("projects: %d %v", status, projects)
	}
	names, _ := projects["projects"].([]any)
	if len(names) != 1 || names[0] != "notes" {
		t.Fatalf("projects must be scoped to the caller: %v", projects["projects"])
	}

	// Without the subsystems wired, the endpoints name the gap.
	bare, _ := newAPI(t, config.UsersConfig{Enabled: true})
	_, bareToken := register(t, bare, nil)
	if status, out := call(t, "GET", bare.URL+Prefix+"/me/usage", bareToken, nil); status != http.StatusServiceUnavailable {
		t.Fatalf("usage without a ledger: %d %v", status, out)
	}
	if status, out := call(t, "GET", bare.URL+Prefix+"/me/projects", bareToken, nil); status != http.StatusServiceUnavailable {
		t.Fatalf("projects without a file store: %d %v", status, out)
	}
}
