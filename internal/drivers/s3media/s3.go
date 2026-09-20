// Package s3media implements media.Store over S3 (or an S3-compatible
// endpoint such as MinIO). Blobs are content-addressed exactly like the
// filesystem store — the sha256 handle is the object key — so handles stay
// backend-agnostic and writes dedupe (HEAD before PUT). SigV4 is implemented
// with the stdlib only: agentflow ships a single binary with no AWS SDK.
//
// Payloads are signed as UNSIGNED-PAYLOAD (valid over TLS), which keeps PUT
// streaming-friendly. Credentials come from config (env-interpolated) and
// never appear in errors or logs.
package s3media

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/media"
)

// Store is an S3-backed media.Store.
type Store struct {
	bucket   string
	region   string
	endpoint string // custom base (https://host[:port]); empty = AWS virtual-hosted
	prefix   string
	key      string
	secret   string
	http     *http.Client
	now      func() time.Time // test hook
}

// New builds an S3 media store from config.
func New(cfg config.MediaS3) (*Store, error) {
	if cfg.Bucket == "" || cfg.Region == "" {
		return nil, fmt.Errorf("s3media: bucket and region are required")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("s3media: access_key and secret_key are required")
	}
	ep := strings.TrimSuffix(cfg.Endpoint, "/")
	if ep != "" && !strings.HasPrefix(ep, "http") {
		ep = "https://" + ep
	}
	return &Store{
		bucket:   cfg.Bucket,
		region:   cfg.Region,
		endpoint: ep,
		prefix:   strings.Trim(cfg.Prefix, "/"),
		key:      cfg.AccessKey,
		secret:   cfg.SecretKey,
		http:     &http.Client{Timeout: 60 * time.Second},
		now:      time.Now,
	}, nil
}

// objectKey maps a blob handle onto its S3 object key.
func (s *Store) objectKey(handle string) string {
	sum := strings.TrimPrefix(handle, "media:")
	if s.prefix == "" {
		return sum
	}
	return s.prefix + "/" + sum
}

// objectURL builds the request URL for key: virtual-hosted style against AWS,
// path style against a custom endpoint.
func (s *Store) objectURL(key string) string {
	if s.endpoint != "" {
		return s.endpoint + "/" + s.bucket + "/" + key
	}
	return fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", s.bucket, s.region, key)
}

// Put streams r into S3 under the policy ceiling, content-addressed by
// sha256. The body is spooled to a temp file so the hash is computed once and
// the PUT can carry a Content-Length; a HEAD first dedupes repeat writes.
func (s *Store) Put(r io.Reader, mime string, pol media.Policy) (*media.Ref, error) {
	limit := polMaxBytes(pol)
	tmp, err := os.CreateTemp("", "s3media-in-*")
	if err != nil {
		return nil, fmt.Errorf("s3media: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("s3media: %w", err)
	}
	if size > limit {
		return nil, fmt.Errorf("%w: %d bytes (limit %d)", media.ErrTooLarge, size, limit)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	ref := &media.Ref{Handle: "media:" + sum, MIME: mime, Size: size}
	key := s.objectKey(ref.Handle)

	// Dedupe: an existing object with the same key is the same content.
	exists, err := s.head(key)
	if err != nil {
		return nil, err
	}
	if exists {
		return ref, nil
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("s3media: %w", err)
	}
	req, err := http.NewRequest(http.MethodPut, s.objectURL(key), tmp)
	if err != nil {
		return nil, fmt.Errorf("s3media: build put: %w", err)
	}
	req.ContentLength = size
	if mime != "" {
		req.Header.Set("Content-Type", mime)
	}
	if err := s.do(req); err != nil {
		return nil, fmt.Errorf("s3media: put %s: %w", ref.Handle, err)
	}
	return ref, nil
}

// ReadAll reads the whole blob, enforcing limit as a sanity ceiling.
func (s *Store) ReadAll(handle string, limit int64) ([]byte, error) {
	if !media.ValidHandle(handle) {
		return nil, fmt.Errorf("s3media: malformed handle %q", handle)
	}
	req, err := http.NewRequest(http.MethodGet, s.objectURL(s.objectKey(handle)), nil)
	if err != nil {
		return nil, fmt.Errorf("s3media: build get: %w", err)
	}
	resp, err := s.doRaw(req)
	if err != nil {
		return nil, fmt.Errorf("s3media: get %s: %w", handle, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("s3media: read %s: %w", handle, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%w: %s", media.ErrExceedsLimit, handle)
	}
	return b, nil
}

// List returns every blob in the bucket under the configured prefix
// (paginated ListObjectsV2), for mark-sweep GC.
func (s *Store) List() ([]media.Blob, error) {
	var out []media.Blob
	var token string
	for {
		listURL := s.objectURL("") // bucket listing base
		q := url.Values{
			"list-type":          {"2"},
			"prefix":             {s.prefixWithSlash()},
			"continuation-token": {token},
			"max-keys":           {"1000"},
		}
		// A zero token must not be sent.
		if token == "" {
			delete(q, "continuation-token")
		}
		req, err := http.NewRequest(http.MethodGet, listURL+"?"+q.Encode(), nil)
		if err != nil {
			return nil, fmt.Errorf("s3media: build list: %w", err)
		}
		resp, err := s.doRaw(req)
		if err != nil {
			return nil, fmt.Errorf("s3media: list: %w", err)
		}
		var page struct {
			Contents []struct {
				Key          string    `xml:"Key"`
				Size         int64     `xml:"Size"`
				LastModified time.Time `xml:"LastModified"`
			} `xml:"Contents"`
			Next string `xml:"NextContinuationToken"`
		}
		if err := xml.NewDecoder(resp.Body).Decode(&page); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("s3media: decode list: %w", err)
		}
		resp.Body.Close()
		for _, c := range page.Contents {
			sum := strings.TrimPrefix(c.Key, s.prefixWithSlash())
			if h := "media:" + sum; media.ValidHandle(h) {
				out = append(out, media.Blob{Handle: h, Size: c.Size, ModTime: c.LastModified})
			}
		}
		if page.Next == "" {
			return out, nil
		}
		token = page.Next
	}
}

// prefixWithSlash is the object key prefix with exactly one trailing slash
// (or "" when unconfigured) — the namespace every blob key lives under.
func (s *Store) prefixWithSlash() string {
	if s.prefix == "" {
		return ""
	}
	return s.prefix + "/"
}

// Delete removes one blob. A missing object is not an error (idempotent GC).
func (s *Store) Delete(handle string) error {
	if !media.ValidHandle(handle) {
		return fmt.Errorf("s3media: malformed handle %q", handle)
	}
	req, err := http.NewRequest(http.MethodDelete, s.objectURL(s.objectKey(handle)), nil)
	if err != nil {
		return fmt.Errorf("s3media: build delete: %w", err)
	}
	s.sign(req, "UNSIGNED-PAYLOAD")
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("s3media: delete %s: %w", handle, err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("s3media: delete %s: status %d", handle, resp.StatusCode)
	}
	return nil
}

// head reports whether key already exists (404 = no).
func (s *Store) head(key string) (bool, error) {
	req, err := http.NewRequest(http.MethodHead, s.objectURL(key), nil)
	if err != nil {
		return false, fmt.Errorf("s3media: build head: %w", err)
	}
	resp, err := s.doRawAllow404(req)
	if err != nil {
		return false, fmt.Errorf("s3media: head: %w", err)
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}

// --- SigV4 signing (stdlib only) --------------------------------------------

func (s *Store) sign(req *http.Request, payloadHash string) {
	t := s.now().UTC()
	amzDate := t.Format("20060102T150405Z")
	date := t.Format("20060102")

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	signedHeaders, canonicalHeaders := canonicalHeaders(req)
	canonReq := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL.Query()),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := date + "/" + s.region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hexSHA256(canonReq)

	kDate := hmacSHA256([]byte("AWS4"+s.secret), date)
	kRegion := hmacSHA256(kDate, s.region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.key+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+sig)
}

// do signs and performs a request that must succeed (2xx), with the payload
// hash UNSIGNED-PAYLOAD.
func (s *Store) do(req *http.Request) error {
	resp, err := s.doRaw(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (s *Store) doRaw(req *http.Request) (*http.Response, error) {
	resp, err := s.doRawAllow404(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, fmt.Errorf("not found")
	}
	return resp, nil
}

func (s *Store) doRawAllow404(req *http.Request) (*http.Response, error) {
	s.sign(req, "UNSIGNED-PAYLOAD")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return resp, nil
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		// S3 error bodies carry the request's Host/Resource but never the
		// signing key; still, surface only the status and a trimmed body.
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, trim(b, 512))
	}
	return resp, nil
}

// canonicalHeaders returns the signed-header list and the canonical header
// block. Only host and the x-amz-* headers are signed.
func canonicalHeaders(req *http.Request) (signed, block string) {
	host := req.URL.Host
	if h := req.Host; h != "" {
		host = h
	}
	hdrs := map[string]string{"host": host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") {
			hdrs[lk] = strings.Join(v, ",")
		}
	}
	keys := make([]string, 0, len(hdrs))
	for k := range hdrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s:%s\n", k, strings.TrimSpace(hdrs[k]))
	}
	return strings.Join(keys, ";"), b.String()
}

// canonicalURI is the path exactly as it goes on the wire (Go has already
// escaped it). It must NOT be normalized: the bucket-root listing path ends
// in a trailing slash ("/bucket/"), and path.Clean would strip it, signing a
// different URI than R2 receives — 403 SignatureDoesNotMatch on LIST while
// every object-path call passes. R2 is strict where permissive fakes are not.
func canonicalURI(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		return "/"
	}
	return p
}

// canonicalQuery is the SigV4 canonical query string: URI-encoded
// (RFC 3986 — a space is %20, never "+"), sorted by encoded key. Params with
// empty values keep the trailing "="; multi-value keys sort by encoded value.
func canonicalQuery(v url.Values) string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), v[k]...)
		sort.Strings(vals)
		for _, val := range vals {
			parts = append(parts, awsQueryEscape(k)+"="+awsQueryEscape(val))
		}
	}
	return strings.Join(parts, "&")
}

// awsQueryEscape is url.QueryEscape with the RFC 3986 space encoding SigV4
// requires (QueryEscape emits "+" for space, which breaks the signature).
func awsQueryEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func hexSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func polMaxBytes(pol media.Policy) int64 { return pol.MaxOrDefault() }

func trim(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
