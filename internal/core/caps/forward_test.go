package caps

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/config"
	"agentflow/internal/core/accounting"
	"agentflow/internal/core/chatreq"
	"agentflow/internal/core/identity"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/llm"
)

func forwardCtx(ctx context.Context, ref string) context.Context {
	return session.WithTurnMessage(ctx, &session.Message{
		ID: "msg-1", Channel: "api", ReplyTo: "req-1",
		Payload: map[string]any{"request_ref": ref},
	})
}

func forwardRegistry(t *testing.T, maxTokens int) (*chatreq.Registry, string) {
	t.Helper()
	reg := chatreq.NewRegistry()
	req := &chatreq.Request{
		ID: "req-1", Model: "coder", MaxTokens: maxTokens,
		Messages: []session.ChatMessage{{Role: "user", Content: "hi"}},
	}
	ref := reg.Add(req)
	return reg, ref
}

func TestForwardWritesOneUsageRow(t *testing.T) {
	srv := sseProvider(t, 100)
	mgr := llm.NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	reg, ref := forwardRegistry(t, 100)
	rec := &fakeRecorder{}
	h := MeteredLLMHandlers(mgr, nil, Metering{Agent: "bot", Ledger: rec, Requests: reg})

	resp, ok := h["llm.forward"](forwardCtx(context.Background(), ref), session.Op{Type: "llm.forward"})
	if !ok {
		t.Fatalf("forward: %s", resp)
	}
	if !strings.Contains(resp, `"text":"ok"`) {
		t.Fatalf("forward response: %s", resp)
	}
	if len(rec.got) != 1 {
		t.Fatalf("ledger rows = %d; want 1", len(rec.got))
	}
	if rec.got[0].Input != 100 || rec.got[0].Output != 1 || rec.got[0].Kind != "stream" {
		t.Fatalf("ledger row: %+v", rec.got[0])
	}
}

// TestForwardReleasesTheQuotaHoldOnProviderError: the first call reserves the
// request's max_tokens and then fails upstream. If the hold were leaked, the
// second call — which fits the remaining budget only because the first was
// released — would be refused before reaching the provider.
func TestForwardReleasesTheQuotaHoldOnProviderError(t *testing.T) {
	quota, userID := quotaFixture(t, 150)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[{\"delta\":{}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	mgr := llm.NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	reg, ref := forwardRegistry(t, 100)
	rec := &fakeRecorder{}
	h := MeteredLLMHandlers(mgr, nil, Metering{Agent: "bot", Ledger: rec, Requests: reg, Quota: quota})
	ctx := session.WithPersonalIdentity(forwardCtx(context.Background(), ref), userID)

	if resp, ok := h["llm.forward"](ctx, session.Op{Type: "llm.forward"}); ok {
		t.Fatalf("first call should fail upstream: %s", resp)
	}
	if resp, ok := h["llm.forward"](ctx, session.Op{Type: "llm.forward"}); !ok {
		t.Fatalf("second call denied (the first call's hold leaked): %s", resp)
	}
	if calls != 2 {
		t.Fatalf("provider calls = %d; want 2", calls)
	}
	if len(rec.got) != 2 {
		t.Fatalf("ledger rows = %d; want 2 (one per attempt)", len(rec.got))
	}
}

// TestForwardReleasesTheQuotaHoldOnCancel: a client that goes away mid-stream
// leaves no reservation behind, and the attempt is recorded.
func TestForwardReleasesTheQuotaHoldOnCancel(t *testing.T) {
	quota, userID := quotaFixture(t, 150)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			// Establish the stream, then stay silent until the client goes
			// away. Writing the header first is what makes the server register
			// the disconnect.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[{\"delta\":{}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	mgr := llm.NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	reg, ref := forwardRegistry(t, 100)
	rec := &fakeRecorder{}
	h := MeteredLLMHandlers(mgr, nil, Metering{Agent: "bot", Ledger: rec, Requests: reg, Quota: quota})
	userCtx := session.WithPersonalIdentity(forwardCtx(context.Background(), ref), userID)

	ctx, cancel := context.WithCancel(userCtx)
	done := make(chan struct{})
	var firstOK bool
	go func() {
		_, firstOK = h["llm.forward"](ctx, session.Op{Type: "llm.forward"})
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not return after cancel")
	}
	if firstOK {
		t.Fatal("a cancelled forward must fail")
	}
	if resp, ok := h["llm.forward"](userCtx, session.Op{Type: "llm.forward"}); !ok {
		t.Fatalf("call after cancel denied (the hold leaked): %s", resp)
	}
	if calls != 2 {
		t.Fatalf("provider calls = %d; want 2", calls)
	}
	if len(rec.got) != 2 {
		t.Fatalf("ledger rows = %d; want 2", len(rec.got))
	}
}

func quotaFixture(t *testing.T, limit int64) (*accounting.Quota, string) {
	t.Helper()
	dir := t.TempDir()
	ledger, err := runtime.OpenSQLite(filepath.Join(dir, "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	reg, err := identity.Open(filepath.Join(dir, "identity.db"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	p, err := reg.CreateProfile("Oscar", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Update(p.UserID, nil, nil, &limit); err != nil {
		t.Fatal(err)
	}
	return accounting.New(ledger, reg.LimitFor, 0), p.UserID
}

// TestForwardReportsMissingRequest: a ref this process does not hold is the
// regional gap; the error names the limitation and the way out.
func TestForwardReportsMissingRequest(t *testing.T) {
	srv := sseProvider(t, 10)
	mgr := llm.NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	reg := chatreq.NewRegistry()
	h := MeteredLLMHandlers(mgr, nil, Metering{Agent: "bot", Requests: reg})
	_, ok := h["llm.forward"](forwardCtx(context.Background(), "rq-missing"), session.Op{Type: "llm.forward"})
	if ok {
		t.Fatal("a missing request ref must fail")
	}
}

func TestOpenAIRequestReturnsTheTable(t *testing.T) {
	reg, ref := forwardRegistry(t, 10)
	h := MeteredLLMHandlers(nil, nil, Metering{Requests: reg})
	resp, ok := h["openai.request"](forwardCtx(context.Background(), ref), session.Op{Type: "openai.request"})
	if !ok {
		t.Fatalf("openai.request: %s", resp)
	}
	if !strings.Contains(resp, `"model":"coder"`) || !strings.Contains(resp, `"messages"`) {
		t.Fatalf("request table: %s", resp)
	}
}

func TestOpenAIRequestCapsTheBody(t *testing.T) {
	reg := chatreq.NewRegistry()
	big := strings.Repeat("x", 5<<20)
	ref := reg.Add(&chatreq.Request{
		ID: "req-big", Model: "coder",
		Messages: []session.ChatMessage{{Role: "user", Content: big}},
	})
	h := MeteredLLMHandlers(nil, nil, Metering{Requests: reg})
	resp, ok := h["openai.request"](forwardCtx(context.Background(), ref), session.Op{Type: "openai.request"})
	if ok || !strings.Contains(resp, "over the") {
		t.Fatalf("oversized request = %v %s; want a size refusal", ok, resp)
	}
}
