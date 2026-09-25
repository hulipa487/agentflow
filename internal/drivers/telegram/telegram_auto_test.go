package telegram

import (
	"context"
	"encoding/json"
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
	allowed        []string // allowed_updates values seen in setWebhook bodies
	updates        string   // one raw getUpdates result, served once; empty = no updates
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
		// serve the queued update once — polling stops via ctx
		if upd := m.updates; upd != "" {
			m.updates = ""
			_, _ = w.Write([]byte(`{"ok":true,"result":[` + upd + `]}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	})
	mux.HandleFunc("/botTEST/setWebhook", func(w http.ResponseWriter, r *http.Request) {
		m.sets++
		// The fields are unchanged, but the body is multipart/form-data now
		// rather than the JSON this driver used to hand-roll: the SDK builds
		// one form for every method. Reading the values off the form is what
		// the assertions below still pin; the encoding is the SDK's business.
		_ = r.ParseMultipartForm(1 << 20)
		m.webhook = r.FormValue("url")
		if s := r.FormValue("secret_token"); s != "" {
			m.secrets = append(m.secrets, s)
		}
		if a := r.FormValue("allowed_updates"); a != "" {
			var updates []string
			if json.Unmarshal([]byte(a), &updates) == nil {
				m.allowed = updates
			}
		}
		if m.failSet {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// The SDK decodes the envelope's result into a bool and reports a
		// missing one as a decode error, so the answer has to carry it; real
		// Telegram always does.
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	})
	mux.HandleFunc("/botTEST/deleteWebhook", func(w http.ResponseWriter, r *http.Request) {
		m.deletes++
		if m.failDelete {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
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

// TestSetDeleteWebhook also pins allowed_updates: Telegram defaults to sending
// every update type, and only messages are handled, so the registration has to
// keep asking for messages alone now that the SDK builds the request.
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
	if len(m.allowed) != 1 || m.allowed[0] != "message" {
		t.Fatalf("setWebhook must ask for message updates only: %v", m.allowed)
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

// TestPollingDeliversUpdateToSink: polling is handed the SDK's own update model
// and converts it, so it is the only path where a field can be dropped without
// the webhook tests noticing — they decode the driver's shape straight from a
// request body. The allow-list is left on, so a conversion that lost the sender
// id would drop this message instead of delivering it.
func TestPollingDeliversUpdateToSink(t *testing.T) {
	m := newMockTG(t)
	m.updates = `{"update_id":7,"message":{"message_id":1,"date":1,"text":"hello",` +
		`"from":{"id":7,"is_bot":false,"username":"someone","first_name":"Some One"},` +
		`"chat":{"id":99,"type":"private"}}}`
	sink := &captureSink{}
	d := &Driver{
		name:    "telegram",
		token:   "TEST",
		agent:   "main",
		mode:    "polling",
		apiBase: m.base,
		allow:   map[int64]bool{7: true},
		sink:    sink,
		log:     testLogger(),
		client:  &http.Client{Timeout: 5 * time.Second},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("polling start: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(sink.inbs) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(sink.inbs) != 1 {
		t.Fatalf("polling must deliver the update: %d", len(sink.inbs))
	}
	msg := sink.inbs[0].Message
	if msg.Text != "hello" || msg.ReplyTo != "99" || msg.From != "user:telegram:7" {
		t.Fatalf("converted update lost a field: %+v", msg)
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

// TestSetWebhookRetriesOnRateLimit: a 429 is Telegram asking to be called again
// after a window, not a failure, so the driver sleeps the advertised
// retry_after and retries. The SDK does this inside its own getUpdates loop but
// leaves every other method to the caller, which is what call() exists for.
//
// The cost of this test is real: retry_after is in whole seconds, so the
// shortest honest window still sleeps ~1s per 429.
func TestSetWebhookRetriesOnRateLimit(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 2 {
			// Telegram's own answer, HTTP status and body together. The SDK
			// keys on error_code in the body, not the status line.
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":1}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer srv.Close()

	d := &Driver{
		name:    "telegram",
		token:   "TEST",
		agent:   "main",
		mode:    "polling",
		path:    "/webhook/telegram/",
		apiBase: srv.URL,
		log:     testLogger(),
		client:  &http.Client{Timeout: 5 * time.Second},
	}
	start := time.Now()
	if err := d.setWebhook("https://example.com/wh/"); err != nil {
		t.Fatalf("setWebhook: %v", err)
	}
	if calls != 3 {
		t.Fatalf("setWebhook made %d attempts; want 3 (two 429s, then success)", calls)
	}
	// Two 1s windows. The floor matters: a retry that returned immediately
	// would hammer Telegram, which is what the window is for.
	if elapsed := time.Since(start); elapsed < 2*time.Second {
		t.Fatalf("setWebhook returned after %v; the retry_after window was not slept", elapsed)
	}
}

// TestSetWebhookGivesUpAfterRateLimit: the backoff is bounded. Once the retries
// are spent the 429 is surfaced rather than slept on forever, so a hard
// throttle cannot wedge the caller.
func TestSetWebhookGivesUpAfterRateLimit(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":1}}`))
	}))
	defer srv.Close()

	d := &Driver{
		name:    "telegram",
		token:   "TEST",
		agent:   "main",
		mode:    "polling",
		path:    "/webhook/telegram/",
		apiBase: srv.URL,
		log:     testLogger(),
		client:  &http.Client{Timeout: 5 * time.Second},
	}
	err := d.setWebhook("https://example.com/wh/")
	if err == nil {
		t.Fatal("a permanently throttled setWebhook must fail rather than retry forever")
	}
	if !strings.Contains(err.Error(), "Too Many Requests") {
		t.Fatalf("error %q does not surface the rate limit", err)
	}
	if calls != rateLimitRetries+1 {
		t.Fatalf("made %d attempts; want %d (the initial call plus rateLimitRetries)", calls, rateLimitRetries+1)
	}
}
