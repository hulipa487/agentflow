package ext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"agentflow/internal/core/hosthttp"
)

// HostDeps supplies the Core-owned services exposed to External API sandboxes.
type HostDeps struct {
	HTTP *hosthttp.Client
	Log  *slog.Logger
}

type httpInvokeReq struct {
	Method      string            `json:"method,omitempty"`
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers,omitempty"`
	QueryParams map[string]string `json:"query_params,omitempty"`
	Body        string            `json:"body,omitempty"`
	JSON        any               `json:"json,omitempty"`
	AuthService string            `json:"auth_service,omitempty"`
	Auth        *struct {
		Service string `json:"service"`
	} `json:"auth,omitempty"`
	TimeoutMS int `json:"timeout_ms,omitempty"`
}

// executeHTTP mediates one outbound HTTP call on behalf of an External API,
// strictly enforcing the package's declared allowed_hosts and credentials allowlist.
func executeHTTP(ctx context.Context, m Manifest, deps HostDeps, rawJSON []byte) (map[string]any, error) {
	if m.Capabilities.HTTP == nil || len(m.Capabilities.HTTP.AllowedHosts) == 0 {
		return nil, fmt.Errorf("api %q does not declare capabilities.http.allowed_hosts", m.Name)
	}
	if deps.HTTP == nil {
		return nil, errors.New("host HTTP client is unavailable")
	}

	var req httpInvokeReq
	if err := json.Unmarshal(rawJSON, &req); err != nil {
		return nil, fmt.Errorf("invalid http request payload: %w", err)
	}
	authSvc := strings.TrimSpace(req.AuthService)
	if authSvc == "" && req.Auth != nil {
		authSvc = strings.TrimSpace(req.Auth.Service)
	}
	var timeout time.Duration
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}

	resp, err := deps.HTTP.Do(ctx, hosthttp.Request{
		Method:                     req.Method,
		URL:                        req.URL,
		Headers:                    req.Headers,
		QueryParams:                req.QueryParams,
		Body:                       req.Body,
		JSON:                       req.JSON,
		AuthService:                authSvc,
		Timeout:                    timeout,
		AllowedHosts:               m.Capabilities.HTTP.AllowedHosts,
		AllowedCredentials:         m.Capabilities.HTTP.Credentials,
		RequireCredentialAllowlist: true,
	})
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"ok":      resp.OK,
		"status":  resp.Status,
		"headers": resp.Headers,
		"body":    resp.Body,
	}
	if resp.Truncated {
		out["truncated"] = true
	}
	return out, nil
}

func logFromExtension(deps HostDeps, apiName, action, level, msg string) {
	if deps.Log == nil {
		return
	}
	lvl := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	deps.Log.Log(context.Background(), lvl, msg, "api", apiName, "action", action)
}
