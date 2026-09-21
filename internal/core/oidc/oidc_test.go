package oidc

import (
	"context"
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testIssuer = "https://auth.example.com"

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

// jwksEntry renders one RSA signing key as a JWKS entry.
func jwksEntry(kid string, pub *rsa.PublicKey) string {
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	return fmt.Sprintf(`{"kty":"RSA","kid":%q,"use":"sig","alg":"RS256","n":%q,"e":%q}`, kid, n, e)
}

// jwksFor renders a one-key JWKS document for a public key.
func jwksFor(kid string, pub *rsa.PublicKey) string {
	return `{"keys":[` + jwksEntry(kid, pub) + `]}`
}

// jwksServer serves whatever the current document is, so a test can rotate keys
// under a running verifier.
func jwksServer(t *testing.T, doc *atomic.Value) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, doc.Load().(string))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func mint(t *testing.T, key *rsa.PrivateKey, kid, iss, aud string, exp time.Time) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub":   "user-1",
		"email": "oscar@example.com",
		"name":  "Oscar",
		"iss":   iss,
		"iat":   time.Now().Unix(),
		"exp":   exp.Unix(),
	}
	if aud != "" {
		claims["aud"] = aud
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

// newVerifier builds a verifier against a local JWKS endpoint.
func newVerifier(t *testing.T, cfg Config, doc *atomic.Value) *Verifier {
	t.Helper()
	srv := jwksServer(t, doc)
	cfg.JWKSURL = srv.URL
	v, err := New(cfg, discard())
	if err != nil {
		t.Fatalf("build verifier: %v", err)
	}
	return v
}

func TestVerifyAcceptsAWellFormedToken(t *testing.T) {
	key := testKey(t)
	var doc atomic.Value
	doc.Store(jwksFor("k1", &key.PublicKey))
	v := newVerifier(t, Config{Issuer: testIssuer, Audience: "agentflow"}, &doc)

	claims, err := v.Verify(context.Background(), mint(t, key, "k1", testIssuer, "agentflow", time.Now().Add(time.Hour)))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Subject != "user-1" || claims.Email != "oscar@example.com" || claims.Name != "Oscar" {
		t.Fatalf("claims: %+v", claims)
	}
	if claims.Expiry.IsZero() {
		t.Error("expiry should be carried")
	}
	// The identity key is issuer-scoped, so two issuers cannot collide on a
	// subject string.
	if got := claims.Identity(testIssuer); got != testIssuer+"|user-1" {
		t.Errorf("identity key: %q", got)
	}
}

func TestVerifyRejectsBadTokens(t *testing.T) {
	key := testKey(t)
	other := testKey(t)
	var doc atomic.Value
	doc.Store(jwksFor("k1", &key.PublicKey))
	v := newVerifier(t, Config{Issuer: testIssuer, Audience: "agentflow"}, &doc)
	ctx := context.Background()

	cases := []struct {
		name  string
		token string
	}{
		{"wrong issuer", mint(t, key, "k1", "https://elsewhere.example", "agentflow", time.Now().Add(time.Hour))},
		{"wrong audience", mint(t, key, "k1", testIssuer, "some-other-service", time.Now().Add(time.Hour))},
		{"expired", mint(t, key, "k1", testIssuer, "agentflow", time.Now().Add(-2*time.Hour))},
		{"signed by a key the issuer does not publish", mint(t, other, "k1", testIssuer, "agentflow", time.Now().Add(time.Hour))},
		{"unknown kid", mint(t, key, "k9", testIssuer, "agentflow", time.Now().Add(time.Hour))},
		{"not a token at all", "afu_notajwt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := v.Verify(ctx, tc.token); err == nil {
				t.Fatalf("token must not verify: %s", tc.name)
			}
		})
	}
}

// The classic confusion attack: sign an HS256 token with the RSA *public* key,
// hoping the verifier will treat it as a shared secret. Only asymmetric
// algorithms are accepted, so it must fail.
func TestVerifyRejectsSymmetricAlgorithm(t *testing.T) {
	key := testKey(t)
	var doc atomic.Value
	doc.Store(jwksFor("k1", &key.PublicKey))
	v := newVerifier(t, Config{Issuer: testIssuer}, &doc)

	claims := jwt.MapClaims{"sub": "attacker", "iss": testIssuer, "exp": time.Now().Add(time.Hour).Unix()}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tok.Header["kid"] = "k1"
	secret := base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes())
	signed, err := tok.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := v.Verify(context.Background(), signed); err == nil {
		t.Fatal("an HS256 token signed with the public key must be refused")
	}
	// And "alg: none" too.
	unsigned := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	signedNone, err := unsigned.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none: %v", err)
	}
	if _, err := v.Verify(context.Background(), signedNone); err == nil {
		t.Fatal("an unsigned token must be refused")
	}
}

// A rotation must be picked up without a restart: the unknown key triggers a
// refresh, and the new key then verifies.
func TestVerifyFollowsKeyRotation(t *testing.T) {
	oldKey := testKey(t)
	newKey := testKey(t)
	var doc atomic.Value
	doc.Store(jwksFor("old", &oldKey.PublicKey))
	v := newVerifier(t, Config{Issuer: testIssuer}, &doc)

	if _, err := v.Verify(context.Background(), mint(t, oldKey, "old", testIssuer, "", time.Now().Add(time.Hour))); err != nil {
		t.Fatalf("token with the original key: %v", err)
	}

	// The issuer rotates: new key published, old one withdrawn.
	doc.Store(jwksFor("new", &newKey.PublicKey))
	if _, err := v.Verify(context.Background(), mint(t, newKey, "new", testIssuer, "", time.Now().Add(time.Hour))); err != nil {
		t.Fatalf("a token signed by the rotated key should verify after a refresh: %v", err)
	}
}

// Discovery: with no jwks_url configured, the issuer's OpenID configuration
// supplies it.
func TestDiscoveryResolvesJWKS(t *testing.T) {
	key := testKey(t)
	var doc atomic.Value
	doc.Store(jwksFor("k1", &key.PublicKey))
	jwks := jwksServer(t, &doc)

	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": r.Host, "jwks_uri": jwks.URL})
	}))
	defer issuer.Close()

	v, err := New(Config{Issuer: issuer.URL}, discard())
	if err != nil {
		t.Fatalf("build with discovery: %v", err)
	}
	if _, err := v.Verify(context.Background(), mint(t, key, "k1", issuer.URL, "", time.Now().Add(time.Hour))); err != nil {
		t.Fatalf("verify after discovery: %v", err)
	}
}

func TestIssuerRequired(t *testing.T) {
	if _, err := New(Config{}, discard()); err == nil {
		t.Fatal("an issuer is required")
	}
}

// An unreachable issuer fails at construction: a misconfigured deployment
// should not boot into a state where every login fails.
func TestUnreachableIssuerFailsAtBoot(t *testing.T) {
	if _, err := New(Config{Issuer: "https://127.0.0.1:1", JWKSURL: "https://127.0.0.1:1/jwks"}, discard()); err == nil {
		t.Fatal("an unreachable issuer must fail construction")
	}
}

// A JWKS carrying key types this build cannot verify is not fatal as long as a
// usable key is present; a set with nothing usable is.
func TestJWKSKeySelection(t *testing.T) {
	key := testKey(t)
	mixed := `{"keys":[{"kty":"OKP","kid":"ed1","use":"sig","crv":"Ed25519","x":"AAAA"},` +
		jwksEntry("rsa1", &key.PublicKey) + `]}`
	var doc atomic.Value
	doc.Store(mixed)
	v := newVerifier(t, Config{Issuer: testIssuer}, &doc)
	if _, err := v.Verify(context.Background(), mint(t, key, "rsa1", testIssuer, "", time.Now().Add(time.Hour))); err != nil {
		t.Fatalf("the RSA key beside an unusable one should still verify: %v", err)
	}

	// Encryption keys are skipped, and a set with no signing key fails loudly.
	var enc atomic.Value
	enc.Store(`{"keys":[{"kty":"RSA","kid":"k1","use":"enc","n":"AQAB","e":"AQAB"}]}`)
	srv := jwksServer(t, &enc)
	if _, err := New(Config{Issuer: testIssuer, JWKSURL: srv.URL}, discard()); err == nil {
		t.Fatal("a JWKS with no usable signing key must fail construction")
	}
}
