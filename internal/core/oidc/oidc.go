// Package oidc verifies access tokens minted by an external identity provider
// (Better Auth, Keycloak, Auth0, …).
//
// The engine is a resource server, not an identity provider: it does not log
// anyone in and it never sees a password. It checks that a token presented to
// it was signed by a key the configured issuer publishes, that the issuer and
// audience are the ones this deployment expects, and that the token is still
// valid — then hands back the subject claim as the identity.
//
// Signing keys come from the issuer's JWKS, fetched through the go-oidc remote
// key set. Only asymmetric algorithms are accepted. That is not a detail:
// allowing a symmetric algorithm here would let anyone who learns a *public*
// key mint tokens with it, which is the classic JWT confusion attack.
package oidc

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
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
	cfg      Config
	log      *slog.Logger
	verifier *oidc.IDTokenVerifier
}

// supportedSigningAlgs limits verification to asymmetric algorithms. Allowing
// a symmetric algorithm would let anyone with the public key mint tokens.
var supportedSigningAlgs = []string{
	"RS256", "RS384", "RS512",
	"ES256", "ES384", "ES512",
	"PS256", "PS384", "PS512",
}

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

	client := &http.Client{Timeout: 10 * time.Second}
	oidcCfg := &oidc.Config{
		ClientID:             cfg.Audience,
		SkipClientIDCheck:    cfg.Audience == "",
		SupportedSigningAlgs: supportedSigningAlgs,
		// go-oidc has no leeway field; shift the clock back to tolerate small
		// clock skew.
		Now: func() time.Time { return time.Now().Add(-30 * time.Second) },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ctx = oidc.ClientContext(ctx, client)

	var verifier *oidc.IDTokenVerifier
	if cfg.JWKSURL == "" {
		provider, err := oidc.NewProvider(ctx, cfg.Issuer)
		if err != nil {
			return nil, fmt.Errorf("oidc: %w", err)
		}
		// VerifierContext carries the custom HTTP client to the remote key set.
		verifier = provider.VerifierContext(ctx, oidcCfg)
	} else {
		// Preserve eager-fail semantics when the JWKS location is configured
		// directly: a misconfigured deployment should not boot.
		if err := pingJWKS(ctx, client, cfg.JWKSURL); err != nil {
			return nil, err
		}
		keySet := oidc.NewRemoteKeySet(ctx, cfg.JWKSURL)
		verifier = oidc.NewVerifier(cfg.Issuer, keySet, oidcCfg)
	}

	return &Verifier{
		cfg:      cfg,
		log:      log.With("module", "oidc"),
		verifier: verifier,
	}, nil
}

// pingJWKS performs a lightweight reachability check on a configured JWKS
// endpoint so that boot fails if the endpoint is unreachable.
func pingJWKS(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("oidc: build jwks request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("oidc: fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oidc: jwks status %d", resp.StatusCode)
	}
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)); err != nil {
		return fmt.Errorf("oidc: read jwks: %w", err)
	}
	return nil
}

// Verify checks a token's signature, issuer, audience and lifetime, and returns
// the claims it carries.
func (v *Verifier) Verify(ctx context.Context, token string) (Claims, error) {
	idToken, err := v.verifier.Verify(ctx, token)
	if err != nil {
		return Claims{}, fmt.Errorf("oidc: %w", err)
	}

	var raw map[string]any
	if err := idToken.Claims(&raw); err != nil {
		return Claims{}, fmt.Errorf("oidc: decode claims: %w", err)
	}

	out := Claims{
		Subject: stringClaim(raw, v.cfg.SubjectClaim),
		Email:   stringClaim(raw, v.cfg.EmailClaim),
		Name:    stringClaim(raw, v.cfg.NameClaim),
		Expiry:  idToken.Expiry,
	}
	if out.Subject == "" {
		return Claims{}, fmt.Errorf("oidc: token carries no %q claim", v.cfg.SubjectClaim)
	}
	return out, nil
}

func stringClaim(m map[string]any, name string) string {
	switch v := m[name].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}
