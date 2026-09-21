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
	"agentflow/internal/core/identity"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/llm"
)

// sseProvider answers every chat with a fixed usage block, so a test can
// predict exactly what the ledger will hold.
func sseProvider(t *testing.T, inputTokens int) *httptest.Server {
	t.Helper()
	body := `data: {"choices":[{"delta":{"content":"ok"}}]}

data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":` + itoa(inputTokens) + `,"completion_tokens":1}}

data: [DONE]

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// The whole point of enforcement: once a user's daily quota is spent, the next
// call is refused before it reaches the provider.
func TestUserQuotaDeniesTheCallBeforeTheProvider(t *testing.T) {
	dir := t.TempDir()
	ledger, err := runtime.Open(filepath.Join(dir, "runtime.db"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	reg, err := identity.Open(filepath.Join(dir, "identity.db"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open identity: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	p, err := reg.CreateProfile("Oscar", "")
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	limit := int64(150)
	if err := reg.Update(p.UserID, nil, nil, &limit); err != nil {
		t.Fatalf("set limit: %v", err)
	}

	var providerHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerHits++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"ok"}}]}

data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":100,"completion_tokens":1}}

data: [DONE]

`)
	}))
	defer srv.Close()

	mgr := llm.NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	quota := accounting.New(ledger, reg.LimitFor, 0)
	h := MeteredLLMHandlers(mgr, nil, Metering{
		Quota:  quota,
		Agent:  "bot",
		Ledger: ledger,
	})
	chat := h["llm.chat"]
	ctx := session.WithUserUUID(context.Background(), p.UserID)
	// MaxTokens pins the reservation estimate to 100, so the arithmetic is
	// about the quota rather than the default 4096-token guess.
	op := session.Op{
		Type: "llm.chat", Model: "default", MaxTokens: 100,
		Messages: []session.ChatMessage{{Role: "user", Content: "hi"}},
	}

	if resp, ok := chat(ctx, op); !ok {
		t.Fatalf("first call should be allowed: %s", resp)
	}
	if providerHits != 1 {
		t.Fatalf("expected one provider call, got %d", providerHits)
	}

	// 100 of 150 is spent, so another 100 cannot be reserved.
	resp, ok := chat(ctx, op)
	if ok {
		t.Fatal("the second call should have been refused")
	}
	if !strings.Contains(resp, "user_quota_exhausted") {
		t.Fatalf("refusal should name the quota, got %s", resp)
	}
	if providerHits != 1 {
		t.Fatalf("a refused call must not reach the provider, hits=%d", providerHits)
	}

	// A context with no user has no account: it is accounted to the service
	// bucket and never quota-limited.
	if _, ok := chat(context.Background(), op); !ok {
		t.Fatal("a userless call must not be quota-limited")
	}
	if providerHits != 2 {
		t.Fatalf("the userless call should have reached the provider, hits=%d", providerHits)
	}
}

// An unregistered handle carries no user stamp, so its traffic is never charged
// to anyone's quota — the property that stops a forged link from burning
// somebody else's budget.
func TestQuotaIgnoresTrafficWithoutAUser(t *testing.T) {
	dir := t.TempDir()
	ledger, err := runtime.Open(filepath.Join(dir, "runtime.db"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	srv := sseProvider(t, 10)
	mgr := llm.NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// A default limit of 1 token: any user-attributed call would be refused.
	quota := accounting.New(ledger, nil, 1)
	h := MeteredLLMHandlers(mgr, nil, Metering{Quota: quota, Agent: "bot", Ledger: ledger})
	op := session.Op{
		Type: "llm.chat", Model: "default", MaxTokens: 10,
		Messages: []session.ChatMessage{{Role: "user", Content: "hi"}},
	}

	if resp, ok := h["llm.chat"](context.Background(), op); !ok {
		t.Fatalf("traffic with no user must pass: %s", resp)
	}
	// The same call attributed to a user would be refused.
	ctx := session.WithUserUUID(context.Background(), "u_someone")
	if resp, ok := h["llm.chat"](ctx, op); ok {
		t.Fatalf("a user-attributed call over the limit must be refused: %s", resp)
	}
	// And the uncharged traffic landed in the service bucket, not on a user.
	day := runtime.DayKey(time.Now())
	totals, err := ledger.UsageForDay("", day)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if totals.Calls == 0 {
		t.Fatal("userless traffic should be recorded in the service bucket")
	}
}
