package ghhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentflow/internal/core/router"
	"agentflow/internal/drivers/httpd"
)

// The package shipped with no tests, which is how an HMAC check that returned
// true for an empty secret went unnoticed.
const testSecret = "gh-webhook-secret"

type sink struct{ inbs []router.Inbound }

func (s *sink) Submit(in router.Inbound) { s.inbs = append(s.inbs, in) }

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func newDriver(t *testing.T, secret string) (*Driver, *sink) {
	t.Helper()
	s := &sink{}
	srv := httpd.New(":0", testLogger())
	return New("gh", "", "pm", secret, s, srv, testLogger()), s
}

// TestVerifySignature: the signature is the only thing separating a real GitHub
// delivery from anyone who found the path. Every rejection mode matters, and an
// empty secret must fail closed — it used to return true, which is how a channel
// built without one accepted unsigned events.
func TestVerifySignature(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	d, _ := newDriver(t, testSecret)
	valid := sign(testSecret, body)

	cases := []struct {
		name string
		sig  string
		want bool
	}{
		{"valid", valid, true},
		{"missing header", "", false},
		{"signed with another secret", sign("other", body), false},
		{"prefix stripped", strings.TrimPrefix(valid, "sha256="), false},
		{"not hex", "sha256=zzzz", false},
		{"truncated digest", valid[:len(valid)-2], false},
	}
	for _, c := range cases {
		if got := d.verifySignature(body, c.sig); got != c.want {
			t.Errorf("%s: verifySignature = %v; want %v", c.name, got, c.want)
		}
	}

	empty, _ := newDriver(t, "")
	if empty.verifySignature(body, sign("", body)) {
		t.Error("an empty secret must verify nothing, even a signature made with the empty key")
	}
	if empty.verifySignature(body, valid) {
		t.Error("an empty secret must not accept a signature made with a real one")
	}
}

// TestHandleRefusesAnUnsignedEvent: the refusal happens before parsing, and the
// event never reaches the router.
func TestHandleRefusesAnUnsignedEvent(t *testing.T) {
	d, s := newDriver(t, testSecret)
	rec := httptest.NewRecorder()
	d.handle(rec, httptest.NewRequest(http.MethodPost, "/hooks/github/",
		strings.NewReader(`{"action":"opened"}`)))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned event: got %d, want 401", rec.Code)
	}
	if len(s.inbs) != 0 {
		t.Fatalf("a rejected event reached the router: %d", len(s.inbs))
	}
}

// TestHandleAcceptsASignedEvent: a correctly signed delivery is acknowledged
// immediately and submitted as an agent-typed event carrying the decoded
// payload.
func TestHandleAcceptsASignedEvent(t *testing.T) {
	d, s := newDriver(t, testSecret)
	const body = `{"action":"opened","repository":{"full_name":"o/r"}}`
	req := httptest.NewRequest(http.MethodPost, "/hooks/github/", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(testSecret, []byte(body)))
	req.Header.Set("X-GitHub-Event", "pull_request")

	rec := httptest.NewRecorder()
	d.handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("signed event: got %d, want 200", rec.Code)
	}
	if len(s.inbs) != 1 {
		t.Fatalf("signed event not submitted: %d", len(s.inbs))
	}
	in := s.inbs[0]
	if in.Channel != "gh" || in.Agent != "pm" {
		t.Fatalf("inbound routed to %q/%q; want gh/pm", in.Channel, in.Agent)
	}
	if in.Message.Type != "agent" {
		t.Fatalf("message type = %q; want agent", in.Message.Type)
	}
	if got := in.Message.Payload["event"]; got != "pull_request" {
		t.Fatalf("payload event = %v; want pull_request", got)
	}
}

// TestHandleRejectsOtherMethodsAndBadJSON keeps the intake honest on the two
// paths that are not about the signature.
func TestHandleRejectsOtherMethodsAndBadJSON(t *testing.T) {
	d, _ := newDriver(t, testSecret)

	rec := httptest.NewRecorder()
	d.handle(rec, httptest.NewRequest(http.MethodGet, "/hooks/github/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: got %d, want 405", rec.Code)
	}

	const body = `not json`
	req := httptest.NewRequest(http.MethodPost, "/hooks/github/", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(testSecret, []byte(body)))
	rec = httptest.NewRecorder()
	d.handle(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: got %d, want 400", rec.Code)
	}
}

// TestDeliverAlwaysErrors: a GitHub event carries no chat target, so there is
// nothing to reply to. The channel is intake-only by design.
func TestDeliverAlwaysErrors(t *testing.T) {
	d, _ := newDriver(t, testSecret)
	if err := d.Deliver("", "hi", nil); err == nil {
		t.Fatal("Deliver must report that a ghhook channel cannot reply")
	}
}
