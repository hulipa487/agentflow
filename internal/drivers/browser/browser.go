// Package browser implements the Cloudflare Browser Run (formerly Browser
// Rendering) Quick Actions backend for the builtin:browser tool.
//
// Quick Actions are stateless browser tasks behind one REST endpoint:
//
//	POST <base>/accounts/<accountId>/browser-rendering/<action>
//	Authorization: Bearer <api token>
//	Content-Type: application/json
//
// The token needs the "Browser Rendering - Edit" permission. A successful call
// answers {"success":true,"result":...}; a failure answers
// {"success":false,"errors":[{"code":...,"message":...}]}. Both are handled
// here.
//
// Only the actions that answer with JSON are implemented: markdown, content,
// links, scrape, json and accessibility_tree. Three actions are deliberately
// absent rather than overlooked:
//
//   - screenshot and pdf return raw bytes. A tool result is JSON that goes
//     straight into the model's context — nothing in the engine truncates it,
//     and the loop's token budgeter does not count tool results at all — so
//     bytes must go to the media store and come back as a handle instead.
//     That is separate plumbing, not a different request shape.
//   - crawl is asynchronous: the POST returns a job id and the result is
//     polled. A blocking poll is not viable here; tool calls run on an
//     8-worker pool, so a long-held call starves every other agent's work.
//
// Keeping them out rather than half-implementing them is the same choice the
// search driver makes: each package covers what it can answer honestly and
// returns an explicit error for the rest.
package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"agentflow/internal/config"
)

// defaultBaseURL is the Cloudflare API root. The account-scoped
// browser-rendering path is appended to it.
const defaultBaseURL = "https://api.cloudflare.com/client/v4"

// maxBodyBytes bounds one response read. This is double the search driver's
// 16 MiB because a fully rendered page is far larger than a search result, but
// it stays a bound rather than a buffer the server can grow without limit.
const maxBodyBytes = 32 << 20

// Action names an implemented Quick Action. These are the tool-facing names;
// pathFor maps them to the REST segments, which differ in case for one of
// them.
type Action string

const (
	ActionMarkdown          Action = "markdown"
	ActionContent           Action = "content"
	ActionLinks             Action = "links"
	ActionScrape            Action = "scrape"
	ActionJSON              Action = "json"
	ActionAccessibilityTree Action = "accessibility_tree"
)

// Actions returns the implemented action names, for tool descriptions.
func Actions() []string {
	return []string{
		string(ActionMarkdown),
		string(ActionContent),
		string(ActionLinks),
		string(ActionScrape),
		string(ActionJSON),
		string(ActionAccessibilityTree),
	}
}

// Implemented reports whether a is one of the actions this package supports.
func Implemented(a Action) bool {
	_, ok := pathFor(a)
	return ok
}

// pathFor maps a tool-facing action name to its REST path segment.
//
// The two differ only for accessibility_tree: the tool surface is snake_case
// like the rest of agentflow's tool params, while the endpoint is camelCase.
//
// The segment is "browser-rendering" even though the product is now called
// Browser Run: Cloudflare's API reference and every sibling endpoint use
// browser-rendering, and the rename explicitly does not rename API paths. The
// one Quick Actions guide page that shows "browser-run/accessibilityTree" is
// inconsistent with the rest of Cloudflare's own documentation, so this
// follows the reference. If that page turns out to be right, this is the
// single place to change.
func pathFor(a Action) (string, bool) {
	switch a {
	case ActionMarkdown:
		return "markdown", true
	case ActionContent:
		return "content", true
	case ActionLinks:
		return "links", true
	case ActionScrape:
		return "scrape", true
	case ActionJSON:
		return "json", true
	case ActionAccessibilityTree:
		return "accessibilityTree", true
	}
	return "", false
}

// Request is one Quick Action call. URL or HTML selects the page; every other
// field is optional and omitted from the request body when unset, so
// Cloudflare's own defaults apply rather than being overridden with a zero.
type Request struct {
	Action Action
	URL    string // page to load; exactly one of URL or HTML
	HTML   string // raw HTML to render instead of fetching a URL

	// Page-load control, shared by every action.
	WaitUntil           string   // gotoOptions.waitUntil: load | domcontentloaded | networkidle0 | networkidle2
	WaitForSelector     string   // waitForSelector
	TimeoutMS           int      // gotoOptions.timeout, max 60000
	ActionTimeoutMS     int      // actionTimeout, max 300000
	UserAgent           string   // userAgent
	RejectResourceTypes []string // rejectResourceTypes, e.g. ["image"]

	// scrape only: CSS selectors to extract, each becoming one elements entry.
	Selectors []string

	// json only: the natural-language prompt and/or the JSON Schema the
	// extracted data must match. At least one is required by the endpoint.
	Prompt string
	Schema map[string]any
}

// Result is one Quick Action response. Exactly one payload field is populated,
// chosen by Action.
//
// Three payloads are carried as decoded JSON rather than as Go structs: the
// accessibility tree is recursive, the scrape result nests a group per
// selector, and the json result is whatever the caller's schema asked for.
// Pinning those to structs here would silently drop any field this package
// does not know about, which is the wrong failure for a pass-through the
// caller is better placed to interpret.
type Result struct {
	Action Action `json:"action"`

	Markdown string   `json:"markdown,omitempty"` // markdown
	Content  string   `json:"content,omitempty"`  // content: the rendered HTML
	Links    []string `json:"links,omitempty"`    // links
	Elements any      `json:"elements,omitempty"` // scrape
	Response any      `json:"response,omitempty"` // json
	Tree     any      `json:"tree,omitempty"`     // accessibility_tree

	Meta       *Meta `json:"meta,omitempty"`
	TimeCostMs int64 `json:"time_cost_ms,omitempty"`
}

// Meta is the envelope metadata Cloudflare returns alongside some results (the
// accessibility tree carries it, most others do not).
type Meta struct {
	Status int    `json:"status,omitempty"`
	Title  string `json:"title,omitempty"`
}

// Client is a configured Browser Run account. A nil Client, or one missing an
// account or token, is empty: the tool then reports honest-unavailable rather
// than failing.
type Client struct {
	accountID string
	token     string
	baseURL   string
	http      *http.Client
}

// Empty reports whether no usable account is configured. It is nil-safe so the
// tool can ask an unconfigured client the same question it asks a configured
// one.
func (c *Client) Empty() bool {
	return c == nil || c.accountID == "" || c.token == ""
}

// Build constructs the client from config. With no account configured it
// returns nil and says nothing: the browser tool is opt-in, so an absent
// block is not a misconfiguration to warn about — unlike a search engine,
// which only exists because somebody wrote one.
//
// A configured account whose api_token will not resolve degrades the same way
// but loudly, naming the credential through config.CredentialName so the
// unresolved reference is identifiable without its value ever being printed.
// This is the honest-degradation rule the search engines follow: a credential
// missing at runtime skips the capability, it never fails the boot. log may be
// nil.
func Build(cfg config.Browser, res *config.Resolver, log *slog.Logger) *Client {
	if cfg.AccountID == "" {
		return nil
	}
	token := cfg.APIToken
	if token == "" {
		// Validation rejects this pairing, so reaching it means Build was
		// handed an unvalidated config; degrade rather than panic.
		warnSkip(log, "")
		return nil
	}
	if res != nil {
		v, ok := res.Resolve(context.Background(), token)
		if !ok {
			warnSkip(log, token)
			return nil
		}
		token = v
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	return &Client{
		accountID: cfg.AccountID,
		token:     token,
		baseURL:   strings.TrimRight(base, "/"),
		http:      &http.Client{Timeout: cfg.TimeoutD()},
	}
}

func warnSkip(log *slog.Logger, raw string) {
	if log == nil {
		return
	}
	log.Warn("browser skipped: unresolved credential",
		"credential", config.CredentialName(raw))
}

// Do runs one Quick Action. A non-2xx response becomes an error carrying
// Cloudflare's own code and message; the token is redacted from every error
// this returns, including transport errors.
func (c *Client) Do(ctx context.Context, req Request) (*Result, error) {
	if c.Empty() {
		return nil, fmt.Errorf("browser: no account is configured")
	}
	seg, ok := pathFor(req.Action)
	if !ok {
		return nil, fmt.Errorf("browser: unsupported action %q", req.Action)
	}
	body, err := json.Marshal(buildBody(req))
	if err != nil {
		return nil, fmt.Errorf("browser %s: marshal request: %w", req.Action, err)
	}
	endpoint := fmt.Sprintf("%s/accounts/%s/browser-rendering/%s", c.baseURL, c.accountID, seg)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, c.redact(fmt.Errorf("browser %s: build request: %w", req.Action, err))
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+c.token)

	start := time.Now()
	resp, err := c.http.Do(hreq)
	if err != nil {
		return nil, c.redact(fmt.Errorf("browser %s: %w", req.Action, err))
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, c.redact(fmt.Errorf("browser %s: read response: %w", req.Action, err))
	}
	elapsed := time.Since(start).Milliseconds()

	if resp.StatusCode != http.StatusOK {
		return nil, c.redact(statusError(req.Action, resp, payload))
	}

	var env struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
		Meta    *Meta           `json:"meta"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("browser %s: decode response: %w", req.Action, err)
	}
	if !env.Success {
		// A 200 can still carry success:false. Redacted like every other error
		// path: the fallback echoes the raw body, which is exactly the case
		// where a reflected token could ride along.
		return nil, c.redact(fmt.Errorf("browser %s: %s", req.Action, envelopeMessage(env.Errors, payload)))
	}

	var raw any
	if err := json.Unmarshal(env.Result, &raw); err != nil {
		return nil, fmt.Errorf("browser %s: decode result: %w", req.Action, err)
	}
	out := &Result{Action: req.Action, Meta: env.Meta, TimeCostMs: elapsed}
	switch req.Action {
	case ActionMarkdown:
		s, ok := raw.(string)
		if !ok {
			return nil, c.redact(fmt.Errorf("browser markdown: expected a string result, got %T", raw))
		}
		out.Markdown = s
	case ActionContent:
		s, ok := raw.(string)
		if !ok {
			return nil, c.redact(fmt.Errorf("browser content: expected a string result, got %T", raw))
		}
		out.Content = s
	case ActionLinks:
		list, ok := raw.([]any)
		if !ok {
			return nil, c.redact(fmt.Errorf("browser links: expected an array result, got %T", raw))
		}
		out.Links = make([]string, 0, len(list))
		for _, item := range list {
			// Non-strings are skipped rather than failing the call: the links
			// endpoint is documented to return strings, and one odd entry is
			// not worth discarding a page's worth of usable links over.
			if s, ok := item.(string); ok {
				out.Links = append(out.Links, s)
			}
		}
	case ActionScrape:
		out.Elements = raw
	case ActionJSON:
		out.Response = raw
	case ActionAccessibilityTree:
		out.Tree = raw
	}
	return out, nil
}

// statusError renders Cloudflare's error envelope for a non-2xx response.
//
// A 429 is a rate limit, so Retry-After is the actionable part and is appended
// rather than dropped. This driver never retries on its own: no driver in this
// repo does, and a silent retry would move the rate limit rather than respect
// it.
func statusError(action Action, resp *http.Response, payload []byte) error {
	var env struct {
		Errors []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(payload, &env)
	msg := envelopeMessage(env.Errors, payload)
	if resp.StatusCode == http.StatusTooManyRequests {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			msg += fmt.Sprintf(" (rate limited; retry after %s)", ra)
		} else {
			msg += " (rate limited)"
		}
	}
	return fmt.Errorf("browser %s: status %d: %s", action, resp.StatusCode, msg)
}

// envelopeMessage renders the errors array shared by the non-2xx path and a
// 200 carrying success:false, falling back to the raw body when the envelope
// is absent or empty (an error page from a proxy, say).
func envelopeMessage(errs []struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}, payload []byte) string {
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, fmt.Sprintf("%d %s", e.Code, e.Message))
	}
	if len(parts) == 0 {
		return truncate(payload, 512)
	}
	return strings.Join(parts, "; ")
}

// redact removes the API token from an error before it can reach a log or the
// model. A transport error can embed the full request URL and a reflected
// error body can quote the Authorization header back, so both paths are run
// through this.
func (c *Client) redact(err error) error {
	if err == nil || c.token == "" {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), c.token, "***"))
}

// wireRequest is the Quick Action request body. Everything optional is
// omitempty so an unset field is absent from the JSON rather than sent as a
// zero value that would override Cloudflare's own default.
type wireRequest struct {
	URL  string `json:"url,omitempty"`
	HTML string `json:"html,omitempty"`

	GotoOptions         *wireGoto `json:"gotoOptions,omitempty"`
	WaitForSelector     string    `json:"waitForSelector,omitempty"`
	ActionTimeout       int       `json:"actionTimeout,omitempty"`
	UserAgent           string    `json:"userAgent,omitempty"`
	RejectResourceTypes []string  `json:"rejectResourceTypes,omitempty"`

	Elements []wireElement `json:"elements,omitempty"`

	Prompt         string      `json:"prompt,omitempty"`
	ResponseFormat *wireFormat `json:"response_format,omitempty"`
}

type wireGoto struct {
	WaitUntil string `json:"waitUntil,omitempty"`
	Timeout   int    `json:"timeout,omitempty"`
}

type wireElement struct {
	Selector string `json:"selector"`
}

// wireFormat is the /json response_format wrapper. The schema goes under
// json_schema, not at the top level.
type wireFormat struct {
	Type       string         `json:"type"`
	JSONSchema map[string]any `json:"json_schema"`
}

// buildBody translates a Request into the wire body.
func buildBody(req Request) *wireRequest {
	w := &wireRequest{
		URL:                 req.URL,
		HTML:                req.HTML,
		WaitForSelector:     req.WaitForSelector,
		ActionTimeout:       req.ActionTimeoutMS,
		UserAgent:           req.UserAgent,
		RejectResourceTypes: req.RejectResourceTypes,
		Prompt:              req.Prompt,
	}
	// gotoOptions is a nested object, so it is only worth sending when one of
	// its fields is actually set.
	if req.WaitUntil != "" || req.TimeoutMS > 0 {
		w.GotoOptions = &wireGoto{WaitUntil: req.WaitUntil, Timeout: req.TimeoutMS}
	}
	for _, s := range req.Selectors {
		w.Elements = append(w.Elements, wireElement{Selector: s})
	}
	if len(req.Schema) > 0 {
		w.ResponseFormat = &wireFormat{Type: "json_schema", JSONSchema: req.Schema}
	}
	return w
}

// truncate bounds a response body echoed into an error message, dropping a
// partial multi-byte rune rather than emitting invalid UTF-8.
func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		s = s[:n]
	}
	return strings.ToValidUTF8(s, "")
}
