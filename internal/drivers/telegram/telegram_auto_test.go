package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mockTG is a minimal Telegram API server for testing setWebhook/deleteWebhook
// and the auto-mode probe decision. It records the calls it received.
type mockTG struct {
	base           string
	webhook        string
	secrets        []string // secret_token values seen in setWebhook bodies
	deletes        int
	sets           int
	getUpdatesHits int
	failDelete     bool // deleteWebhook answers 500
	failSet        bool // setWebhook answers 500
	srv            *httptest.Server
}

func newMockTG(t *testing.T) *mockTG {
	m := &mockTG{}
	mux := http.NewServeMux()
	mux.HandleFunc("/botTEST/getUpdates", func(w http.ResponseWriter, r *http.Request) {
		m.getUpdatesHits++
		// return empty result list — polling stops via ctx
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	})
	mux.HandleFunc("/botTEST/setWebhook", func(w http.ResponseWriter, r *http.Request) {
		m.sets++
		b, _ := io.ReadAll(r.Body)
		var v map[string]any
		_ = json.Unmarshal(b, &v)
		if u, ok := v["url"].(string); ok {
			m.webhook = u
		}
		if s, ok := v["secret_token"].(string); ok {
			m.secrets = append(m.secrets, s)
		}
		if m.failSet {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/botTEST/deleteWebhook", func(w http.ResponseWriter, r *http.Request) {
		m.deletes++
		if m.failDelete {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	m.base = m.srv.URL
	return m
}

// newMockTG and testLogger (testhelpers_test.go) are the test seams. Each
// test builds a *Driver literal with apiBase pointed at the mock so it talks
// to httptest instead of api.telegram.org.
func TestAutoNoPublicURLPolls(t *testing.T) {
	m := newMockTG(t)
	d := &Driver{
		name:    "telegram",
		token:   "TEST",
		agent:   "main",
		mode:    "auto",
		path:    "/webhook/telegram/",
		apiBase: m.base,
		log:     testLogger(),
		client:  &http.Client{Timeout: 5 * time.Second},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("auto start: %v", err)
	}
}

func TestAutoHealthySetsWebhook(t *testing.T) {
	// Two servers: the "public" health server and the mock TG API.
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(health.Close)
	// newDriver constructs its own mock TG; we need its setWebhook count.
	// We can't read m directly from inside newDriver, so we set apiBase
	// explicitly by constructing a fresh mock here and overriding.
	m := newMockTG(t)
	d := &Driver{
		name:      "telegram",
		token:     "TEST",
		agent:     "main",
		mode:      "auto",
		path:      "/webhook/telegram/",
		publicURL: health.URL,
		apiBase:   m.base,
		log:       testLogger(),
		client:    &http.Client{Timeout: 5 * time.Second},
	}
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("auto start: %v", err)
	}
	if m.sets != 1 {
		t.Fatalf("expected 1 setWebhook, got %d", m.sets)
	}
	if !strings.HasSuffix(m.webhook, "/webhook/telegram/") {
		t.Fatalf("unexpected webhook url: %q", m.webhook)
	}
}

func TestAutoUnreachableFallsBackToPolling(t *testing.T) {
	m := newMockTG(t)
	d := &Driver{
		name:      "telegram",
		token:     "TEST",
		agent:     "main",
		mode:      "auto",
		path:      "/webhook/telegram/",
		publicURL: "http://127.0.0.1:1", // unreachable: nothing listens on port 1
		apiBase:   m.base,
		log:       testLogger(),
		client:    &http.Client{Timeout: 5 * time.Second},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("auto start: %v", err)
	}
	// unreachable should call deleteWebhook and not setWebhook
	if m.sets != 0 {
		t.Fatalf("expected no setWebhook when unreachable, got %d", m.sets)
	}
	if m.deletes != 1 {
		t.Fatalf("expected 1 deleteWebhook, got %d", m.deletes)
	}
}

func TestSetDeleteWebhook(t *testing.T) {
	m := newMockTG(t)
	d := &Driver{
		name:    "telegram",
		token:   "TEST",
		agent:   "main",
		mode:    "polling",
		path:    "/webhook/telegram/",
		apiBase: m.base,
		log:     testLogger(),
		client:  &http.Client{Timeout: 5 * time.Second},
	}
	if err := d.setWebhook("https://example.com/wh/"); err != nil {
		t.Fatal(err)
	}
	if m.sets != 1 || m.webhook != "https://example.com/wh/" {
		t.Fatalf("set: sets=%d url=%q", m.sets, m.webhook)
	}
	if err := d.deleteWebhook(); err != nil {
		t.Fatal(err)
	}
	if m.deletes != 1 {
		t.Fatalf("delete: %d", m.deletes)
	}
}

// TestSetWebhookSendsSecretToken: a configured secret rides the setWebhook
// body so Telegram authenticates future deliveries with it.
func TestSetWebhookSendsSecretToken(t *testing.T) {
	m := newMockTG(t)
	d := &Driver{
		name:        "telegram",
		token:       "TEST",
		agent:       "main",
		mode:        "webhook",
		path:        "/webhook/telegram/",
		secretToken: "tok-abc_123",
		apiBase:     m.base,
		log:         testLogger(),
		client:      &http.Client{Timeout: 5 * time.Second},
	}
	if err := d.setWebhook("https://example.com/wh/"); err != nil {
		t.Fatal(err)
	}
	if len(m.secrets) != 1 || m.secrets[0] != "tok-abc_123" {
		t.Fatalf("secret_token not sent: %v", m.secrets)
	}
}

// TestPollingModeDeletesStaleWebhook: entering polling mode must clear any
// webhook Telegram still holds — reverting from webhook mode, or auto's
// setWebhook-failed fallback — or getUpdates 409s forever.
func TestPollingModeDeletesStaleWebhook(t *testing.T) {
	m := newMockTG(t)
	d := &Driver{
		name:    "telegram",
		token:   "TEST",
		agent:   "main",
		mode:    "polling",
		path:    "/webhook/telegram/",
		apiBase: m.base,
		log:     testLogger(),
		client:  &http.Client{Timeout: 5 * time.Second},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("polling start: %v", err)
	}
	if m.deletes != 1 {
		t.Fatalf("polling must clear a stale webhook first: deletes=%d", m.deletes)
	}
}

// TestPollingSurvivesDeleteFailure: deleteWebhook is best-effort — a network
// blip must not stop polling from starting (the WARN is what makes the
// persistent 409 visible to the operator).
func TestPollingSurvivesDeleteFailure(t *testing.T) {
	m := newMockTG(t)
	m.failDelete = true
	d := &Driver{
		name:    "telegram",
		token:   "TEST",
		agent:   "main",
		mode:    "polling",
		path:    "/webhook/telegram/",
		apiBase: m.base,
		log:     testLogger(),
		client:  &http.Client{Timeout: 5 * time.Second},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("polling start: %v", err)
	}
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) && m.getUpdatesHits == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if m.getUpdatesHits == 0 {
		t.Fatal("polling must start even when deleteWebhook fails")
	}
}

// TestAutoSetWebhookFailureDeletesAndPolls: the probe succeeded but
// setWebhook itself failed — the fallback to polling must still clear any
// (partially-applied or previous-boot) webhook, or getUpdates 409s.
func TestAutoSetWebhookFailureDeletesAndPolls(t *testing.T) {
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(health.Close)
	m := newMockTG(t)
	m.failSet = true
	d := &Driver{
		name:      "telegram",
		token:     "TEST",
		agent:     "main",
		mode:      "auto",
		path:      "/webhook/telegram/",
		publicURL: health.URL,
		apiBase:   m.base,
		log:       testLogger(),
		client:    &http.Client{Timeout: 5 * time.Second},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("auto start: %v", err)
	}
	if m.deletes != 1 {
		t.Fatalf("setWebhook-failed fallback must deleteWebhook first: deletes=%d", m.deletes)
	}
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) && m.getUpdatesHits == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if m.getUpdatesHits == 0 {
		t.Fatal("fallback must reach the poll loop")
	}
}
