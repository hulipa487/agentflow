package moderation

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/core/budget"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/safety"
	"agentflow/internal/drivers/llm"
)

func testClient(t *testing.T, handler http.HandlerFunc, opts Options) (*Client, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := llm.NewManager(map[string]config.Model{
		"mod": {Provider: "openai", Model: "m", BaseURL: srv.URL, APIKey: "test-key"},
	}, log)
	opts.Model = "mod"
	opts.Log = log
	c, err := New(mgr, opts)
	if err != nil {
		t.Fatal(err)
	}
	return c, &hits
}

func verdictReply() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(
			`data: {"choices":[{"delta":{"content":"{\"label\":\"violence\",\"score\":0.93,\"reason\":\"threat\"}"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}` + "\n\n" +
				"data: [DONE]\n\n"))
	}
}

func TestClassifyAndCache(t *testing.T) {
	c, hits := testClient(t, verdictReply(), Options{
		Labels:    []string{"safe", "violence"},
		CacheSize: 10,
	})
	ctx := context.Background()
	v, err := c.Classify(ctx, safety.ClassifyRequest{Text: "hurt them", Agent: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Label != "violence" || v.Score != 0.93 || v.Model != "mod" {
		t.Fatalf("got %+v", v)
	}
	v2, err := c.Classify(ctx, safety.ClassifyRequest{Text: "hurt them", Agent: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	if !v2.Cached {
		t.Fatal("second classify should be a cache hit")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("provider hits = %d; want 1", got)
	}
}

func TestClassifyRejectsBadOutput(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(
			`data: {"choices":[{"delta":{"content":"looks fine to me"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}` + "\n\n" +
				"data: [DONE]\n\n"))
	}, Options{Labels: []string{"safe", "violence"}})
	if _, err := c.Classify(context.Background(), safety.ClassifyRequest{Text: "hi"}); err == nil {
		t.Fatal("expected invalid-output error")
	}
}

type fakeLedger struct {
	mu   sync.Mutex
	recs []runtime.UsageRecord
}

func (f *fakeLedger) RecordUsage(rec runtime.UsageRecord, _ bool) error {
	f.mu.Lock()
	f.recs = append(f.recs, rec)
	f.mu.Unlock()
	return nil
}

func TestClassifyMetersModeration(t *testing.T) {
	ledger := &fakeLedger{}
	c, _ := testClient(t, verdictReply(), Options{
		Labels:      []string{"safe", "violence"},
		Ledger:      ledger,
		LedgerAgent: "__moderation_mod",
	})
	if _, err := c.Classify(context.Background(), safety.ClassifyRequest{Text: "hurt them", Agent: "assistant"}); err != nil {
		t.Fatal(err)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.recs) != 1 {
		t.Fatalf("ledger records = %d; want 1", len(ledger.recs))
	}
	got := ledger.recs[0]
	if got.Agent != "__moderation_mod" || got.Kind != "moderation" || got.Input != 10 || got.Output != 5 {
		t.Fatalf("unexpected ledger record: %+v", got)
	}
}

func TestClassifyBudgetExhausted(t *testing.T) {
	pool := budget.NewPool(1) // below the minimum reserve
	c, hits := testClient(t, verdictReply(), Options{
		Labels: []string{"safe", "violence"},
		Pool:   pool,
	})
	_, err := c.Classify(context.Background(), safety.ClassifyRequest{Text: "hurt them"})
	if err == nil || !strings.Contains(err.Error(), safety.ErrClassifierBudget.Error()) {
		t.Fatalf("got %v; want budget exhaustion", err)
	}
	if hits.Load() != 0 {
		t.Fatal("a budget-denied call must not reach the provider")
	}
}
