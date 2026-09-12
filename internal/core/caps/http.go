package caps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"agentflow/internal/core/credentials"
	"agentflow/internal/core/session"
)

// maxBodyBytes caps how much of an HTTP response we buffer, so a runaway
// endpoint can't OOM the worker pool. Larger bodies are truncated with a note.
const maxBodyBytes = 10 * 1024 * 1024

// defaultTimeout and maxTimeout bound a single HTTP request. A Lua caller
// asking for more than max gets clamped (fail soft).
const (
	defaultTimeout = 30 * time.Second
	maxTimeout     = 120 * time.Second
)

// sharedClient is reused across all http.request calls. Per-call timeouts are
// applied via context, not via Client.Timeout, so different calls can have
// different bounds.
var sharedClient = &http.Client{Timeout: 0}

// secretHeaderRe matches header names that carry secrets; values of such
// headers are redacted from any error/log output so secrets never land in logs.
var secretHeaderRe = regexp.MustCompile(`(?i)authorization|token|api[_-]?key|secret|password`)

// HTTPHandlers returns the per-agent HTTP op handler map. Two ops:
//   - http.request: issue a single HTTP request, return {ok,status,headers,body}
//   - os.env: read a process environment variable (for API keys in headers)
//
// creds is the encrypted per-tenant credential store; when non-nil, the
// http.request op resolves an `auth={service=...}` reference into a request
// header at call time. When nil, auth references fail with a clear error.
//
// Both are registered the same way as LLM/Shell/Tool handlers and merged into
// the actor's handler map in cmd/agentflow.
func HTTPHandlers(log *slog.Logger, creds *credentials.Store) map[string]session.OpHandler {
	logger := log.With("module", "caps.http")
	fail := func(err error) (string, bool) {
		b, _ := json.Marshal(err.Error())
		return string(b), false
	}
	okJSON := func(v any) (string, bool) {
		b, err := json.Marshal(v)
		if err != nil {
			return fail(err)
		}
		return string(b), true
	}

	return map[string]session.OpHandler{
		// os.env reads one process env var. Inline (not blocking); cheap.
		"os.env": func(ctx context.Context, op session.Op) (string, bool) {
			name := strings.TrimSpace(op.EnvName)
			if name == "" {
				return fail(fmt.Errorf("os.env: name is required"))
			}
			return okJSON(os.Getenv(name))
		},

		"http.request": func(ctx context.Context, op session.Op) (string, bool) {
			rawURL := strings.TrimSpace(op.URL)
			if rawURL == "" {
				return fail(fmt.Errorf("http.request: url is required"))
			}
			parsed, err := url.Parse(rawURL)
			if err != nil {
				return fail(fmt.Errorf("http.request: bad url: %w", err))
			}
			if parsed.Scheme != "http" && parsed.Scheme != "https" {
				return fail(fmt.Errorf("http.request: scheme must be http or https (got %q)", parsed.Scheme))
			}

			// Query params from the op's query table.
			if len(op.QueryParams) > 0 {
				q := parsed.Query()
				for k, v := range op.QueryParams {
					q.Set(k, v)
				}
				parsed.RawQuery = q.Encode()
			}

			method := strings.ToUpper(op.Method)
			if method == "" {
				method = "GET"
			}

			// Body: either op.Json (marshaled, sets content-type) or op.Body (raw).
			var bodyReader io.Reader
			headers := map[string]string{}
			for k, v := range op.Headers {
				headers[k] = v
			}

			// Resolve an `auth` credential reference into a request header. The
			// loop only names the service; the secret is fetched from the store
			// (encrypted at rest) and injected here, so it never crosses the
			// Lua bridge and lands in the same headers map redactErr scrubs.
			if op.Auth != nil && op.Auth.Service != "" {
				if creds == nil {
					return fail(fmt.Errorf("http.request: auth=%q but credentials are not enabled", op.Auth.Service))
				}
				uuid := session.UserUUIDFromCtx(ctx)
				if uuid == "" {
					return fail(fmt.Errorf("http.request: auth=%q but no user in context", op.Auth.Service))
				}
				cred, ok, err := creds.Get(ctx, uuid, op.Auth.Service)
				if err != nil {
					return fail(fmt.Errorf("http.request: resolve auth %q: %w", op.Auth.Service, err))
				}
				if !ok {
					return fail(fmt.Errorf("http.request: no credential for service %q", op.Auth.Service))
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
			}

			if op.Json != nil {
				b, err := json.Marshal(op.Json)
				if err != nil {
					return fail(fmt.Errorf("http.request: marshal json body: %w", err))
				}
				bodyReader = bytes.NewReader(b)
				if _, ok := headers["Content-Type"]; !ok {
					headers["Content-Type"] = "application/json"
				}
			} else if op.Body != "" {
				bodyReader = strings.NewReader(op.Body)
			}

			req, err := http.NewRequestWithContext(ctx, method, parsed.String(), bodyReader)
			if err != nil {
				return fail(fmt.Errorf("http.request: build request: %w", err))
			}
			for k, v := range headers {
				req.Header.Set(k, v)
			}

			// Per-call timeout. op.Timeout is in seconds.
			timeout := defaultTimeout
			if op.Timeout > 0 {
				timeout = time.Duration(op.Timeout * float64(time.Second))
				if timeout > maxTimeout {
					timeout = maxTimeout
				}
			}
			callCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			req = req.WithContext(callCtx)

			resp, err := sharedClient.Do(req)
			if err != nil {
				// Redact secret header values from the error string (Go's
				// http error can include the URL but not headers by default;
				// be defensive anyway).
				return fail(redactErr(fmt.Errorf("http.request: %w", err), headers))
			}
			defer resp.Body.Close()

			// Cap the body we buffer.
			lr := io.LimitReader(resp.Body, maxBodyBytes+1)
			bodyBytes, err := io.ReadAll(lr)
			if err != nil {
				return fail(fmt.Errorf("http.request: read body: %w", err))
			}
			truncated := false
			if len(bodyBytes) > maxBodyBytes {
				bodyBytes = bodyBytes[:maxBodyBytes]
				truncated = true
			}

			respHeaders := map[string]string{}
			for k := range resp.Header {
				respHeaders[k] = strings.Join(resp.Header[k], ", ")
			}
			result := map[string]any{
				"ok":      true,
				"status":  resp.StatusCode,
				"headers": respHeaders,
				"body":    string(bodyBytes),
			}
			if truncated {
				result["truncated"] = true
				logger.Warn("http.response truncated", "url", rawURL, "cap", maxBodyBytes)
			}
			return okJSON(result)
		},
	}
}

// redactErr ensures no secret header value appears in an error message that
// could reach logs. It replaces any occurrence of a secret value with "***".
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
