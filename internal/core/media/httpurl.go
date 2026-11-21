package media

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HTTPPresigner implements Presigner over any media.Store (including local FS)
// by minting short-lived HMAC-SHA256 signed URLs served by its http.Handler.
// This allows URL-only channels (like QQ) to operate without S3 when the
// runtime is reachable at gateway.public_url.
type HTTPPresigner struct {
	store     Store
	publicURL string // e.g. "https://bot.example.com"
	path      string // e.g. "/media/t/" (leading and trailing slash)
	secret    []byte
	maxBytes  int64
	now       func() time.Time
}

var _ Presigner = (*HTTPPresigner)(nil)

const (
	maxTempURLTTL    = 24 * time.Hour
	defaultServeMax  = 50 << 20
	tempTokenVersion = 1
)

type tempTokenPayload struct {
	V      int    `json:"v"`
	Handle string `json:"h"`
	Exp    int64  `json:"e"`
	MIME   string `json:"m,omitempty"`
	Name   string `json:"n,omitempty"`
}

// NewHTTPPresigner builds an HMAC-signed temporary URL publisher over store.
// publicURL must be an http(s) base URL; path must start and end with '/'.
// If secret is empty, an ephemeral 32-byte random key is generated for this
// process (suitable for single-node deployments).
func NewHTTPPresigner(store Store, publicURL, path, secret string) (*HTTPPresigner, error) {
	if store == nil {
		return nil, fmt.Errorf("media temp_url: store is required")
	}
	pub := strings.TrimRight(strings.TrimSpace(publicURL), "/")
	if !strings.HasPrefix(pub, "http://") && !strings.HasPrefix(pub, "https://") {
		return nil, fmt.Errorf("media temp_url: public_url must be an http(s) URL (got %q)", publicURL)
	}
	if path == "" {
		path = "/media/t/"
	}
	if !strings.HasPrefix(path, "/") || !strings.HasSuffix(path, "/") {
		return nil, fmt.Errorf("media temp_url: path must start and end with / (got %q)", path)
	}
	var key []byte
	if secret != "" {
		sum := sha256.Sum256([]byte(secret))
		key = sum[:]
	} else {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("media temp_url: generate signing key: %w", err)
		}
	}
	return &HTTPPresigner{
		store:     store,
		publicURL: pub,
		path:      path,
		secret:    key,
		maxBytes:  defaultServeMax,
		now:       time.Now,
	}, nil
}

// Path returns the route prefix this handler should be mounted on.
func (p *HTTPPresigner) Path() string { return p.path }

// PresignGet mints an HMAC-SHA256 signed URL pointing at <publicURL><path><token>.
func (p *HTTPPresigner) PresignGet(handle string, ttl time.Duration, opts PresignOptions) (string, error) {
	if !ValidHandle(handle) {
		return "", fmt.Errorf("media temp_url: malformed handle %q", handle)
	}
	if ttl <= 0 || ttl > maxTempURLTTL {
		return "", fmt.Errorf("media temp_url: ttl %s must be between 1s and %s", ttl, maxTempURLTTL)
	}
	payload := tempTokenPayload{
		V:      tempTokenVersion,
		Handle: handle,
		Exp:    p.now().Add(ttl).Unix(),
		MIME:   strings.TrimSpace(opts.ContentType),
		Name:   strings.TrimSpace(opts.Filename),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding.EncodeToString(raw)
	mac := p.sign(enc)
	token := enc + "." + base64.RawURLEncoding.EncodeToString(mac)
	return p.publicURL + p.path + token, nil
}

// ServeHTTP verifies the HMAC token and streams the blob. Only GET and HEAD
// are accepted; expired, malformed, or tampered tokens return 403/404 without
// leaking details.
func (p *HTTPPresigner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, p.path)
	if token == "" || strings.Contains(token, "/") {
		http.NotFound(w, r)
		return
	}
	payload, err := p.verify(token)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	b, err := p.store.ReadAll(payload.Handle, p.maxBytes)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := payload.MIME
	if ct == "" {
		ct = SniffMIME(b, payload.Name)
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if payload.Name != "" {
		if cd := safeContentDisposition(payload.Name); cd != "" {
			w.Header().Set("Content-Disposition", cd)
		}
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(b)
	}
}

func (p *HTTPPresigner) verify(token string) (tempTokenPayload, error) {
	enc, sigEnc, ok := strings.Cut(token, ".")
	if !ok || enc == "" || sigEnc == "" {
		return tempTokenPayload{}, fmt.Errorf("malformed token")
	}
	gotSig, err := base64.RawURLEncoding.DecodeString(sigEnc)
	if err != nil {
		return tempTokenPayload{}, fmt.Errorf("bad signature encoding")
	}
	wantSig := p.sign(enc)
	if !hmac.Equal(gotSig, wantSig) {
		return tempTokenPayload{}, fmt.Errorf("signature mismatch")
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return tempTokenPayload{}, fmt.Errorf("bad payload encoding")
	}
	var payload tempTokenPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return tempTokenPayload{}, fmt.Errorf("bad payload json")
	}
	if payload.V != tempTokenVersion || !ValidHandle(payload.Handle) {
		return tempTokenPayload{}, fmt.Errorf("invalid payload")
	}
	if p.now().Unix() > payload.Exp {
		return tempTokenPayload{}, fmt.Errorf("expired")
	}
	return payload, nil
}

func (p *HTTPPresigner) sign(enc string) []byte {
	h := hmac.New(sha256.New, p.secret)
	h.Write([]byte("af-media-temp-v1\n"))
	h.Write([]byte(enc))
	return h.Sum(nil)
}

func safeContentDisposition(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var clean, ascii strings.Builder
	nonASCII := false
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			continue
		}
		clean.WriteRune(r)
		if r < 0x80 {
			ascii.WriteRune(r)
		} else {
			nonASCII = true
			ascii.WriteByte('_')
		}
	}
	utf8Name := strings.TrimSpace(clean.String())
	if utf8Name == "" {
		return ""
	}
	if !nonASCII {
		return `attachment; filename="` + utf8Name + `"`
	}
	fb := strings.TrimSpace(ascii.String())
	if fb == "" {
		fb = "file"
	}
	escaped := strings.ReplaceAll(url.QueryEscape(utf8Name), "+", "%20")
	return `attachment; filename="` + fb + `"; filename*=UTF-8''` + escaped
}
