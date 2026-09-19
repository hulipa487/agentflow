package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"agentflow/internal/config"
	"agentflow/internal/drivers/browser"
)

// browserClientFor points a browser client at a test server, or returns nil to
// exercise the unconfigured path.
func browserClientFor(srv *httptest.Server) *browser.Client {
	if srv == nil {
		return nil
	}
	return browser.Build(config.Browser{
		AccountID: "acct",
		APIToken:  "tok",
		BaseURL:   srv.URL,
	}, &config.Resolver{}, nil)
}

// invokeBrowser registers and exposes builtin:browser exactly as main.go does,
// then runs one call.
func invokeBrowser(t *testing.T, srv *httptest.Server, args map[string]any) map[string]any {
	t.Helper()
	r := NewRegistry()
	RegisterBrowserBuiltins(r, browserClientFor(srv), nil)
	as := r.Expose([]string{"builtin:browser"}, config.ToolsPolicy{}, false)
	res, err := as.Invoke(context.Background(), "builtin:browser", args)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result type %T", res)
	}
	return m
}

// serveResult answers every request with {"success":true,"result":v}.
func serveResult(t *testing.T, v any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": v})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// serveCapturing answers every request and reports the decoded bodies it saw.
// A channel rather than a shared variable so the handler goroutine and the test
// goroutine synchronise explicitly.
func serveCapturing(t *testing.T) (*httptest.Server, chan map[string]any) {
	t.Helper()
	bodies := make(chan map[string]any, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies <- b
		_, _ = io.WriteString(w, `{"success":true,"result":""}`)
	}))
	t.Cleanup(srv.Close)
	return srv, bodies
}

// TestBrowserUnavailable: with no account configured the tool reports
// honest-unavailable rather than failing.
func TestBrowserUnavailable(t *testing.T) {
	m := invokeBrowser(t, nil, map[string]any{"action": "markdown", "url": "https://example.com"})
	if m["ok"] != false {
		t.Fatalf("expected ok=false, got %v", m["ok"])
	}
	if m["unavailable"] != true {
		t.Fatalf("expected unavailable=true, got %v", m["unavailable"])
	}
	if m["text"] != "No Browser Run account is configured." {
		t.Fatalf("text = %v", m["text"])
	}
}

// TestBrowserUnavailableSkipsArgumentChecks: the unavailable branch comes
// first, so an unconfigured deployment reports the real reason (nothing is
// configured) rather than complaining about arguments it will never use.
func TestBrowserUnavailableSkipsArgumentChecks(t *testing.T) {
	m := invokeBrowser(t, nil, map[string]any{"action": "nonsense"})
	if m["unavailable"] != true {
		t.Fatalf("expected unavailable, got %v", m)
	}
}

// TestBrowserActionDispatch: the action reaches the driver and the result is
// mapped onto the tool's result shape.
func TestBrowserActionDispatch(t *testing.T) {
	srv := serveResult(t, "# Example Domain\n\nBody text.")
	m := invokeBrowser(t, srv, map[string]any{"action": "markdown", "url": "https://example.com"})
	if m["ok"] != true {
		t.Fatalf("ok = %v (%v)", m["ok"], m["error"])
	}
	if m["action"] != "markdown" {
		t.Fatalf("action = %v", m["action"])
	}
	if m["url"] != "https://example.com" {
		t.Fatalf("url = %v", m["url"])
	}
	if !strings.Contains(m["markdown"].(string), "Example Domain") {
		t.Fatalf("markdown = %v", m["markdown"])
	}
	if m["chars"] != len("# Example Domain\n\nBody text.") {
		t.Fatalf("chars = %v", m["chars"])
	}
	if _, cut := m["truncated"]; cut {
		t.Fatal("a short result must not be flagged truncated")
	}
	if _, has := m["time_cost_ms"]; !has {
		t.Fatal("time_cost_ms must be reported")
	}
}

// TestBrowserArgumentValidation: a malformed call comes back as a correctable
// result with a nil error, not as a tool failure.
func TestBrowserArgumentValidation(t *testing.T) {
	srv := serveResult(t, "")
	cases := map[string]struct {
		args map[string]any
		want string
	}{
		"unknown action": {
			args: map[string]any{"action": "screenshot", "url": "https://example.com"},
			want: "unknown action",
		},
		"missing action": {
			args: map[string]any{"url": "https://example.com"},
			want: "unknown action",
		},
		"neither url nor html": {
			args: map[string]any{"action": "markdown"},
			want: "url or html is required",
		},
		"scrape without selectors": {
			args: map[string]any{"action": "scrape", "url": "https://example.com"},
			want: "requires selectors",
		},
		"json without prompt or schema": {
			args: map[string]any{"action": "json", "url": "https://example.com"},
			want: "requires prompt or schema",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := invokeBrowser(t, srv, tc.args)
			if m["ok"] != false {
				t.Fatalf("expected ok=false, got %v", m)
			}
			msg, _ := m["error"].(string)
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("error = %q; want it to mention %q", msg, tc.want)
			}
		})
	}
}

// TestBrowserAcceptsHTMLInsteadOfURL: the endpoint takes either, so the tool
// must not insist on a url.
func TestBrowserAcceptsHTMLInsteadOfURL(t *testing.T) {
	srv, bodies := serveCapturing(t)
	m := invokeBrowser(t, srv, map[string]any{"action": "markdown", "html": "<div>hi</div>"})
	if m["ok"] != true {
		t.Fatalf("ok = %v (%v)", m["ok"], m["error"])
	}
	body := <-bodies
	if body["html"] != "<div>hi</div>" {
		t.Fatalf("html not forwarded: %v", body)
	}
	if _, has := m["url"]; has {
		t.Fatal("no url was given, so none should be reported")
	}
}

// TestBrowserForwardsLoadOptions: the shared page-load options reach the wire.
func TestBrowserForwardsLoadOptions(t *testing.T) {
	srv, bodies := serveCapturing(t)
	invokeBrowser(t, srv, map[string]any{
		"action":            "markdown",
		"url":               "https://example.com",
		"wait_until":        "networkidle2",
		"wait_for_selector": "#main",
		"timeout_ms":        45000,
		"user_agent":        "agentflow-test",
	})
	body := <-bodies
	gotoOpts, ok := body["gotoOptions"].(map[string]any)
	if !ok {
		t.Fatalf("gotoOptions missing: %v", body)
	}
	if gotoOpts["waitUntil"] != "networkidle2" || gotoOpts["timeout"] != float64(45000) {
		t.Fatalf("gotoOptions = %v", gotoOpts)
	}
	if body["waitForSelector"] != "#main" || body["userAgent"] != "agentflow-test" {
		t.Fatalf("body = %v", body)
	}
}

// TestBrowserScrapeForwarding: selectors arrive as []any from the Lua path and
// must become elements entries.
func TestBrowserScrapeForwarding(t *testing.T) {
	srv, bodies := serveCapturing(t)
	invokeBrowser(t, srv, map[string]any{
		"action":    "scrape",
		"url":       "https://example.com",
		"selectors": []any{"h1", "a"},
	})
	body := <-bodies
	els, ok := body["elements"].([]any)
	if !ok || len(els) != 2 {
		t.Fatalf("elements = %v", body["elements"])
	}
	if els[0].(map[string]any)["selector"] != "h1" {
		t.Fatalf("elements = %v", els)
	}
}

// TestBrowserJSONForwarding: prompt and schema reach response_format.
func TestBrowserJSONForwarding(t *testing.T) {
	srv, bodies := serveCapturing(t)
	invokeBrowser(t, srv, map[string]any{
		"action": "json",
		"url":    "https://example.com",
		"prompt": "list the products",
		"schema": map[string]any{"type": "object"},
	})
	body := <-bodies
	if body["prompt"] != "list the products" {
		t.Fatalf("prompt = %v", body["prompt"])
	}
	rf, ok := body["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_schema" {
		t.Fatalf("response_format = %v", body["response_format"])
	}
}

// longText builds a markdown result of n characters.
func longText(n int) string { return strings.Repeat("x", n) }

// TestBrowserMaxCharsDefault: an oversized page is cut to the default, and the
// true length is reported so a model can tell a short page from a clipped one.
// Nothing downstream truncates a tool result, so this bound is the only one.
func TestBrowserMaxCharsDefault(t *testing.T) {
	full := longText(maxCharsDefault + 5000)
	srv := serveResult(t, full)
	m := invokeBrowser(t, srv, map[string]any{"action": "markdown", "url": "https://example.com"})
	if m["chars"] != len(full) {
		t.Fatalf("chars = %v; want the untruncated length %d", m["chars"], len(full))
	}
	if m["truncated"] != true {
		t.Fatalf("expected truncated=true, got %v", m["truncated"])
	}
	if got := len(m["markdown"].(string)); got != maxCharsDefault {
		t.Fatalf("markdown length = %d; want %d", got, maxCharsDefault)
	}
}

// TestBrowserMaxCharsExplicit: an explicit cap is honored.
func TestBrowserMaxCharsExplicit(t *testing.T) {
	srv := serveResult(t, longText(1000))
	m := invokeBrowser(t, srv, map[string]any{
		"action": "markdown", "url": "https://example.com", "max_chars": 100,
	})
	if got := len(m["markdown"].(string)); got != 100 {
		t.Fatalf("markdown length = %d; want 100", got)
	}
	if m["truncated"] != true || m["chars"] != 1000 {
		t.Fatalf("chars = %v, truncated = %v", m["chars"], m["truncated"])
	}
}

// TestBrowserMaxCharsZeroMeansNoCap: 0 is an explicit "give me everything",
// distinct from omitting the argument (which takes the default).
func TestBrowserMaxCharsZeroMeansNoCap(t *testing.T) {
	full := longText(maxCharsDefault + 5000)
	srv := serveResult(t, full)
	m := invokeBrowser(t, srv, map[string]any{
		"action": "markdown", "url": "https://example.com", "max_chars": 0,
	})
	if got := len(m["markdown"].(string)); got != len(full) {
		t.Fatalf("markdown length = %d; want the full %d", got, len(full))
	}
	if _, cut := m["truncated"]; cut {
		t.Fatal("max_chars=0 must not truncate")
	}
}

// TestBrowserMaxCharsIsRuneSafe: cutting on a byte boundary inside a multi-byte
// character must not emit invalid UTF-8 into the provider request.
func TestBrowserMaxCharsIsRuneSafe(t *testing.T) {
	// Three-byte runes, so a cut at an odd offset lands mid-character.
	srv := serveResult(t, strings.Repeat("日", 200))
	m := invokeBrowser(t, srv, map[string]any{
		"action": "markdown", "url": "https://example.com", "max_chars": 100,
	})
	got, _ := m["markdown"].(string)
	if !utf8.ValidString(got) {
		t.Fatal("truncated text must be valid UTF-8")
	}
	if len(got) > 100 {
		t.Fatalf("length = %d; want at most 100", len(got))
	}
}

// TestBrowserCapsLinks: a nav-heavy page must not dump thousands of links into
// the model's context.
func TestBrowserCapsLinks(t *testing.T) {
	links := make([]string, maxLinks+250)
	for i := range links {
		links[i] = "https://example.com/" + string(rune('a'+i%26))
	}
	srv := serveResult(t, links)
	m := invokeBrowser(t, srv, map[string]any{"action": "links", "url": "https://example.com"})
	if m["count"] != len(links) {
		t.Fatalf("count = %v; want the true %d", m["count"], len(links))
	}
	if m["truncated"] != true {
		t.Fatalf("expected truncated=true, got %v", m["truncated"])
	}
	if got := len(m["links"].([]string)); got != maxLinks {
		t.Fatalf("links length = %d; want %d", got, maxLinks)
	}
}

// TestBrowserNumericArgsFromLua: numbers arrive as float64 on the Lua->JSON
// path, so argCount/argMaxChars must accept that shape.
func TestBrowserNumericArgsFromLua(t *testing.T) {
	srv, bodies := serveCapturing(t)
	m := invokeBrowser(t, srv, map[string]any{
		"action":     "markdown",
		"url":        "https://example.com",
		"max_chars":  float64(50),
		"timeout_ms": float64(30000),
	})
	body := <-bodies
	gotoOpts, _ := body["gotoOptions"].(map[string]any)
	if gotoOpts["timeout"] != float64(30000) {
		t.Fatalf("timeout_ms not forwarded: %v", body)
	}
	if m["ok"] != true {
		t.Fatalf("ok = %v (%v)", m["ok"], m["error"])
	}
}

// TestBrowserTransportErrorIsAnError: a request that fails at the transport
// level is a Go error, distinct from the correctable-argument result.
func TestBrowserTransportErrorIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"success":false,"errors":[{"code":1000,"message":"kaboom"}]}`)
	}))
	defer srv.Close()

	r := NewRegistry()
	RegisterBrowserBuiltins(r, browserClientFor(srv), nil)
	as := r.Expose([]string{"builtin:browser"}, config.ToolsPolicy{}, false)
	_, err := as.Invoke(context.Background(), "builtin:browser",
		map[string]any{"action": "markdown", "url": "https://example.com"})
	if err == nil {
		t.Fatal("a 500 must surface as an error")
	}
	if !strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("err = %v", err)
	}
}

// TestBrowserSchemaDeclaresAction: the tool is one schema with an action enum,
// and action is the only required parameter — url and html are alternatives.
func TestBrowserSchemaDeclaresAction(t *testing.T) {
	r := NewRegistry()
	RegisterBrowserBuiltins(r, nil, nil)
	spec, ok := r.tools["builtin:browser"]
	if !ok {
		t.Fatal("builtin:browser must be registered even when unconfigured")
	}
	props, _ := spec.Parameters["properties"].(map[string]any)
	action, _ := props["action"].(map[string]any)
	enum, _ := action["enum"].([]string)
	if len(enum) != len(browser.Actions()) {
		t.Fatalf("action enum = %v", enum)
	}
	required, _ := spec.Parameters["required"].([]string)
	if len(required) != 1 || required[0] != "action" {
		t.Fatalf("required = %v; want exactly [action]", required)
	}
	if !spec.Autonomous {
		t.Fatal("reading a page is a read-only, autonomous action")
	}
}
