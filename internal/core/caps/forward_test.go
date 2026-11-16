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

	resp, ok := h["llm.forward"](session.WithAgentModel(forwardCtx(context.Background(), ref), "default"), session.Op{Type: "llm.forward"})
	if !ok {
		t.Fatalf("forward: %s", resp)
	}
	if !strings.Contains(resp, `"text":"ok"`) {
		t.Fatalf("forward response: %s", resp)
	}
	if len(rec.got) != 1 {
		t.Fatalf("ledger rows = %d; want 1", len(rec.got))
	}
	// The row names the call for what it is and the model it ran on. Recorded
	// as a generic "stream" against no model, a whole channel's usage could
	// not be broken down per model.
	row := rec.got[0]
	if row.Input != 100 || row.Output != 1 || row.Kind != "forward" || row.Model != "default" || !row.OK {
		t.Fatalf("ledger row: %+v", row)
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
	ctx := session.WithTurnMessage(context.Background(), &session.Message{
		ID: "msg-big", Channel: "api", ReplyTo: "req-big",
		Payload: map[string]any{"request_ref": ref},
	})
	resp, ok := h["openai.request"](ctx, session.Op{Type: "openai.request"})
	if ok || !strings.Contains(resp, "over the") {
		t.Fatalf("oversized request = %v %s; want a size refusal", ok, resp)
	}
}

// recordingProvider is an OpenAI-shaped stream that keeps the request body and
// answers with the given SSE.
func recordingProvider(t *testing.T, sse string, body *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*body = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const okSSE = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":1}}\n\n" +
	"data: [DONE]\n\n"

// TestForwardSendsEveryClientOptionUpstream: the pass-through's whole promise
// is that the client's request reaches the model. Every sampling option the
// channel parses has to survive the Op-to-Opts conversion, which once copied
// only the five options that existed before this channel did.
func TestForwardSendsEveryClientOptionUpstream(t *testing.T) {
	var body string
	srv := recordingProvider(t, okSSE, &body)
	mgr := llm.NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	reg := chatreq.NewRegistry()
	temp, topP, seed, parallel := 0.2, 0.5, int64(42), false
	ref := reg.Add(&chatreq.Request{
		ID: "req-1", Model: "coder", MaxTokens: 64,
		Messages:          []session.ChatMessage{{Role: "user", Content: "hi"}},
		Tools:             []session.ToolSpec{{Name: "lookup", Parameters: map[string]any{"type": "object"}}},
		ToolChoice:        "required",
		Temperature:       &temp,
		TopP:              &topP,
		Seed:              &seed,
		ParallelToolCalls: &parallel,
		Stop:              []string{"END"},
		ResponseFormat:    map[string]any{"type": "json_object"},
	})
	h := MeteredLLMHandlers(mgr, nil, Metering{Agent: "bot", Requests: reg})
	if resp, ok := h["llm.forward"](forwardCtx(context.Background(), ref), session.Op{Type: "llm.forward"}); !ok {
		t.Fatalf("forward: %s", resp)
	}
	for _, want := range []string{
		`"temperature":0.2`, `"top_p":0.5`, `"seed":42`, `"parallel_tool_calls":false`,
		`"stop":["END"]`, `"response_format":{"type":"json_object"}`, `"tool_choice":"required"`,
		`"max_tokens":64`, `"name":"lookup"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("upstream request is missing %s\nbody: %s", want, body)
		}
	}
}

// TestChatSendsTheNewOptionsToo: the same conversion serves llm.chat, whose
// documented options include the new ones.
func TestChatSendsTheNewOptionsToo(t *testing.T) {
	var body string
	srv := recordingProvider(t, okSSE, &body)
	mgr := llm.NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := MeteredLLMHandlers(mgr, nil, Metering{Agent: "bot"})
	topP, seed := 0.9, int64(7)
	resp, ok := h["llm.chat"](context.Background(), session.Op{
		Type: "llm.chat", Model: "default",
		Messages: []session.ChatMessage{{Role: "user", Content: "hi"}},
		TopP:     &topP, Seed: &seed, Stop: []string{"X"},
	})
	if !ok {
		t.Fatalf("chat: %s", resp)
	}
	for _, want := range []string{`"top_p":0.9`, `"seed":7`, `"stop":["X"]`} {
		if !strings.Contains(body, want) {
			t.Errorf("upstream request is missing %s\nbody: %s", want, body)
		}
	}
	if !strings.Contains(resp, `"finish_reason":"stop"`) {
		t.Errorf("chat reply carries no finish_reason: %s", resp)
	}
}

// TestForwardKeepsTheModelsOwnOutput: the finish reason and each tool call's
// argument text come back as the provider sent them.
func TestForwardKeepsTheModelsOwnOutput(t *testing.T) {
	sse := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"edit\",\"arguments\":\"{\\\"files\\\":[]}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\n" +
		"data: [DONE]\n\n"
	var body string
	srv := recordingProvider(t, sse, &body)
	mgr := llm.NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	reg, ref := forwardRegistry(t, 10)
	h := MeteredLLMHandlers(mgr, nil, Metering{Agent: "bot", Requests: reg})
	resp, ok := h["llm.forward"](forwardCtx(context.Background(), ref), session.Op{Type: "llm.forward"})
	if !ok {
		t.Fatalf("forward: %s", resp)
	}
	if !strings.Contains(resp, `"finish_reason":"length"`) {
		t.Errorf("finish reason was not passed on: %s", resp)
	}
	if !strings.Contains(resp, `"arguments":"{\"files\":[]}"`) {
		t.Errorf("the tool call's own argument text was not kept: %s", resp)
	}
}

// TestForwardRefusesARefFromAnotherTurn: the ref is data in the message
// payload. It has to name the request this turn answers.
func TestForwardRefusesARefFromAnotherTurn(t *testing.T) {
	reg, ref := forwardRegistry(t, 10) // registered as request req-1
	h := MeteredLLMHandlers(nil, nil, Metering{Requests: reg})
	other := session.WithTurnMessage(context.Background(), &session.Message{
		ID: "msg-2", Channel: "api", ReplyTo: "req-2",
		Payload: map[string]any{"request_ref": ref},
	})
	resp, ok := h["openai.request"](other, session.Op{Type: "openai.request"})
	if ok || !strings.Contains(resp, "does not belong") {
		t.Fatalf("a ref carried over from another turn = %v %s; want a refusal", ok, resp)
	}
}

// TestForwardMarksAnUpstreamFailure: a channel has to tell "the model failed"
// from "the loop failed", and the marker is how.
func TestForwardMarksAnUpstreamFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	mgr := llm.NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	reg, ref := forwardRegistry(t, 10)
	rec := &fakeRecorder{}
	h := MeteredLLMHandlers(mgr, nil, Metering{Agent: "bot", Ledger: rec, Requests: reg})
	resp, ok := h["llm.forward"](forwardCtx(context.Background(), ref), session.Op{Type: "llm.forward"})
	if ok || !strings.Contains(resp, "upstream_error: ") {
		t.Fatalf("provider failure = %v %s; want an upstream_error marker", ok, resp)
	}
	if len(rec.got) != 1 || rec.got[0].OK || rec.got[0].Kind != "forward" {
		t.Fatalf("ledger rows: %+v; want one failed forward row", rec.got)
	}
}
