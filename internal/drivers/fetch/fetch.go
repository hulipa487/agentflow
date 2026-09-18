// Package fetch implements the guarded HTTP client behind builtin:fetch.
//
// "Guarded" is the substance of it: every connection is refused if it resolves
// to a private, loopback, link-local, unique-local or otherwise special-purpose
// address, and the check happens in the dialer rather than at URL-parse time so
// DNS rebinding and redirect hops cannot slip past it. See
// internal/core/netguard for the rule and why it sits where it does.
//
// The client holds two transports over one guarded dialer — a verifying one and
// one with certificate checks disabled — and picks between them per request.
// Skipping verification is a decision about who the peer claims to be; it is
// not a decision about where the connection goes, so the address guard applies
// either way.
//
// Nothing here logs or counts. The caller (builtin:fetch) is what knows which
// agent is asking, so it is what raises the alert when verification is skipped.
package fetch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"agentflow/internal/core/netguard"
)

// Bounds on one request. MaxBytes is the one that matters: a tool result is
// handed to the model whole, nothing downstream truncates it, and the loop's
// token budgeter does not count it — so an unbounded body would evict real
// conversation history rather than being trimmed.
const (
	defaultTimeout      = 30 * time.Second
	maxTimeout          = 120 * time.Second
	defaultMaxBytes     = 1 << 20  // 1 MiB
	ceilingMaxBytes     = 20 << 20 // 20 MiB
	defaultMaxRedirects = 10
	ceilingMaxRedirects = 20
)

// secretHeaderRe matches header names that carry credentials. Their values are
// scrubbed from any error this package returns: a transport error can embed the
// request it failed on, and the caller's own arguments are the one place a
// credential is most likely to be sitting in the clear.
var secretHeaderRe = regexp.MustCompile(`(?i)authorization|token|api[_-]?key|secret|password|cookie`)

// methodRe is the RFC 7230 token shape, upper-cased. Anything else is refused
// rather than handed to the transport, so a caller cannot smuggle CR/LF or
// punctuation into the request line.
var methodRe = regexp.MustCompile(`^[A-Z]+$`)

// Client is an HTTP client whose connections are governed by a netguard Policy.
// It is safe for concurrent use: the transports are shared and each request
// gets its own *http.Client (which carries the per-request redirect policy).
type Client struct {
	verified *http.Transport
	insecure *http.Transport
}

// New builds a client. The policy is applied to both transports.
func New(policy netguard.Policy) *Client {
	return &Client{
		verified: policy.Transport(),
		insecure: policy.InsecureTransport(),
	}
}

// Request is one HTTP request. URL is the only required field.
type Request struct {
	URL    string
	Method string // default GET

	Headers map[string]string
	Body    string            // raw request body
	JSON    any               // marshalled into the body; sets Content-Type when the caller did not
	Query   map[string]string // merged into the URL's query string

	Timeout time.Duration // default 30s, clamped to 120s
	// NoFollow stops at the first redirect and returns the 3xx itself. It is
	// deliberately the negated form: the useful default is to follow, and a
	// plain bool would make the zero value mean "do not follow" — a caller who
	// simply left it unset would get the opposite of what the docs promise.
	NoFollow     bool
	MaxRedirects int // default 10, clamped to 20
	UserAgent    string
	MaxBytes     int64 // response body cap; default 1 MiB, clamped to 20 MiB

	// Insecure skips TLS certificate verification. It is reported back on the
	// Response so the caller can alert on it.
	Insecure bool
}

// Response is one HTTP response.
type Response struct {
	Status     int
	StatusText string
	Headers    map[string]string
	Body       string

	// FinalURL is the URL the body actually came from, which differs from the
	// requested one when redirects were followed.
	FinalURL  string
	Redirects int

	ContentType string
	// Size is the number of body bytes read, before the cap; ContentLength is
	// what the server declared (-1 when it declared nothing). They differ when
	// the body was truncated or when the server lied.
	Size          int64
	ContentLength int64
	Truncated     bool

	// TLSVerified is false only when certificate verification was actually
	// skipped for this request — Insecure on an https URL. A plain http
	// request has nothing to verify and stays true.
	TLSVerified bool
	TimeCostMs  int64
}

// Do runs one request. A connection refused by the guard comes back as an
// error wrapping *netguard.BlockedError, which the caller detects with
// netguard.IsBlocked to count the refusal separately from a transport failure.
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	parsed, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil {
		return nil, fmt.Errorf("fetch: bad url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("fetch: scheme must be http or https (got %q)", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("fetch: url has no host")
	}

	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	if !methodRe.MatchString(method) {
		return nil, fmt.Errorf("fetch: invalid method %q", req.Method)
	}

	headers := make(map[string]string, len(req.Headers))
	for k, v := range req.Headers {
		headers[k] = v
	}

	var bodyReader io.Reader
	if req.JSON != nil {
		b, err := json.Marshal(req.JSON)
		if err != nil {
			return nil, fmt.Errorf("fetch: marshal json body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
		if _, ok := headerLookup(headers, "Content-Type"); !ok {
			headers["Content-Type"] = "application/json"
		}
	} else if req.Body != "" {
		bodyReader = strings.NewReader(req.Body)
	}

	if len(req.Query) > 0 {
		q := parsed.Query()
		for k, v := range req.Query {
			q.Set(k, v)
		}
		parsed.RawQuery = q.Encode()
	}

	// Everything that must not reach a log or an error message.
	secrets := secretValues(headers)
	if parsed.User != nil {
		if pw, ok := parsed.User.Password(); ok && pw != "" {
			secrets = append(secrets, pw)
		}
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	hreq, err := http.NewRequestWithContext(callCtx, method, parsed.String(), bodyReader)
	if err != nil {
		return nil, redact(fmt.Errorf("fetch: build request: %w", err), secrets)
	}
	if req.UserAgent != "" {
		hreq.Header.Set("User-Agent", req.UserAgent)
	}
	for _, k := range sortedKeys(headers) {
		hreq.Header.Set(k, headers[k])
	}

	maxRedirects := req.MaxRedirects
	if maxRedirects <= 0 {
		maxRedirects = defaultMaxRedirects
	}
	if maxRedirects > ceilingMaxRedirects {
		maxRedirects = ceilingMaxRedirects
	}
	redirects := 0
	// Built per request rather than stored: http.Client holds a mutex, so it
	// must not be copied, and the redirect policy is per-call.
	hclient := &http.Client{
		Transport: c.transportFor(req.Insecure),
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if req.NoFollow {
				return http.ErrUseLastResponse
			}
			redirects = len(via)
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			// The dialer guards every hop's address on its own; this guards
			// the scheme, which the dialer does not see.
			if next.URL.Scheme != "http" && next.URL.Scheme != "https" {
				return fmt.Errorf("refusing redirect to scheme %q", next.URL.Scheme)
			}
			return nil
		},
	}

	start := time.Now()
	resp, err := hclient.Do(hreq)
	if err != nil {
		return nil, redact(err, secrets)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start).Milliseconds()

	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	if maxBytes > ceilingMaxBytes {
		maxBytes = ceilingMaxBytes
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, redact(fmt.Errorf("fetch: read body: %w", err), secrets)
	}
	size := int64(len(raw))
	truncated := size > maxBytes
	if truncated {
		raw = raw[:maxBytes]
	}

	out := &Response{
		Status:        resp.StatusCode,
		StatusText:    http.StatusText(resp.StatusCode),
		Headers:       flattenHeaders(resp.Header),
		Body:          string(raw),
		FinalURL:      resp.Request.URL.String(),
		Redirects:     redirects,
		ContentType:   resp.Header.Get("Content-Type"),
		Size:          size,
		ContentLength: resp.ContentLength,
		Truncated:     truncated,
		TLSVerified:   !(req.Insecure && parsed.Scheme == "https"),
		TimeCostMs:    elapsed,
	}
	return out, nil
}

// transportFor returns the transport matching the verification choice. Both
// share the guarded dialer, so an insecure request is still address-checked.
func (c *Client) transportFor(insecure bool) *http.Transport {
	if insecure {
		return c.insecure
	}
	return c.verified
}

// headerLookup finds a header by case-insensitive name. The caller's map is
// keyed by whatever spelling the model chose, while HTTP header names are
// case-insensitive, so a plain map index would miss "content-type".
func headerLookup(headers map[string]string, name string) (string, bool) {
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}

// secretValues collects the header values that must never appear in an error.
func secretValues(headers map[string]string) []string {
	var out []string
	for k, v := range headers {
		if v != "" && secretHeaderRe.MatchString(k) {
			out = append(out, v)
		}
	}
	return out
}

// redact scrubs credential values from an error message.
//
// It returns the original error unchanged when nothing was scrubbed, which is
// the common case and matters: wrapping would break the error chain that
// netguard.IsBlocked walks, and an address refusal never contains a credential
// anyway.
func redact(err error, secrets []string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	changed := false
	for _, s := range secrets {
		if s == "" || !strings.Contains(msg, s) {
			continue
		}
		msg = strings.ReplaceAll(msg, s, "***")
		changed = true
	}
	if !changed {
		return err
	}
	return errors.New(msg)
}

// flattenHeaders joins multi-valued headers the way a caller reads them.
func flattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k := range h {
		out[k] = strings.Join(h[k], ", ")
	}
	return out
}

// sortedKeys makes header application deterministic, so a request built from a
// map is byte-identical run to run.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
