package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agentflow/internal/config"
)

// Response bodies below are the shapes Cloudflare documents, kept verbatim so
// the mapping is pinned against the published wire format rather than against
// what this package happens to emit.
const (
	markdownOK = `{"success":true,"result":"# Example Domain\n\nThis domain is for use in illustrative examples in documents."}`
	linksOK    = `{"success":true,"result":["https://example.com/","https://www.iana.org/domains/example"]}`
	scrapeOK   = `{"success":true,"result":[{"selector":"h1","results":[{"attributes":[],"height":39,"html":"Example Domain","left":100,"text":"Example Domain","top":133.4375,"width":600}]}]}`
	jsonOK     = `{"success":true,"result":{"products":[{"name":"R2","link":"https://developers.cloudflare.com/r2/"}]}}`
	treeOK     = `{"success":true,"result":{"accessibilityTree":{"role":"RootWebArea","name":"Example Domain","children":[{"role":"heading","name":"Example Domain","level":1}]}},"meta":{"status":200,"title":"Example Domain"}}`

	errorEnvelope = `{"success":false,"errors":[{"code":1000,"message":"Bad request"}]}`
)

// newTestClient points a client at a test server. The token is a literal, so
// the credential resolver returns it unchanged — the same path a deployment's
// ${CF_TOKEN} takes once the environment has resolved it.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c := Build(config.Browser{
		AccountID: "acct",
		APIToken:  "test-token",
		BaseURL:   srv.URL,
	}, &config.Resolver{}, nil)
	if c.Empty() {
		t.Fatal("client must be configured")
	}
	return c
}

// okServer answers every request with body and records the last request.
type recorded struct {
	mu     sync.Mutex
	path   string
	auth   string
	ctype  string
	body   map[string]any
	header http.Header
}

func (r *recorded) set(req *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(req.Body).Decode(&body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.path = req.URL.Path
	r.auth = req.Header.Get("Authorization")
	r.ctype = req.Header.Get("Content-Type")
	r.header = req.Header.Clone()
	r.body = body
}

func (r *recorded) get() (string, string, string, map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.path, r.auth, r.ctype, r.body
}

// okServer stands up a server answering with body and returns it plus the
// recorder. The recorder is mutex-guarded because the handler goroutine writes
// it while the test goroutine reads it; the HTTP round trip synchronises them
// in practice but not in a way the race detector can see through.
func okServer(t *testing.T, body string) (*httptest.Server, *recorded) {
	t.Helper()
	rec := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.set(r)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// captureBody runs one request against a throwaway server and returns the
// decoded request body it received.
func captureBody(t *testing.T, req Request) map[string]any {
	t.Helper()
	srv, rec := okServer(t, `{"success":true,"result":""}`)
	if _, err := newTestClient(t, srv).Do(context.Background(), req); err != nil {
		t.Fatalf("do: %v", err)
	}
	_, _, _, body := rec.get()
	return body
}

// TestMarkdownRequestShape pins the endpoint, the auth header and the body of
// the simplest action.
func TestMarkdownRequestShape(t *testing.T) {
	srv, rec := okServer(t, markdownOK)
	res, err := newTestClient(t, srv).Do(context.Background(),
		Request{Action: ActionMarkdown, URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	path, auth, ctype, body := rec.get()
	if path != "/accounts/acct/browser-rendering/markdown" {
		t.Fatalf("path = %q", path)
	}
	if auth != "Bearer test-token" {
		t.Fatalf("Authorization = %q", auth)
	}
	if ctype != "application/json" {
		t.Fatalf("Content-Type = %q", ctype)
	}
	if body["url"] != "https://example.com" {
		t.Fatalf("body = %v", body)
	}
	if !strings.Contains(res.Markdown, "Example Domain") {
		t.Fatalf("markdown = %q", res.Markdown)
	}
	if res.Action != ActionMarkdown {
		t.Fatalf("action = %q", res.Action)
	}
}

// TestActionPaths: each action maps to its REST segment, and the three
// deliberately unimplemented actions stay unimplemented. accessibility_tree is
// the one name whose case differs between the tool surface and the endpoint.
func TestActionPaths(t *testing.T) {
	want := map[Action]string{
		ActionMarkdown:          "markdown",
		ActionContent:           "content",
		ActionLinks:             "links",
		ActionScrape:            "scrape",
		ActionJSON:              "json",
		ActionAccessibilityTree: "accessibilityTree",
	}
	for action, segment := range want {
		got, ok := pathFor(action)
		if !ok {
			t.Fatalf("%s must be implemented", action)
		}
		if got != segment {
			t.Fatalf("pathFor(%s) = %q; want %q", action, got, segment)
		}
		if !Implemented(action) {
			t.Fatalf("Implemented(%s) = false", action)
		}
	}
	if len(Actions()) != len(want) {
		t.Fatalf("Actions() = %v; want %d entries", Actions(), len(want))
	}
	for _, absent := range []Action{"screenshot", "pdf", "crawl"} {
		if Implemented(absent) {
			t.Fatalf("%s is deliberately unimplemented; implementing it needs the "+
				"media-store and job-polling work described in the package doc", absent)
		}
		if _, err := (&Client{accountID: "a", token: "t", baseURL: "http://x", http: http.DefaultClient}).
			Do(context.Background(), Request{Action: absent, URL: "https://example.com"}); err == nil {
			t.Fatalf("%s must be rejected by Do", absent)
		}
	}
}

// TestLoadOptionsWiring: the shared page-load options land in the nested
// gotoOptions object under their wire names.
func TestLoadOptionsWiring(t *testing.T) {
	body := captureBody(t, Request{
		Action:          ActionMarkdown,
		URL:             "https://example.com",
		WaitUntil:       "networkidle2",
		WaitForSelector: "#main",
		TimeoutMS:       45000,
		ActionTimeoutMS: 120000,
		UserAgent:       "agentflow-test",
	})
	gotoOpts, ok := body["gotoOptions"].(map[string]any)
	if !ok {
		t.Fatalf("gotoOptions missing: %v", body)
	}
	if gotoOpts["waitUntil"] != "networkidle2" {
		t.Fatalf("gotoOptions = %v", gotoOpts)
	}
	if gotoOpts["timeout"] != float64(45000) {
		t.Fatalf("gotoOptions = %v", gotoOpts)
	}
	if body["waitForSelector"] != "#main" {
		t.Fatalf("waitForSelector = %v", body["waitForSelector"])
	}
	if body["actionTimeout"] != float64(120000) {
		t.Fatalf("actionTimeout = %v", body["actionTimeout"])
	}
	if body["userAgent"] != "agentflow-test" {
		t.Fatalf("userAgent = %v", body["userAgent"])
	}
}

// TestLoadOptionsOmittedWhenUnset: an unset option must be absent rather than
// sent as a zero value, or it would override Cloudflare's own default.
func TestLoadOptionsOmittedWhenUnset(t *testing.T) {
	body := captureBody(t, Request{Action: ActionMarkdown, URL: "https://example.com"})
	for _, key := range []string{"gotoOptions", "waitForSelector", "actionTimeout", "userAgent", "html", "elements", "prompt", "response_format"} {
		if _, present := body[key]; present {
			t.Fatalf("%s must be omitted when unset, got %v", key, body[key])
		}
	}
}

// TestScrapeSendsElements: the selectors array becomes one elements entry per
// selector, which is the shape the endpoint documents.
func TestScrapeSendsElements(t *testing.T) {
	body := captureBody(t, Request{
		Action:    ActionScrape,
		URL:       "https://example.com",
		Selectors: []string{"h1", "a"},
	})
	els, ok := body["elements"].([]any)
	if !ok {
		t.Fatalf("elements = %v", body["elements"])
	}
	if len(els) != 2 {
		t.Fatalf("elements = %v", els)
	}
	first, _ := els[0].(map[string]any)
	second, _ := els[1].(map[string]any)
	if first["selector"] != "h1" || second["selector"] != "a" {
		t.Fatalf("elements = %v", els)
	}
}

// TestJSONSendsResponseFormat: the schema is wrapped in
// {type: json_schema, json_schema: ...}, not sent at the top level.
func TestJSONSendsResponseFormat(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}}
	body := captureBody(t, Request{
		Action: ActionJSON,
		URL:    "https://example.com",
		Prompt: "list the products",
		Schema: schema,
	})
	if body["prompt"] != "list the products" {
		t.Fatalf("prompt = %v", body["prompt"])
	}
	rf, ok := body["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format = %v", body["response_format"])
	}
	if rf["type"] != "json_schema" {
		t.Fatalf("response_format.type = %v", rf["type"])
	}
	if _, ok := rf["json_schema"].(map[string]any); !ok {
		t.Fatalf("response_format.json_schema = %v", rf["json_schema"])
	}
}

// TestLinksResult: the links payload is a plain string array.
func TestLinksResult(t *testing.T) {
	srv, _ := okServer(t, linksOK)
	res, err := newTestClient(t, srv).Do(context.Background(),
		Request{Action: ActionLinks, URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Links) != 2 {
		t.Fatalf("links = %v", res.Links)
	}
	if res.Links[0] != "https://example.com/" {
		t.Fatalf("links = %v", res.Links)
	}
}

// TestNestedResultsPassThrough: the scrape, json and accessibility-tree
// payloads are carried through as decoded JSON, and the envelope's meta is
// lifted onto the result.
func TestNestedResultsPassThrough(t *testing.T) {
	t.Run("scrape", func(t *testing.T) {
		srv, _ := okServer(t, scrapeOK)
		res, err := newTestClient(t, srv).Do(context.Background(),
			Request{Action: ActionScrape, URL: "https://example.com", Selectors: []string{"h1"}})
		if err != nil {
			t.Fatal(err)
		}
		groups, ok := res.Elements.([]any)
		if !ok || len(groups) != 1 {
			t.Fatalf("elements = %#v", res.Elements)
		}
		group, _ := groups[0].(map[string]any)
		if group["selector"] != "h1" {
			t.Fatalf("group = %v", group)
		}
		hits, _ := group["results"].([]any)
		if len(hits) != 1 {
			t.Fatalf("results = %v", group["results"])
		}
		hit, _ := hits[0].(map[string]any)
		if hit["text"] != "Example Domain" || hit["width"] != float64(600) {
			t.Fatalf("hit = %v", hit)
		}
	})

	t.Run("json", func(t *testing.T) {
		srv, _ := okServer(t, jsonOK)
		res, err := newTestClient(t, srv).Do(context.Background(),
			Request{Action: ActionJSON, URL: "https://example.com", Prompt: "products"})
		if err != nil {
			t.Fatal(err)
		}
		obj, ok := res.Response.(map[string]any)
		if !ok {
			t.Fatalf("response = %#v", res.Response)
		}
		products, _ := obj["products"].([]any)
		if len(products) != 1 {
			t.Fatalf("products = %v", obj["products"])
		}
		first, _ := products[0].(map[string]any)
		if first["name"] != "R2" {
			t.Fatalf("product = %v", first)
		}
	})

	t.Run("accessibility_tree", func(t *testing.T) {
		srv, _ := okServer(t, treeOK)
		res, err := newTestClient(t, srv).Do(context.Background(),
			Request{Action: ActionAccessibilityTree, URL: "https://example.com"})
		if err != nil {
			t.Fatal(err)
		}
		obj, ok := res.Tree.(map[string]any)
		if !ok {
			t.Fatalf("tree = %#v", res.Tree)
		}
		if _, ok := obj["accessibilityTree"].(map[string]any); !ok {
			t.Fatalf("tree = %v", obj)
		}
		if res.Meta == nil || res.Meta.Status != 200 || res.Meta.Title != "Example Domain" {
			t.Fatalf("meta = %+v", res.Meta)
		}
	})
}

// TestUnexpectedResultShapeFails: a payload that is not the documented type is
// an error naming what arrived, not a silent zero value.
func TestUnexpectedResultShapeFails(t *testing.T) {
	srv, _ := okServer(t, `{"success":true,"result":["not","a","string"]}`)
	_, err := newTestClient(t, srv).Do(context.Background(),
		Request{Action: ActionMarkdown, URL: "https://example.com"})
	if err == nil {
		t.Fatal("an array where a string was documented must fail")
	}
	if !strings.Contains(err.Error(), "expected a string result") {
		t.Fatalf("err = %v", err)
	}
}

// TestAPIErrorEnvelope: a non-2xx carries Cloudflare's own code and message.
func TestAPIErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, errorEnvelope)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).Do(context.Background(),
		Request{Action: ActionMarkdown, URL: "https://example.com"})
	if err == nil {
		t.Fatal("a 400 must fail")
	}
	if !strings.Contains(err.Error(), "1000") || !strings.Contains(err.Error(), "Bad request") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("err must name the status: %v", err)
	}
}

// TestSuccessFalseWithOKStatus: a 200 can still carry success:false, and it is
// an error rather than an empty result.
func TestSuccessFalseWithOKStatus(t *testing.T) {
	srv, _ := okServer(t, errorEnvelope)
	_, err := newTestClient(t, srv).Do(context.Background(),
		Request{Action: ActionMarkdown, URL: "https://example.com"})
	if err == nil {
		t.Fatal("success:false must fail even with a 200")
	}
	if !strings.Contains(err.Error(), "Bad request") {
		t.Fatalf("err = %v", err)
	}
}

// TestRateLimitReportsRetryAfter: a 429 is the one error with an actionable
// header, so Retry-After must survive into the message. The driver never
// retries on its own.
func TestRateLimitReportsRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, errorEnvelope)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).Do(context.Background(),
		Request{Action: ActionMarkdown, URL: "https://example.com"})
	if err == nil {
		t.Fatal("a 429 must fail")
	}
	if !strings.Contains(err.Error(), "12") || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("err must report the retry window: %v", err)
	}
}

// TestRedactsToken is the regression test for credential leakage: the server
// reflects the Authorization header back inside its error body, which is the
// worst case — an error body that quotes the credential it rejected.
func TestRedactsToken(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"reflected in a 401 body": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"success":false,"errors":[{"code":1,"message":"token test-token rejected"}]}`)
		},
		"reflected in a 200 success:false body": func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"success":false,"errors":[{"code":1,"message":"token test-token rejected"}]}`)
		},
		"unparseable body quoting the raw header": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "upstream rejected Authorization: Bearer test-token")
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(handler)
			defer srv.Close()

			_, err := newTestClient(t, srv).Do(context.Background(),
				Request{Action: ActionMarkdown, URL: "https://example.com"})
			if err == nil {
				t.Fatal("expected a failure")
			}
			if strings.Contains(err.Error(), "test-token") {
				t.Fatalf("the API token leaked into an error: %v", err)
			}
			if !strings.Contains(err.Error(), "***") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// TestEmptyClient: an unconfigured client is empty and refuses to run, so the
// tool's honest-unavailable branch is the only way a call can end.
func TestEmptyClient(t *testing.T) {
	var nilClient *Client
	if !nilClient.Empty() {
		t.Fatal("a nil client is empty")
	}
	if _, err := nilClient.Do(context.Background(), Request{Action: ActionMarkdown, URL: "https://example.com"}); err == nil {
		t.Fatal("a nil client must refuse to run")
	}
	if !Build(config.Browser{}, &config.Resolver{}, nil).Empty() {
		t.Fatal("no account configured means an empty client")
	}
}

// TestBuildUnresolvableTokenWarns: a configured account whose token will not
// resolve degrades with a warning naming the credential, never a boot failure
// — and a warning that does not print the reference's value.
func TestBuildUnresolvableTokenWarns(t *testing.T) {
	cfg := config.Browser{AccountID: "acct", APIToken: "${MISSING_CF_TOKEN}"}

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	if c := Build(cfg, &config.Resolver{}, log); !c.Empty() {
		t.Fatal("an unresolvable token must leave the client empty")
	}
	out := buf.String()
	if !strings.Contains(out, "MISSING_CF_TOKEN") || !strings.Contains(out, "skipped") {
		t.Fatalf("the warning must name the unresolved credential: %s", out)
	}

	// Env resolution brings the client back.
	t.Setenv("MISSING_CF_TOKEN", "now-present")
	c := Build(cfg, &config.Resolver{}, nil)
	if c.Empty() {
		t.Fatal("an env-resolved token must build")
	}
	if c.token != "now-present" {
		t.Fatal("the resolved token must be the one used")
	}
}

// TestBuildDefaults: the base URL defaults to the Cloudflare API root and a
// trailing slash is trimmed so the path is not doubled.
func TestBuildDefaults(t *testing.T) {
	c := Build(config.Browser{AccountID: "acct", APIToken: "tok"}, &config.Resolver{}, nil)
	if c.Empty() {
		t.Fatal("a literal token must build")
	}
	if c.baseURL != defaultBaseURL {
		t.Fatalf("baseURL = %q", c.baseURL)
	}
	c = Build(config.Browser{AccountID: "acct", APIToken: "tok", BaseURL: "http://localhost:1234/"}, &config.Resolver{}, nil)
	if c.baseURL != "http://localhost:1234" {
		t.Fatalf("a trailing slash must be trimmed, got %q", c.baseURL)
	}
}
