package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// echoIn is the input schema of the test server's one tool.
type echoIn struct {
	Text string `json:"text"`
}

// headerRec records the first request's headers. The concurrent map read/write
// is what the mutex is for: the Streamable HTTP handler serves the POST and
// the event stream on separate goroutines.
type headerRec struct {
	mu sync.Mutex
	h  http.Header
}

func (r *headerRec) set(h http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.h == nil {
		r.h = h.Clone()
	}
}

func (r *headerRec) get() http.Header {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.h
}

// newHTTPServer stands up a real MCP server over Streamable HTTP, in
// process, using the SDK's own server side. The two ends are therefore
// exercised against each other rather than against a hand-rolled fake, so a
// transport or protocol mismatch fails here instead of in a deployment.
func newHTTPServer(t *testing.T, rec *headerRec) *httptest.Server {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test-server", Version: "1.0"}, nil)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "echo", Description: "echo the text back"},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, in echoIn) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "echo:" + in.Text}},
			}, nil, nil
		})

	h := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec != nil {
			rec.set(r.Header)
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestHTTPClientHandshakeAndListTools: the connection completes and the tool
// arrives namespaced, with its schema intact — the same contract the stdio
// transport is held to, over a transport the runtime used to refuse.
func TestHTTPClientHandshakeAndListTools(t *testing.T) {
	ts := newHTTPServer(t, nil)

	c, err := NewHTTPClient("http", ts.URL, "", discardLog())
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	got, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(got) != 1 || got[0].Name != "http/echo" {
		t.Fatalf("tools = %+v; want exactly one named http/echo", got)
	}
	// The schema has to survive the hop: a tool registered with no parameters
	// is a tool the model cannot call correctly.
	props, ok := got[0].Parameters["properties"].(map[string]any)
	if !ok || props["text"] == nil {
		t.Fatalf("the echo tool lost its schema: %#v", got[0].Parameters)
	}
	if got[0].Description == "" {
		t.Error("the echo tool lost its description")
	}
}

// TestHTTPClientInvokesTool: a call round-trips and lands in the same
// {"ok", "text"} shape every other tool result uses.
func TestHTTPClientInvokesTool(t *testing.T) {
	ts := newHTTPServer(t, nil)

	c, err := NewHTTPClient("http", ts.URL, "", discardLog())
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	got, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("tools = %+v", got)
	}

	res, err := got[0].Invoke(context.Background(), map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	m, ok := res.(map[string]any)
	if !ok || m["ok"] != true || m["text"] != "echo:hi" {
		t.Fatalf("invoke returned %#v; want ok with text echo:hi", res)
	}
}

// TestHTTPClientSendsBearerToken: the configured token reaches the server. The
// transport has no header field of its own, so this pins the transport
// wrapper that carries it.
func TestHTTPClientSendsBearerToken(t *testing.T) {
	var rec headerRec
	ts := newHTTPServer(t, &rec)

	c, err := NewHTTPClient("http", ts.URL, "s3cret", discardLog())
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if got := rec.get().Get("Authorization"); got != "Bearer s3cret" {
		t.Fatalf("Authorization = %q; want %q", got, "Bearer s3cret")
	}
}

// TestHTTPClientOmitsTokenWhenUnset: an endpoint that needs no credential must
// not be sent an empty bearer, which some gateways reject outright.
func TestHTTPClientOmitsTokenWhenUnset(t *testing.T) {
	var rec headerRec
	ts := newHTTPServer(t, &rec)

	c, err := NewHTTPClient("http", ts.URL, "", discardLog())
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if got := rec.get().Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q; want it absent when no token is configured", got)
	}
}

// TestHTTPClientFailsOnAnUnreachableEndpoint: a dead endpoint must surface as an
// error the boot can skip, not as a hang.
func TestHTTPClientFailsOnAnUnreachableEndpoint(t *testing.T) {
	if _, err := NewHTTPClient("nope", "http://127.0.0.1:1/mcp", "", discardLog()); err == nil {
		t.Fatal("connecting to an unreachable endpoint must fail")
	}
}

func TestHTTPClientRejectsAMalformedEndpoint(t *testing.T) {
	_, err := NewHTTPClient("nope", "://not a url", "", discardLog())
	if err == nil {
		t.Fatal("a malformed endpoint must fail")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error %q does not name the server", err)
	}
}

func TestHTTPClientAfterCloseFails(t *testing.T) {
	ts := newHTTPServer(t, nil)

	c, err := NewHTTPClient("http", ts.URL, "", discardLog())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if _, err := c.ListTools(context.Background()); err == nil {
		t.Fatal("a call after Close must fail rather than block or panic")
	}
}
