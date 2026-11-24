// Package hosthttp provides a single audited outbound HTTP client shared by
// capability handlers (http.request) and External API extensions (Script/Wasm).
// Every request goes through netguard.Policy at dial time, optional host
// allowlists (checked initially and on every redirect hop), and host-side
// credential injection so raw secrets never enter VM or Wasm memory.
package hosthttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"time"

	"agentflow/internal/core/credentials"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/netguard"
	"agentflow/internal/core/session"
)

const (
	// MaxBodyBytes caps how much of an HTTP response is buffered in memory.
	MaxBodyBytes = 10 * 1024 * 1024

	// DefaultTimeout and MaxTimeout bound a single HTTP request.
	DefaultTimeout = 30 * time.Second
	MaxTimeout     = 120 * time.Second
)

var secretHeaderRe = regexp.MustCompile(`(?i)authorization|token|api[_-]?key|secret|password`)

// Client executes outbound HTTP requests under NetGuard, optional host/cred
// allowlists, and per-tenant credential injection.
type Client struct {
	log    *slog.Logger
	creds  *credentials.Store
	policy netguard.Policy
	base   *http.Client
}

// New constructs a hosthttp Client. A nil logger falls back to slog.Default().
func New(log *slog.Logger, creds *credentials.Store, policy netguard.Policy) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		log:    log.With("module", "hosthttp"),
		creds:  creds,
		policy: policy,
		base:   &http.Client{Timeout: 0, Transport: policy.Transport()},
	}
}

// Request describes one outbound HTTP call initiated from a loop or extension.
type Request struct {
	Method             string
	URL                string
	Headers            map[string]string
	QueryParams        map[string]string
	Body               string
	JSON               any
	AuthService        string
	Timeout            time.Duration
	AllowedHosts       []string
	AllowedCredentials []string
	// RequireCredentialAllowlist refuses any AuthService when AllowedCredentials is empty.
	RequireCredentialAllowlist bool
}

// Response is the bounded HTTP response handed back to the caller.
type Response struct {
	OK          bool              `json:"ok"`
	Status      int               `json:"status"`
	Headers     map[string]string `json:"headers"`
	Body        string            `json:"body"`
	BodyBytes   []byte            `json:"-"`
	ContentType string            `json:"-"`
	Truncated   bool              `json:"truncated,omitempty"`
}

// HostAllowed reports whether hostname satisfies patterns (exact case-insensitive
// match or "*.domain.com" subdomain wildcard). Empty patterns allow all hosts.
func HostAllowed(hostname string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	h := strings.ToLower(strings.TrimSpace(hostname))
	if h == "" {
		return false
	}
	for _, raw := range patterns {
		pat := strings.ToLower(strings.TrimSpace(raw))
		if pat == "" {
			continue
		}
		if pat == "*" || h == pat {
			return true
		}
		if strings.HasPrefix(pat, "*.") {
			suffix := pat[1:] // ".domain.com"
			if strings.HasSuffix(h, suffix) && len(h) > len(suffix) {
				return true
			}
		}
	}
	return false
}

func stringInSet(target string, set []string) bool {
	for _, item := range set {
		if item == target {
			return true
		}
	}
	return false
}

// Do executes req using the shared NetGuard transport, resolving any declared
// credential on the host without exposing its secret value to callers.
func (c *Client) Do(ctx context.Context, reqSpec Request) (*Response, error) {
	rawURL := strings.TrimSpace(reqSpec.URL)
	if rawURL == "" {
		return nil, errors.New("url is required")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("bad url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("scheme must be http or https (got %q)", parsed.Scheme)
	}
	if !HostAllowed(parsed.Hostname(), reqSpec.AllowedHosts) {
		return nil, fmt.Errorf("host %q is not in allowed_hosts", parsed.Hostname())
	}

	if len(reqSpec.QueryParams) > 0 {
		q := parsed.Query()
		for k, v := range reqSpec.QueryParams {
			q.Set(k, v)
		}
		parsed.RawQuery = q.Encode()
	}

	method := strings.ToUpper(strings.TrimSpace(reqSpec.Method))
	if method == "" {
		method = http.MethodGet
	}

	headers := make(map[string]string, len(reqSpec.Headers)+2)
	for k, v := range reqSpec.Headers {
		headers[k] = v
	}

	var credHeader string
	if reqSpec.AuthService != "" {
		if reqSpec.RequireCredentialAllowlist && !stringInSet(reqSpec.AuthService, reqSpec.AllowedCredentials) {
			return nil, fmt.Errorf("credential %q is not permitted by capabilities.http.credentials", reqSpec.AuthService)
		}
		if len(reqSpec.AllowedCredentials) > 0 && !stringInSet(reqSpec.AuthService, reqSpec.AllowedCredentials) {
			return nil, fmt.Errorf("credential %q is not permitted by capabilities.http.credentials", reqSpec.AuthService)
		}
		if c.creds == nil {
			return nil, fmt.Errorf("auth=%q but credentials are not enabled", reqSpec.AuthService)
		}
		uuid := session.CredentialUUIDFromCtx(ctx)
		if uuid == "" {
			return nil, fmt.Errorf("auth=%q but no user in context", reqSpec.AuthService)
		}
		cred, ok, err := c.creds.Get(ctx, uuid, reqSpec.AuthService)
		if err != nil {
			return nil, fmt.Errorf("resolve auth %q: %w", reqSpec.AuthService, err)
		}
		if !ok {
			return nil, fmt.Errorf("no credential for service %q", reqSpec.AuthService)
		}
		scheme := cred.Scheme
		if scheme == "" {
			scheme = "Bearer"
		}
		header := cred.Header
		if header == "" {
			header = "Authorization"
		}
		headers[header] = scheme + " " + cred.Value
		credHeader = header
	}

	var bodyReader io.Reader
	if !isNilJSON(reqSpec.JSON) {
		b, err := json.Marshal(reqSpec.JSON)
		if err != nil {
			return nil, fmt.Errorf("marshal json body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
		if !hasHeaderFold(headers, "Content-Type") {
			headers["Content-Type"] = "application/json"
		}
	} else if reqSpec.Body != "" {
		bodyReader = strings.NewReader(reqSpec.Body)
	}

	req, err := http.NewRequestWithContext(ctx, method, parsed.String(), bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	timeout := reqSpec.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if timeout > MaxTimeout {
		timeout = MaxTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req = req.WithContext(callCtx)

	httpClient := *c.base
	if len(reqSpec.AllowedHosts) > 0 || credHeader != "" {
		initialHost := strings.ToLower(parsed.Host)
		allowed := reqSpec.AllowedHosts
		httpClient.CheckRedirect = func(r *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if r.URL.Scheme != "http" && r.URL.Scheme != "https" {
				return fmt.Errorf("redirect scheme must be http or https (got %q)", r.URL.Scheme)
			}
			if !HostAllowed(r.URL.Hostname(), allowed) {
				return fmt.Errorf("redirect host %q is not in allowed_hosts", r.URL.Hostname())
			}
			if credHeader != "" && strings.ToLower(r.URL.Host) != initialHost {
				r.Header.Del(credHeader)
			}
			return nil
		}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		if netguard.IsBlocked(err) {
			metrics.Inc("agentflow_http_private_blocked")
			c.log.Warn("http.request refused by the address guard",
				"agent", session.OwnerFromCtx(ctx), "url", rawURL, "err", err.Error())
		}
		return nil, redactErr(err, headers)
	}
	defer resp.Body.Close()

	lr := io.LimitReader(resp.Body, MaxBodyBytes+1)
	bodyBytes, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	truncated := false
	if len(bodyBytes) > MaxBodyBytes {
		bodyBytes = bodyBytes[:MaxBodyBytes]
		truncated = true
		c.log.Warn("http.response truncated", "url", rawURL, "cap", MaxBodyBytes)
	}

	respHeaders := make(map[string]string, len(resp.Header))
	for k := range resp.Header {
		respHeaders[k] = strings.Join(resp.Header[k], ", ")
	}
	return &Response{
		OK:          true,
		Status:      resp.StatusCode,
		Headers:     respHeaders,
		Body:        string(bodyBytes),
		BodyBytes:   bodyBytes,
		ContentType: resp.Header.Get("Content-Type"),
		Truncated:   truncated,
	}, nil
}

func hasHeaderFold(headers map[string]string, key string) bool {
	for k := range headers {
		if strings.EqualFold(k, key) {
			return true
		}
	}
	return false
}

func isNilJSON(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map, reflect.Slice, reflect.Pointer, reflect.Interface:
		return rv.IsNil()
	default:
		return false
	}
}

// redactErr scrubs any sensitive header value from err before it can reach
// callers, traces, or logs.
func redactErr(err error, headers map[string]string) error {
	msg := err.Error()
	for k, v := range headers {
		if v == "" || !secretHeaderRe.MatchString(k) {
			continue
		}
		if strings.Contains(msg, v) {
			msg = strings.ReplaceAll(msg, v, "***")
		}
	}
	if msg == err.Error() {
		return err
	}
	return fmt.Errorf("%s", msg)
}
