package telegram

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"agentflow/internal/core/media"
	"agentflow/internal/core/router"
	"agentflow/internal/drivers/httpd"
)

var secretTokenShape = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

// TestNewGeneratesWebhookSecret: webhook/auto mode with no configured
// secret_token gets a random per-boot one (secure by default), a configured
// one is kept, and polling mode generates none.
func TestNewGeneratesWebhookSecret(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httpd.New("127.0.0.1:0", log)
	sink := &captureSink{}

	gen1 := New("tg1", "TEST", "main", "webhook", nil, "/webhook/tg1/", "https://example.com", "", sink, srv, nil, media.Policy{}, log)
	if !secretTokenShape.MatchString(gen1.secretToken) {
		t.Fatalf("generated secret has the wrong shape: %q", gen1.secretToken)
	}
	gen2 := New("tg2", "TEST", "main", "auto", nil, "/webhook/tg2/", "https://example.com", "", sink, srv, nil, media.Policy{}, log)
	if gen1.secretToken == gen2.secretToken {
		t.Fatal("per-boot secrets must differ across channel instances")
	}

	keep := New("tg3", "TEST", "main", "webhook", nil, "/webhook/tg3/", "https://example.com", "my-configured-secret", sink, srv, nil, media.Policy{}, log)
	if keep.secretToken != "my-configured-secret" {
		t.Fatalf("a configured secret must be kept: %q", keep.secretToken)
	}

	poll := New("tg4", "TEST", "main", "polling", nil, "", "", "", sink, srv, nil, media.Policy{}, log)
	if poll.secretToken != "" {
		t.Fatalf("polling mode generates no webhook secret: %q", poll.secretToken)
	}
}

// TestHandleWebhookVerifiesSecretToken: deliveries must present the secret
// Telegram was given at setWebhook, before any parsing. Missing is 401,
// mismatched is 403, and a failed check must not reach update handling.
func TestHandleWebhookVerifiesSecretToken(t *testing.T) {
	sink := &captureSink{}
	d := &Driver{
		name:        "telegram",
		token:       "TEST",
		agent:       "main",
		mode:        "webhook",
		path:        "/webhook/telegram/",
		secretToken: "s3cret-token",
		sink:        sink,
		log:         testLogger(),
		client:      &http.Client{Timeout: 5 * time.Second},
	}

	post := func(headerVal string, present bool) *httptest.ResponseRecorder {
		body := strings.NewReader(`{"update_id":1,"message":{"message_id":1,"text":"hi","from":{"id":7},"chat":{"id":99}}}`)
		req := httptest.NewRequest(http.MethodPost, "/webhook/telegram/", body)
		if present {
			req.Header.Set(secretTokenHeader, headerVal)
		}
		rec := httptest.NewRecorder()
		d.handleWebhook(rec, req)
		return rec
	}

	rec := post("s3cret-token", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("a correct secret must pass: got %d", rec.Code)
	}
	if len(sink.inbs) != 1 {
		t.Fatalf("an authenticated delivery must reach the sink: %d", len(sink.inbs))
	}

	if rec := post("", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a missing secret must 401: got %d", rec.Code)
	}
	if rec := post("wrong", true); rec.Code != http.StatusForbidden {
		t.Fatalf("a mismatched secret must 403: got %d", rec.Code)
	}
	if len(sink.inbs) != 1 {
		t.Fatalf("rejected deliveries must not reach the sink: %d", len(sink.inbs))
	}
}

// TestHandleWebhookRefusesWhenNoSecret: a driver with no secret — constructed
// by hand, or a secret-generation failure at construction — refuses every
// delivery rather than accepting them all. It used to fall through to update
// handling, which meant the one failure mode that leaves the webhook
// unauthenticated was also the one that admitted anything.
func TestHandleWebhookRefusesWhenNoSecret(t *testing.T) {
	sink := &captureSink{}
	d := &Driver{
		name:   "telegram",
		token:  "TEST",
		agent:  "main",
		mode:   "webhook",
		path:   "/webhook/telegram/",
		sink:   sink,
		log:    testLogger(),
		client: &http.Client{Timeout: 5 * time.Second},
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook/telegram/",
		strings.NewReader(`{"update_id":1,"message":{"message_id":1,"text":"hi","from":{"id":7},"chat":{"id":99}}}`))
	rec := httptest.NewRecorder()
	d.handleWebhook(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-secret driver must refuse deliveries: got %d, want 503", rec.Code)
	}
	if len(sink.inbs) != 0 {
		t.Fatalf("a refused delivery must not reach the sink: %d", len(sink.inbs))
	}
}

var _ router.Sink = (*captureSink)(nil)
