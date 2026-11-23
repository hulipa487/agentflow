package caps

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/llm"
)

type recordingLedger struct {
	runtime.Ledger
	mu      sync.Mutex
	records []runtime.UsageRecord
}

func (l *recordingLedger) RecordUsage(rec runtime.UsageRecord, withEvent bool) error {
	l.mu.Lock()
	l.records = append(l.records, rec)
	l.mu.Unlock()
	return l.Ledger.RecordUsage(rec, withEvent)
}

func (l *recordingLedger) snapshot() []runtime.UsageRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]runtime.UsageRecord(nil), l.records...)
}

func sseModelServer(t *testing.T, onReq func()) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onReq != nil {
			onReq()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\n"+
			"data: [DONE]\n\n")
	}))
}

func TestLLMHandlersFallbackToAgentModelFromCtx(t *testing.T) {
	var (
		mu                               sync.Mutex
		defaultHits, fastHits, expHits int
	)
	srvDefault := sseModelServer(t, func() { mu.Lock(); defaultHits++; mu.Unlock() })
	defer srvDefault.Close()
	srvFast := sseModelServer(t, func() { mu.Lock(); fastHits++; mu.Unlock() })
	defer srvFast.Close()
	srvExp := sseModelServer(t, func() { mu.Lock(); expHits++; mu.Unlock() })
	defer srvExp.Close()

	mgr := llm.NewManager(map[string]config.Model{
		"default":  {Provider: "openai", Model: "m-default", BaseURL: srvDefault.URL},
		"fast":     {Provider: "openai", Model: "m-fast", BaseURL: srvFast.URL},
		"explicit": {Provider: "openai", Model: "m-explicit", BaseURL: srvExp.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	h := LLMHandlers(mgr, nil)
	ctx := session.WithAgentModel(context.Background(), "fast")

	// Empty op.Model on llm.chat must fall back to session.AgentModelFromCtx(ctx).
	if resp, ok := h["llm.chat"](ctx, session.Op{
		Type:     "llm.chat",
		Messages: []session.ChatMessage{{Role: "user", Content: "hi"}},
	}); !ok {
		t.Fatalf("llm.chat fallback failed: %s", resp)
	}

	// Explicit op.Model must win over session.AgentModelFromCtx(ctx).
	if resp, ok := h["llm.chat"](ctx, session.Op{
		Type:     "llm.chat",
		Model:    "explicit",
		Messages: []session.ChatMessage{{Role: "user", Content: "hi"}},
	}); !ok {
		t.Fatalf("llm.chat explicit failed: %s", resp)
	}

	// Empty op.Model on llm.stream.open must also fall back to session.AgentModelFromCtx(ctx).
	resp, ok := h["llm.stream.open"](ctx, session.Op{
		Type:     "llm.stream.open",
		Messages: []session.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if !ok {
		t.Fatalf("llm.stream.open fallback failed: %s", resp)
	}
	var handle struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(resp), &handle); err != nil || handle.ID == "" {
		t.Fatalf("missing stream id in %s: %v", resp, err)
	}
	for {
		fResp, ok := h["llm.stream.next"](ctx, session.Op{Type: "llm.stream.next", Stream: handle.ID})
		if !ok {
			t.Fatalf("llm.stream.next failed: %s", fResp)
		}
		var frame struct {
			Done bool `json:"done"`
		}
		if err := json.Unmarshal([]byte(fResp), &frame); err == nil && frame.Done {
			break
		}
	}
	_, _ = h["llm.stream.close"](ctx, session.Op{Type: "llm.stream.close", Stream: handle.ID})

	mu.Lock()
	defer mu.Unlock()
	if defaultHits != 0 || fastHits != 2 || expHits != 1 {
		t.Fatalf("hits default=%d fast=%d explicit=%d; want default=0 fast=2 explicit=1", defaultHits, fastHits, expHits)
	}
}

func TestMeteredLLMHandlersAccountingUsesEffectiveModel(t *testing.T) {
	dir := t.TempDir()
	db, err := runtime.OpenSQLite(filepath.Join(dir, "runtime.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ledger := &recordingLedger{Ledger: db}

	srvDefault := sseModelServer(t, nil)
	defer srvDefault.Close()
	srvFast := sseModelServer(t, nil)
	defer srvFast.Close()
	srvFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	}))
	defer srvFail.Close()

	mgr := llm.NewManager(map[string]config.Model{
		"default":   {Provider: "openai", Model: "m-default", BaseURL: srvDefault.URL},
		"fast":      {Provider: "openai", Model: "m-fast", BaseURL: srvFast.URL},
		"fail-fast": {Provider: "openai", Model: "m-fail", BaseURL: srvFail.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	h := MeteredLLMHandlers(mgr, nil, Metering{
		Agent:  "assistant",
		Ledger: ledger,
	})

	fastCtx := session.WithAgentModel(context.Background(), "fast")
	if resp, ok := h["llm.chat"](fastCtx, session.Op{
		Type:     "llm.chat",
		MaxTokens: 50,
		Messages: []session.ChatMessage{{Role: "user", Content: "hi"}},
	}); !ok {
		t.Fatalf("metered llm.chat failed: %s", resp)
	}

	sResp, ok := h["llm.stream.open"](fastCtx, session.Op{
		Type:      "llm.stream.open",
		MaxTokens: 50,
		Messages:  []session.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if !ok {
		t.Fatalf("metered llm.stream.open failed: %s", sResp)
	}
	var handle struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(sResp), &handle)
	for {
		fResp, ok := h["llm.stream.next"](fastCtx, session.Op{Type: "llm.stream.next", Stream: handle.ID})
		if !ok {
			t.Fatalf("metered llm.stream.next failed: %s", fResp)
		}
		var frame struct {
			Done bool `json:"done"`
		}
		if err := json.Unmarshal([]byte(fResp), &frame); err == nil && frame.Done {
			break
		}
	}

	failCtx := session.WithAgentModel(context.Background(), "fail-fast")
	if _, ok := h["llm.chat"](failCtx, session.Op{
		Type:      "llm.chat",
		MaxTokens: 50,
		Messages:  []session.ChatMessage{{Role: "user", Content: "hi"}},
	}); ok {
		t.Fatal("expected failed provider call")
	}

	recs := ledger.snapshot()
	if len(recs) != 3 {
		t.Fatalf("got %d usage records, want 3: %+v", len(recs), recs)
	}
	if recs[0].Kind != "chat" || recs[0].Model != "fast" || !recs[0].OK {
		t.Fatalf("record 0 = %+v; want chat/fast/ok", recs[0])
	}
	if recs[1].Kind != "stream" || recs[1].Model != "fast" || !recs[1].OK {
		t.Fatalf("record 1 = %+v; want stream/fast/ok", recs[1])
	}
	if recs[2].Kind != "chat" || recs[2].Model != "fail-fast" || recs[2].OK {
		t.Fatalf("record 2 = %+v; want chat/fail-fast/failed", recs[2])
	}
}
