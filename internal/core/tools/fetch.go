package tools

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"agentflow/internal/core/files"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/netguard"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/fetch"
)

// RegisterFetchBuiltins adds builtin:fetch, a curl-like HTTP client.
//
// Unlike builtin:web_search and builtin:browser this tool needs no account and
// no credential, so it is never "unavailable" in the honest-degradation sense.
// The one thing that stops a call is the address guard, and a refusal surfaces
// as an error naming the address rather than as a quiet empty result — a guard
// that failed silently would be indistinguishable from a connection error.
//
// The guard is real, not advisory: it lives in the dialer
// (internal/core/netguard), so it also covers redirect hops and DNS rebinding,
// not just the URL the model typed. net.http.allow_private turns it off for
// deployments that legitimately call internal services.
func RegisterFetchBuiltins(r *Registry, c *fetch.Client, log *slog.Logger, fm *files.Manager) {
	if log == nil {
		log = slog.Default()
	}
	r.Register(ToolSpec{
		Name: "builtin:fetch",
		Description: "Fetch a URL over HTTP(S) and return the response — the general-purpose HTTP client, for APIs and plain pages. " +
			"Set method, headers, and either body (raw) or json (marshalled, sets Content-Type); query adds URL parameters. " +
			"Redirects are followed by default (follow:false to stop, max_redirects to cap). " +
			"A response is returned for any HTTP status, including 4xx and 5xx — read `status`; `ok` means the request itself completed. " +
			"Connections to private, loopback, link-local and reserved addresses are refused, so internal services and cloud metadata endpoints are not reachable. " +
			"Bodies are capped: the result reports the true `size` and sets `truncated` when it was cut. " +
			"insecure:true skips TLS certificate verification — use it only for a host you control; the skip is logged and reported as tls_verified:false.",
		Parameters: objectSchema(map[string]any{
			"url":    map[string]any{"type": "string", "description": "Absolute http:// or https:// URL."},
			"method": map[string]any{"type": "string", "description": "HTTP method (default GET)."},
			"headers": map[string]any{
				"type":        "object",
				"description": "Request headers, e.g. {\"Accept\": \"application/json\"}.",
			},
			"body":  map[string]any{"type": "string", "description": "Raw request body."},
			"json":  map[string]any{"type": "object", "description": "Request body as JSON; marshalled, and sets Content-Type unless a header already does."},
			"query": map[string]any{"type": "object", "description": "Query parameters merged into the URL."},
			"timeout_ms": map[string]any{
				"type":        "number",
				"description": "Request timeout in ms (default 30000, max 120000).",
			},
			"follow":        map[string]any{"type": "boolean", "description": "Follow redirects (default true)."},
			"max_redirects": map[string]any{"type": "number", "description": "Redirect cap (default 10, max 20)."},
			"user_agent":    map[string]any{"type": "string", "description": "Override the User-Agent."},
			"max_bytes": map[string]any{
				"type":        "number",
				"description": "Response body cap in bytes (default 1048576, max 20971520). The result reports the true size and whether it was cut.",
			},
			"insecure": map[string]any{
				"type":        "boolean",
				"description": "Skip TLS certificate verification (default false). Logged and flagged in the result.",
			},
			"save_to": map[string]any{
				"type":        "string",
				"description": "Scratch file name: the response body also lands in this session's files scratch space (readable via files.scratch.read). Bytes never enter the model context.",
			},
		}, []string{"url"}),
		Autonomous: true,
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			if c == nil {
				return ResultUnavailable("builtin:fetch", "The fetch client is not configured."), nil
			}
			rawURL := argString(args["url"])
			if rawURL == "" {
				return fetchArgErr("url is required")
			}
			insecure, _ := args["insecure"].(bool)

			res, err := c.Do(ctx, fetch.Request{
				URL:          rawURL,
				Method:       argString(args["method"]),
				Headers:      argStringMap(args["headers"]),
				Body:         argString(args["body"]),
				JSON:         args["json"],
				Query:        argStringMap(args["query"]),
				Timeout:      time.Duration(argCount(args["timeout_ms"])) * time.Millisecond,
				NoFollow:     !argBoolOr(args["follow"], true),
				MaxRedirects: argCount(args["max_redirects"]),
				UserAgent:    argString(args["user_agent"]),
				MaxBytes:     int64(argCount(args["max_bytes"])),
				Insecure:     insecure,
			})
			if err != nil {
				if netguard.IsBlocked(err) {
					metrics.Inc("agentflow_http_private_blocked")
					log.Warn("outbound fetch refused by the address guard",
						"agent", session.OwnerFromCtx(ctx), "url", rawURL, "err", err.Error())
				}
				return nil, fmt.Errorf("builtin:fetch: %w", err)
			}

			// The alert the caller asked for: never let a TLS downgrade happen
			// quietly. Keyed off the response rather than the argument so an
			// insecure flag on a plain http URL — where there is nothing to
			// verify — does not cry wolf.
			if !res.TLSVerified {
				metrics.Inc("agentflow_http_insecure_tls")
				log.Warn("tls verification skipped",
					"agent", session.OwnerFromCtx(ctx), "url", res.FinalURL, "status", res.Status)
			}

			out := map[string]any{
				"ok":           true,
				"tool":         "builtin:fetch",
				"status":       res.Status,
				"status_text":  res.StatusText,
				"headers":      res.Headers,
				"body":         res.Body,
				"final_url":    res.FinalURL,
				"size":         res.Size,
				"tls_verified": res.TLSVerified,
				"time_cost_ms": res.TimeCostMs,
			}
			if res.Redirects > 0 {
				out["redirects"] = res.Redirects
			}
			if res.ContentType != "" {
				out["content_type"] = res.ContentType
			}
			if res.ContentLength >= 0 {
				out["content_length"] = res.ContentLength
			}
			if res.Truncated {
				out["truncated"] = true
			}
			if name := argString(args["save_to"]); name != "" {
				saveToScratch(ctx, fm, out, name, []byte(res.Body), res.ContentType)
			}
			return out, nil
		},
	})
}

// fetchArgErr reports a malformed call in the result itself, with a nil error,
// so the model sees something it can correct rather than a tool failure.
func fetchArgErr(msg string) (any, error) {
	return map[string]any{"ok": false, "tool": "builtin:fetch", "error": msg}, nil
}

// argStringMap reads an object arg whose values must be strings. Lua delivers
// objects as map[string]any, so a direct assertion would miss every value the
// model sends; non-string values are dropped rather than stringified, since a
// header silently rendered as "[object]" is worse than an absent one.
func argStringMap(v any) map[string]string {
	src, ok := v.(map[string]any)
	if !ok {
		if ss, ok := v.(map[string]string); ok {
			return ss
		}
		return nil
	}
	out := make(map[string]string, len(src))
	for k, item := range src {
		if s, ok := item.(string); ok {
			out[k] = s
		}
	}
	return out
}

// argBoolOr reads a boolean arg, falling back when it is absent. It cannot use
// the zero value as "unset" the way the numeric helpers do, because false is a
// meaningful choice — follow:false must not become follow:true.
func argBoolOr(v any, fallback bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return fallback
}
