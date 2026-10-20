package tools

// Declarative HTTP tools. These are the no-Go-code integration path: an
// operator declares a REST endpoint in config, the runtime owns request
// construction, credential resolution, the address guard, response caps and the
// model schema. The tool is registered as http:<name> so it cannot collide with
// builtin:<...> or an MCP server's <server>/<tool>.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/credentials"
	"agentflow/internal/core/netguard"
	"agentflow/internal/core/session"
)

const (
	httpToolDefaultTimeout = 30 * time.Second
	httpToolMaxTimeout     = 120 * time.Second
	httpToolDefaultBytes   = 1 << 20
	httpToolMaxBytes       = 10 << 20
)

var httpToolTemplate = regexp.MustCompile(`\{([A-Za-z0-9_.-]+)\}`)

// RegisterHTTPTools registers every tools.http entry. A nil registry is a
// no-op; a definition that cannot be built is logged and skipped rather than
// taking down an unrelated deployment surface (config validation already
// refuses the malformed ones at boot).
func RegisterHTTPTools(r *Registry, defs []config.HTTPTool, creds *credentials.Store, policy netguard.Policy, log *slog.Logger) {
	if r == nil || len(defs) == 0 {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	client := &http.Client{Timeout: 0, Transport: policy.Transport()}
	for _, def := range defs {
		spec, err := buildHTTPTool(def, client, creds, log)
		if err != nil {
			log.Warn("tools.http skipped", "tool", def.Name, "err", err)
			continue
		}
		r.Register(spec)
	}
}

func buildHTTPTool(def config.HTTPTool, client *http.Client, creds *credentials.Store, log *slog.Logger) (ToolSpec, error) {
	name := "http:" + def.Name
	params := def.Params
	if params == nil {
		params = map[string]any{"type": "object", "additionalProperties": true}
	}
	method := strings.ToUpper(strings.TrimSpace(def.Method))
	if method == "" {
		method = http.MethodGet
	}
	timeout := httpToolDefaultTimeout
	if def.Timeout != "" {
		d, err := time.ParseDuration(def.Timeout)
		if err != nil || d <= 0 {
			return ToolSpec{}, fmt.Errorf("timeout: %w", err)
		}
		timeout = d
	}
	if timeout > httpToolMaxTimeout {
		timeout = httpToolMaxTimeout
	}
	maxBytes := def.MaxBytes
	if maxBytes == 0 {
		maxBytes = httpToolDefaultBytes
	}
	if maxBytes > httpToolMaxBytes {
		maxBytes = httpToolMaxBytes
	}
	logger := log.With("module", "tools.http", "tool", name)
	readOnly := method == http.MethodGet || method == http.MethodHead

	spec := ToolSpec{
		Name:               name,
		Description:        def.Description,
		Parameters:         params,
		Autonomous:         readOnly,
		RequiresCapability: "net.http",
		Invoke: func(ctx context.Context, args map[string]any) (any, error) {
			return invokeHTTPTool(ctx, def, method, timeout, maxBytes, client, creds, logger, args)
		},
	}
	if def.NeedsConfirm != nil {
		spec.NeedsConfirm = *def.NeedsConfirm
	} else {
		// A side-effecting HTTP call defaults to confirmation; a read does not.
		spec.NeedsConfirm = !readOnly
	}
	return spec, nil
}

func invokeHTTPTool(ctx context.Context, def config.HTTPTool, method string, timeout time.Duration,
	maxBytes int64, client *http.Client, creds *credentials.Store, log *slog.Logger, args map[string]any) (any, error) {
	rawURL, err := renderTemplate(def.URL, args, url.PathEscape, true)
	if err != nil {
		return nil, fmt.Errorf("%s: url: %w", def.Name, err)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%s: bad url: %w", def.Name, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("%s: scheme must be http or https", def.Name)
	}
	if !hostAllowed(parsed.Hostname(), def.Hosts) {
		return nil, fmt.Errorf("%s: host %q is not in hosts allow-list", def.Name, parsed.Hostname())
	}
	if len(def.Query) > 0 {
		q := parsed.Query()
		for key, tmpl := range def.Query {
			v, err := renderTemplate(tmpl, args, url.QueryEscape, false)
			if err != nil {
				return nil, fmt.Errorf("%s: query %s: %w", def.Name, key, err)
			}
			if v != "" {
				q.Set(key, v)
			}
		}
		parsed.RawQuery = q.Encode()
	}

	headers := map[string]string{}
	for k, v := range def.Headers {
		headers[k] = v
	}
	if def.Auth != nil && def.Auth.Service != "" {
		if creds == nil {
			return nil, fmt.Errorf("%s: auth=%q but credentials are not enabled", def.Name, def.Auth.Service)
		}
		uuid := session.CredentialUUIDFromCtx(ctx)
		if uuid == "" {
			return nil, fmt.Errorf("%s: auth=%q but no user in context", def.Name, def.Auth.Service)
		}
		cred, ok, err := creds.Get(ctx, uuid, def.Auth.Service)
		if err != nil {
			return nil, fmt.Errorf("%s: resolve auth %q: %w", def.Name, def.Auth.Service, err)
		}
		if !ok {
			return nil, fmt.Errorf("%s: no credential for service %q", def.Name, def.Auth.Service)
		}
		header := cred.Header
		if header == "" {
			header = "Authorization"
		}
		scheme := cred.Scheme
		if scheme == "" {
			scheme = "Bearer"
		}
		headers[header] = scheme + " " + cred.Value
	}

	var body io.Reader
	if method != http.MethodGet && method != http.MethodHead {
		var payload any
		if def.Body != nil {
			payload, err = renderMap(def.Body, args)
			if err != nil {
				return nil, fmt.Errorf("%s: body: %w", def.Name, err)
			}
		} else {
			payload = args
		}
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("%s: marshal body: %w", def.Name, err)
		}
		body = bytes.NewReader(b)
		if _, ok := headers["Content-Type"]; !ok {
			headers["Content-Type"] = "application/json"
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, method, parsed.String(), body)
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", def.Name, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		if netguard.IsBlocked(err) {
			log.Warn("declarative HTTP tool refused by the address guard", "url", parsed.Redacted())
		}
		return nil, fmt.Errorf("%s: %w", def.Name, err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s: read response: %w", def.Name, err)
	}
	truncated := int64(len(b)) > maxBytes
	if truncated {
		b = b[:maxBytes]
	}
	text := string(b)
	if def.Response != "" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var parsedBody any
		if err := json.Unmarshal(b, &parsedBody); err == nil {
			if selected, ok := selectJSONPath(parsedBody, def.Response); ok {
				return selected, nil
			}
			return map[string]any{
				"ok":            true,
				"status":        resp.StatusCode,
				"body":          text,
				"select_missed": def.Response,
			}, nil
		}
		// A non-JSON response cannot satisfy a JSON path; return it rather than
		// pretending the call failed.
	}
	return map[string]any{
		"ok":        resp.StatusCode >= 200 && resp.StatusCode < 300,
		"status":    resp.StatusCode,
		"body":      text,
		"truncated": truncated,
	}, nil
}

func renderTemplate(tmpl string, args map[string]any, escape func(string) string, strict bool) (string, error) {
	var missing string
	out := httpToolTemplate.ReplaceAllStringFunc(tmpl, func(m string) string {
		key := m[1 : len(m)-1]
		v, ok := args[key]
		if !ok || v == nil {
			if strict {
				missing = key
			}
			return ""
		}
		return escape(fmt.Sprint(v))
	})
	if missing != "" {
		return "", fmt.Errorf("missing argument %q", missing)
	}
	return out, nil
}

// renderMap renders a body mapping. A string that is exactly "{arg}" preserves
// the argument's JSON type (object, array, number); every other string is a
// template. A missing argument drops that key instead of sending an empty
// value, which is what optional API fields want.
func renderMap(m map[string]any, args map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(m))
	for k, v := range m {
		rv, present, err := renderValue(v, args)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		if present {
			out[k] = rv
		}
	}
	return out, nil
}

func renderValue(v any, args map[string]any) (any, bool, error) {
	switch x := v.(type) {
	case string:
		if m := httpToolTemplate.FindStringSubmatch(x); len(m) == 2 && m[0] == x {
			key := m[1]
			raw, ok := args[key]
			if !ok || raw == nil {
				return nil, false, nil
			}
			return raw, true, nil
		}
		rendered, err := renderTemplate(x, args, func(s string) string { return s }, false)
		if err != nil {
			return nil, false, err
		}
		return rendered, true, nil
	case map[string]any:
		m, err := renderMap(x, args)
		if err != nil {
			return nil, false, err
		}
		return m, true, nil
	case []any:
		out := make([]any, 0, len(x))
		for _, item := range x {
			rv, present, err := renderValue(item, args)
			if err != nil {
				return nil, false, err
			}
			if present {
				out = append(out, rv)
			}
		}
		return out, true, nil
	default:
		return v, true, nil
	}
}

func hostAllowed(host string, allow []string) bool {
	if len(allow) == 0 {
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, raw := range allow {
		a := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
		if suffix, ok := strings.CutPrefix(a, "*."); ok {
			if host != suffix && strings.HasSuffix(host, "."+suffix) {
				return true
			}
			continue
		}
		if host == a {
			return true
		}
	}
	return false
}

// selectJSONPath walks a dotted path with optional [index] segments:
// "customer.id", "items[0].name". It is intentionally a small subset: the
// alternative is a JSONPath dependency for what is almost always a two-level
// selection.
func selectJSONPath(v any, path string) (any, bool) {
	cur := v
	for _, part := range strings.Split(path, ".") {
		if part == "" {
			return nil, false
		}
		name := part
		indices := []int{}
		for {
			open := strings.LastIndex(name, "[")
			if open < 0 || !strings.HasSuffix(name, "]") {
				break
			}
			n, err := strconvAtoi(name[open+1 : len(name)-1])
			if err != nil {
				return nil, false
			}
			indices = append([]int{n}, indices...)
			name = name[:open]
		}
		if name != "" {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, false
			}
			cur, ok = m[name]
			if !ok {
				return nil, false
			}
		}
		for _, idx := range indices {
			arr, ok := cur.([]any)
			if !ok || idx < 0 || idx >= len(arr) {
				return nil, false
			}
			cur = arr[idx]
		}
	}
	return cur, true
}

func strconvAtoi(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("empty index")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("bad index")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}
