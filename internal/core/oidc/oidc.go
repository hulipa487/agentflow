// Package oidc verifies access tokens minted by an external identity provider
// (Better Auth, Keycloak, Auth0, …).
//
// The engine is a resource server, not an identity provider: it does not log
// anyone in and it never sees a password. It checks that a token presented to
// it was signed by a key the configured issuer publishes, that the issuer and
// audience are the ones this deployment expects, and that the token is still
// valid — then hands back the subject claim as the identity.
//
// Signing keys come from the issuer's JWKS. They are fetched at construction,
// refreshed when a token names a key we do not have (rotation), and re-fetched
// after a maximum age so a key retired by the issuer stops being honoured.
//
// Only asymmetric algorithms are accepted. That is not a detail: allowing a
// symmetric algorithm here would let anyone who learns a *public* key mint
// tokens with it, which is the classic JWT confusion attack.
package oidc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Config describes the issuer this engine will trust.
type Config struct {
	// Issuer is the expected `iss` claim, and the base for discovery when
	// JWKSURL is empty.
	Issuer string
	// Audience, when set, must appear in the token's `aud` claim. Setting it is
	// what stops a token minted for a different service from being replayed
	// here.
	Audience string
	// JWKSURL overrides discovery. Leave empty to use
	// <issuer>/.well-known/openid-configuration.
	JWKSURL string
	// Claim names carrying the profile fields. Empty means the standard
	// sub/email/name.
	SubjectClaim string
	EmailClaim   string
	NameClaim    string
}

// Claims is what the engine takes from a verified token.
type Claims struct {
	Subject string
	Email   string
	Name    string
	Expiry  time.Time
}

// Issuer is the configured issuer — the value every identity key is scoped by.
func (v *Verifier) Issuer() string { return v.cfg.Issuer }

// Identity is the stable key a profile binds to: the issuer plus the subject,
// so two issuers can never collide on the same subject string.
func (c Claims) Identity(issuer string) string {
	return issuer + "|" + c.Subject
}

// Verifier checks tokens against one issuer's published keys.
type Verifier struct {
	cfg    Config
	log    *slog.Logger
	client *http.Client

	mu        sync.RWMutex
	keys      map[string]any // kid → *rsa.PublicKey | *ecdsa.PublicKey
	jwksURL   string
	fetchedAt time.Time
}

// maxKeyAge bounds how long a signing key is trusted without re-checking the
// issuer's JWKS, so a key the issuer retires ages out instead of being honoured
// forever.
const maxKeyAge = time.Hour

// New builds a verifier and performs the first key fetch. A deployment whose
// issuer is unreachable at boot fails here rather than at the first login.
func New(cfg Config, log *slog.Logger) (*Verifier, error) {
	if cfg.Issuer == "" {
		return nil, fmt.Errorf("oidc: issuer is required")
	}
	if cfg.SubjectClaim == "" {
		cfg.SubjectClaim = "sub"
	}
	if cfg.EmailClaim == "" {
		cfg.EmailClaim = "email"
	}
	if cfg.NameClaim == "" {
		cfg.NameClaim = "name"
	}
	v := &Verifier{
		cfg:     cfg,
		log:     log.With("module", "oidc"),
		client:  &http.Client{Timeout: 10 * time.Second},
		keys:    map[string]any{},
		jwksURL: cfg.JWKSURL, // empty means discover from the issuer
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := v.refresh(ctx); err != nil {
		return nil, err
	}
	return v, nil
}

// Verify checks a token's signature, issuer, audience and lifetime, and returns
// the claims it carries.
func (v *Verifier) Verify(ctx context.Context, token string) (Claims, error) {
	opts := []jwt.ParserOption{
		// Explicitly asymmetric: see the package comment.
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "PS256", "PS384", "PS512"}),
		jwt.WithIssuer(v.cfg.Issuer),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30 * time.Second), // tolerate small clock skew
	}
	if v.cfg.Audience != "" {
		opts = append(opts, jwt.WithAudience(v.cfg.Audience))
	}

	parsed, err := jwt.Parse(token, v.keyFunc(ctx), opts...)
	if err != nil {
		// One retry after a key refresh: a rotation between our last fetch and
		// this request is the common case, and it should not look like a bad
		// token.
		if refreshErr := v.refresh(ctx); refreshErr != nil {
			v.log.Warn("jwks refresh failed", "err", refreshErr)
		}
		parsed, err = jwt.Parse(token, v.keyFunc(ctx), opts...)
		if err != nil {
			return Claims{}, fmt.Errorf("oidc: %w", err)
		}
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return Claims{}, fmt.Errorf("oidc: unexpected claim type")
	}
	out := Claims{
		Subject: stringClaim(claims, v.cfg.SubjectClaim),
		Email:   stringClaim(claims, v.cfg.EmailClaim),
		Name:    stringClaim(claims, v.cfg.NameClaim),
	}
	if out.Subject == "" {
		return Claims{}, fmt.Errorf("oidc: token carries no %q claim", v.cfg.SubjectClaim)
	}
	if exp, err := claims.GetExpirationTime(); err == nil && exp != nil {
		out.Expiry = exp.Time
	}
	return out, nil
}

// keyFunc resolves the signing key for a token, refreshing the key set when the
// token names a key we have not seen.
func (v *Verifier) keyFunc(ctx context.Context) jwt.Keyfunc {
	return func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if key, ok := v.key(kid); ok {
			return key, nil
		}
		if err := v.refresh(ctx); err != nil {
			return nil, fmt.Errorf("oidc: refresh keys: %w", err)
		}
		if key, ok := v.key(kid); ok {
			return key, nil
		}
		return nil, fmt.Errorf("oidc: no signing key for kid %q", kid)
	}
}

func (v *Verifier) key(kid string) (any, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if time.Since(v.fetchedAt) > maxKeyAge {
		return nil, false
	}
	// A provider that publishes exactly one key may omit the kid entirely.
	if kid == "" && len(v.keys) == 1 {
		for _, k := range v.keys {
			return k, true
		}
	}
	k, ok := v.keys[kid]
	return k, ok
}

// refresh resolves the JWKS location (once) and reloads the key set.
func (v *Verifier) refresh(ctx context.Context) error {
	v.mu.RLock()
	url := v.jwksURL
	v.mu.RUnlock()
	if url == "" {
		discovered, err := v.discover(ctx)
		if err != nil {
			return err
		}
		url = discovered
		v.mu.Lock()
		v.jwksURL = url
		v.mu.Unlock()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("oidc: build jwks request: %w", err)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("oidc: fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oidc: jwks status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("oidc: read jwks: %w", err)
	}
	keys, err := parseJWKS(body)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return fmt.Errorf("oidc: jwks published no usable keys")
	}
	v.mu.Lock()
	v.keys = keys
	v.fetchedAt = time.Now()
	v.mu.Unlock()
	v.log.Debug("jwks refreshed", "url", url, "keys", len(keys))
	return nil
}

// discover reads the issuer's OpenID configuration for its jwks_uri.
func (v *Verifier) discover(ctx context.Context) (string, error) {
	url := strings.TrimSuffix(v.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("oidc: build discovery request: %w", err)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("oidc: discovery failed (set jwks_url to skip it): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oidc: discovery status %d (set jwks_url to skip it)", resp.StatusCode)
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
		Issuer  string `json:"issuer"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return "", fmt.Errorf("oidc: decode discovery: %w", err)
	}
	if doc.JWKSURI == "" {
		return "", fmt.Errorf("oidc: issuer publishes no jwks_uri (set jwks_url)")
	}
	return doc.JWKSURI, nil
}

// --- JWKS -------------------------------------------------------------------

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// parseJWKS turns a JWKS document into public keys. Key types this build cannot
// verify (or keys marked for encryption) are skipped rather than failing the
// whole set: an issuer may publish more than one kind.
func parseJWKS(body []byte) (map[string]any, error) {
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("oidc: decode jwks: %w", err)
	}
	out := make(map[string]any, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		key, err := k.publicKey()
		if err != nil {
			continue
		}
		out[k.Kid] = key
	}
	return out, nil
}

func (k jwk) publicKey() (any, error) {
	switch k.Kty {
	case "RSA":
		n, err := b64ToBigInt(k.N)
		if err != nil {
			return nil, err
		}
		e, err := b64ToBigInt(k.E)
		if err != nil {
			return nil, err
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, fmt.Errorf("unsupported curve %q", k.Crv)
		}
		x, err := b64ToBigInt(k.X)
		if err != nil {
			return nil, err
		}
		y, err := b64ToBigInt(k.Y)
		if err != nil {
			return nil, err
		}
		return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
	default:
		return nil, fmt.Errorf("unsupported key type %q", k.Kty)
	}
}

func b64ToBigInt(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(b), nil
}

func stringClaim(m jwt.MapClaims, name string) string {
	switch v := m[name].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}
