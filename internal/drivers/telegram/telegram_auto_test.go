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
	base     string
	webhook  string
	deletes  int
	sets     int
	srv      *httptest.Server
}

func newMockTG(t *testing.T) *mockTG {
	m := &mockTG{}
	mux := http.NewServeMux()
	mux.HandleFunc("/botTEST/getUpdates", func(w http.ResponseWriter, r *http.Request) {
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
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/botTEST/deleteWebhook", func(w http.ResponseWriter, r *http.Request) {
		m.deletes++
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