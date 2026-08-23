package webhook

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentflow/internal/core/media"
	"agentflow/internal/core/router"
	"agentflow/internal/drivers/httpd"
)

type whSink struct{ inbs []router.Inbound }

func (s *whSink) Submit(in router.Inbound) { s.inbs = append(s.inbs, in) }

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newWebhook builds a driver on a shared httpd server and returns the server,
// sink, store, and driver for assertions.
func newWebhook(t *testing.T, pol media.Policy) (*httptest.Server, *whSink, *media.Store, *Driver) {
	t.Helper()
	store, err := media.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sink := &whSink{}
	srv := httpd.New(":0", discardLog())
	d := New("wh", "", "bot", sink, srv, store, pol, discardLog())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, sink, store, d
}

// unblock replies to every landed inbound so the parked HTTP handlers finish
// and ts.Close() in cleanup doesn't wait out the 55s timeout.
func unblock(d *Driver, sink *whSink) {
	for _, in := range sink.inbs {
		_ = d.Deliver(in.Message.ReplyTo, "ok", nil)
	}
}

// post fires the request in the background: the handler parks for up to 55s
// waiting for the agent reply, which these tests never send. The inbound is
// submitted to the sink synchronously before that wait, so assertions poll
// the sink instead of the response.
func post(t *testing.T, url string, body any) {
	t.Helper()
	b, _ := json.Marshal(body)
	go func() {
		resp, err := http.Post(url+"/webhook/", "application/json", bytes.NewReader(b))
		if err == nil {
			resp.Body.Close()
		}
	}()
}

// waitInbound polls the sink until n inbounds land (or times out).
func waitInbound(t *testing.T, sink *whSink, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(sink.inbs) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d inbounds, got %d", n, len(sink.inbs))
}

func TestWebhookAttachmentsStoredAsParts(t *testing.T) {
	ts, sink, store, d := newWebhook(t, media.Policy{Allow: []string{"image/*"}})
	defer unblock(d, sink)
	png := base64.StdEncoding.EncodeToString([]byte("pngbytes"))

	post(t, ts.URL, map[string]any{
		"from": "alice",
		"text": "look",
		"attachments": []map[string]any{
			{"mime": "image/png", "name": "a.png", "data": png},
		},
	})

	waitInbound(t, sink, 1)
	msg := sink.inbs[0].Message
	if msg.Text != "look" {
		t.Fatalf("text %q", msg.Text)
	}
	if len(msg.Attachments) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(msg.Attachments))
	}
	att := msg.Attachments[0]
	if att.Type != "image" || att.MIME != "image/png" || att.Name != "a.png" {
		t.Fatalf("bad part: %+v", att)
	}
	b, err := store.ReadAll(att.Handle, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "pngbytes" {
		t.Fatalf("stored content %q", b)
	}
}

func TestWebhookAttachmentPolicyDrops(t *testing.T) {
	ts, sink, _, d := newWebhook(t, media.Policy{Allow: []string{"image/*"}})
	defer unblock(d, sink)
	pdf := base64.StdEncoding.EncodeToString([]byte("%PDF-fake"))

	post(t, ts.URL, map[string]any{
		"from": "alice",
		"text": "doc",
		"attachments": []map[string]any{
			{"mime": "application/pdf", "name": "x.pdf", "data": pdf},
		},
	})

	waitInbound(t, sink, 1)
	if len(sink.inbs[0].Message.Attachments) != 0 {
		t.Fatal("pdf must be dropped under image-only policy")
	}
}

func TestWebhookMediaRepliesRejected(t *testing.T) {
	// Deliver with attachments must error, never silently drop.
	srv := httpd.New(":0", discardLog())
	New("wh", "", "bot", &whSink{}, srv, nil, media.Policy{}, discardLog())
	err := (&Driver{name: "wh", pending: map[string]chan string{}}).Deliver("wh-1", "hi", []media.Part{{Type: "image", Data: "xx"}})
	if err == nil || !strings.Contains(err.Error(), "does not support media replies") {
		t.Fatalf("want media-replies error, got %v", err)
	}
}

// Guard: the plain text-only request shape is unchanged.
func TestWebhookPlainTextOnly(t *testing.T) {
	ts, sink, _, d := newWebhook(t, media.Policy{Allow: []string{"image/*"}})
	defer unblock(d, sink)
	post(t, ts.URL, map[string]any{"from": "bob", "text": "hi"})
	waitInbound(t, sink, 1)
	if sink.inbs[0].Message.Text != "hi" || len(sink.inbs[0].Message.Attachments) != 0 {
		t.Fatalf("plain request changed shape: %+v", sink.inbs)
	}
}

