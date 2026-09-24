package webhook

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/core/media"
	"agentflow/internal/core/netguard"
	"agentflow/internal/drivers/httpd"
)

// loopbackPolicy admits the addresses httptest binds, which the outbound guard
// refuses by default. Tests that exercise callback delivery need it; tests that
// check the guard itself use the zero Policy.
func loopbackPolicy() netguard.Policy { return netguard.Policy{AllowPrivate: true} }

// newWebhookOpts is newWebhook with explicit driver options.
func newWebhookOpts(t *testing.T, opts Options) (*httptest.Server, *whSink, *Driver) {
	t.Helper()
	sink := &whSink{}
	srv := httpd.New(":0", discardLog())
	d := New("wh", "", "bot", sink, srv, nil, media.Policy{}, opts, loopbackPolicy(), discardLog())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, sink, d
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// A reply slower than the configured sync timeout is lost in sync mode (504)
// but still delivered in async mode via result polling.
func TestWebhookAsyncSlowReplyViaPoll(t *testing.T) {
	ts, sink, d := newWebhookOpts(t, Options{Async: true, Timeout: 150 * time.Millisecond})

	resp := postJSON(t, ts.URL+"/webhook/", map[string]any{"from": "alice", "text": "slow"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("async POST status = %d, want 202", resp.StatusCode)
	}
	var ack struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil || ack.ID == "" {
		t.Fatalf("bad 202 body: %v", err)
	}

	// The reply lands well past the sync timeout — async mode must not cut it off.
	time.Sleep(300 * time.Millisecond)
	waitInbound(t, sink, 1)
	if err := d.Deliver(ack.ID, "late answer", nil); err != nil {
		t.Fatal(err)
	}

	r, err := http.Get(ts.URL + "/webhook/result/" + ack.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusOK || string(b) != "late answer" {
		t.Fatalf("poll: status %d body %q", r.StatusCode, b)
	}
	// Consumed: a second poll is a 404.
	r2, err := http.Get(ts.URL + "/webhook/result/" + ack.ID)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusNotFound {
		t.Fatalf("second poll status = %d, want 404", r2.StatusCode)
	}
}

// While the agent is still working, the poll endpoint reports pending.
func TestWebhookAsyncPollPending(t *testing.T) {
	ts, sink, d := newWebhookOpts(t, Options{Async: true})

	resp := postJSON(t, ts.URL+"/webhook/", map[string]any{"from": "a", "text": "hi"})
	var ack struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ack)
	resp.Body.Close()

	r, err := http.Get(ts.URL + "/webhook/result/" + ack.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("pending poll status = %d, want 202", r.StatusCode)
	}
	// Cleanup: resolve the job so nothing leaks past the test.
	waitInbound(t, sink, 1)
	_ = d.Deliver(ack.ID, "ok", nil)
}

// A caller-supplied callback_url receives the reply as a POST.
func TestWebhookAsyncCallback(t *testing.T) {
	var mu sync.Mutex
	var got []byte
	cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got, _ = io.ReadAll(r.Body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer cb.Close()

	ts, sink, d := newWebhookOpts(t, Options{Async: true})
	resp := postJSON(t, ts.URL+"/webhook/", map[string]any{"from": "a", "text": "hi", "callback_url": cb.URL})
	var ack struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ack)
	resp.Body.Close()

	waitInbound(t, sink, 1)
	if err := d.Deliver(ack.ID, "callback answer", nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if string(got) != "callback answer" {
		t.Fatalf("callback body %q", got)
	}
}

// A callback_url aimed inside the host's own network is refused by the outbound
// guard. callback_url is caller-supplied and the channel is unauthenticated, so
// without this any caller who could reach the webhook could have the runtime
// POST an agent's reply to an internal service or a cloud metadata endpoint.
func TestWebhookCallbackCannotBePointedInward(t *testing.T) {
	var mu sync.Mutex
	var got []byte
	cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got, _ = io.ReadAll(r.Body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer cb.Close()

	// The zero Policy guards — this is what a deployment gets unless it opts in
	// with net.http.allow_private.
	sink := &whSink{}
	srv := httpd.New(":0", discardLog())
	d := New("wh", "", "bot", sink, srv, nil, media.Policy{}, Options{Async: true}, netguard.Policy{}, discardLog())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := postJSON(t, ts.URL+"/webhook/", map[string]any{"from": "a", "text": "hi", "callback_url": cb.URL})
	var ack struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ack)
	resp.Body.Close()

	waitInbound(t, sink, 1)
	if err := d.Deliver(ack.ID, "must not escape", nil); err != nil {
		t.Fatal(err)
	}

	// Give the callback every chance to arrive; it must not. postCallback logs
	// and returns on failure, so the reply stays pollable rather than erroring.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("the guard let a callback through to a private address: %q", got)
	}
}

// A non-http(s) callback_url is rejected up front.
func TestWebhookAsyncCallbackSchemeRejected(t *testing.T) {
	ts, _, _ := newWebhookOpts(t, Options{Async: true})
	resp := postJSON(t, ts.URL+"/webhook/", map[string]any{"from": "a", "text": "hi", "callback_url": "file:///etc/passwd"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// Sync mode honors the configured timeout: reply within it → 200; no reply → 504.
func TestWebhookSyncTimeoutConfigurable(t *testing.T) {
	ts, sink, d := newWebhookOpts(t, Options{Timeout: 200 * time.Millisecond})

	// Under the timeout: reply arrives, 200 + text.
	done := make(chan *http.Response, 1)
	go func() { done <- postJSON(t, ts.URL+"/webhook/", map[string]any{"from": "a", "text": "fast"}) }()
	waitInbound(t, sink, 1)
	if err := d.Deliver(sink.inbs[0].Message.ReplyTo, "quick", nil); err != nil {
		t.Fatal(err)
	}
	resp := <-done
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "quick" {
		t.Fatalf("sync fast: status %d body %q", resp.StatusCode, b)
	}

	// Over the timeout: 504.
	resp2 := postJSON(t, ts.URL+"/webhook/", map[string]any{"from": "a", "text": "slow"})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("sync timeout: status %d, want 504", resp2.StatusCode)
	}
}

// The default timeout is 55s when the channel sets none (backward compatible).
func TestWebhookDefaultTimeout(t *testing.T) {
	_, _, d := newWebhookOpts(t, Options{})
	if d.opts.Timeout != defaultTimeout {
		t.Fatalf("default timeout = %v, want %v", d.opts.Timeout, defaultTimeout)
	}
}

// Guard: a GET to the bare webhook path (not a result poll) is a 404, and the
// plain sync request shape is unchanged by the new options.
func TestWebhookGetNotFound(t *testing.T) {
	ts, _, _ := newWebhookOpts(t, Options{})
	r, err := http.Get(ts.URL + "/webhook/")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /webhook/ status = %d, want 404", r.StatusCode)
	}
	if strings.Contains(r.Header.Get("Content-Type"), "json") {
		t.Fatal("404 should be plain")
	}
}
