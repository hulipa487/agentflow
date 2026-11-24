package caps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"agentflow/internal/core/credentials"
	"agentflow/internal/core/files"
	"agentflow/internal/core/hosthttp"
	"agentflow/internal/core/netguard"
	"agentflow/internal/core/session"
)

// HTTPHandlers returns the per-agent HTTP op handler map. Two ops:
//   - http.request: issue a single HTTP request, return {ok,status,headers,body}
//   - os.env: read a process environment variable (for API keys in headers)
//
// creds is the encrypted per-tenant credential store; when non-nil, the
// http.request op resolves an `auth={service=...}` reference into a request
// header at call time. When nil, auth references fail with a clear error.
//
// policy governs where a connection may go and is enforced inside hosthttp.Client.
func HTTPHandlers(log *slog.Logger, creds *credentials.Store, policy netguard.Policy, fm *files.Manager) map[string]session.OpHandler {
	logger := log.With("module", "caps.http")
	client := hosthttp.New(log, creds, policy)
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
			var authService string
			if op.Auth != nil {
				authService = op.Auth.Service
			}
			var timeout time.Duration
			if op.Timeout > 0 {
				timeout = time.Duration(op.Timeout * float64(time.Second))
			}
			var jsonBody any
			if op.Json != nil {
				jsonBody = op.Json
			}
			resp, err := client.Do(ctx, hosthttp.Request{
				Method:      op.Method,
				URL:         op.URL,
				Headers:     op.Headers,
				QueryParams: op.QueryParams,
				Body:        op.Body,
				JSON:        jsonBody,
				AuthService: authService,
				Timeout:     timeout,
			})
			if err != nil {
				return fail(fmt.Errorf("http.request: %w", err))
			}

			result := map[string]any{
				"ok":      resp.OK,
				"status":  resp.Status,
				"headers": resp.Headers,
				"body":    resp.Body,
			}
			if resp.Truncated {
				result["truncated"] = true
				logger.Warn("http.response truncated", "url", strings.TrimSpace(op.URL), "cap", hosthttp.MaxBodyBytes)
			}
			if op.SaveTo != "" {
				// save_to: the body lands in the calling session's scratch
				// space (bytes never cross Lua). A failed save never discards
				// the response — it is reported alongside it.
				if fm == nil {
					result["save_error"] = "files store unavailable (disabled at boot)"
				} else if e, err := fm.ScratchPut(ctx, op.Owner, op.SaveTo, bytes.NewReader(resp.BodyBytes), resp.ContentType); err != nil {
					result["save_error"] = err.Error()
				} else {
					saved := map[string]any{"name": e.Path, "handle": e.Handle, "size": e.Size}
					if resp.Truncated {
						saved["truncated"] = true
					}
					result["saved"] = saved
				}
			}
			return okJSON(result)
		},
	}
}
