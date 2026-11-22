package telemetry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/core/safety"
	"agentflow/internal/core/session"
)

func TestResolveEndpointAndLangfuseAuth(t *testing.T) {
	ep, err := resolveEndpoint("langfuse", "")
	if err != nil || ep != "https://cloud.langfuse.com/api/public/otel/v1/traces" {
		t.Fatalf("langfuse default endpoint = %q, err=%v", ep, err)
	}
	ep, err = resolveEndpoint("langfuse", "https://lf.example.com/")
	if err != nil || ep != "https://lf.example.com/api/public/otel/v1/traces" {
		t.Fatalf("langfuse custom host = %q, err=%v", ep, err)
	}
	ep, err = resolveEndpoint("otlp", "https://otel.example.com")
	if err != nil || ep != "https://otel.example.com/v1/traces" {
		t.Fatalf("otlp host = %q, err=%v", ep, err)
	}
	if _, err := New(Options{Provider: "langfuse", Endpoint: "https://lf.example.com"}, nil); err == nil {
		t.Fatal("expected error when langfuse keys and Authorization header are both omitted")
	}
}

func TestExporterOTLPAndLangfuseHierarchy(t *testing.T) {
	var (
		mu       sync.Mutex
		authSeen string
		pathSeen string
		reqs     []otlpExportRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req otlpExportRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("invalid OTLP JSON: %v\n%s", err, string(body))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		authSeen = r.Header.Get("Authorization")
		pathSeen = r.URL.Path
		reqs = append(reqs, req)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	exp, err := New(Options{
		Provider:        "langfuse",
		Endpoint:        srv.URL,
		ServiceName:     "agentflow-test",
		PublicKey:       "pk-lf-1",
		SecretKey:       "sk-lf-2",
		SampleRate:      1.0,
		IncludePayloads: false,
		BatchSize:       32,
		FlushInterval:   time.Hour,
	}, nil)
	if err != nil {
		t.Fatalf("New exporter: %v", err)
	}

	t0 := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	exp.Trace(session.TraceEvent{
		Ts: t0, Agent: "helper", SessionID: "helper|studio:th-1", Thread: "th-1", Turn: "turn-1",
		Kind: "turn.start", Messages: 1, Text: "secret user prompt",
	})
	exp.Trace(session.TraceEvent{
		Ts: t0.Add(10 * time.Millisecond), Agent: "helper", SessionID: "helper|studio:th-1", Thread: "th-1", Turn: "turn-1",
		Kind: "safety", Op: "ingress", OK: true, Err: "steady-directive-applied",
		Safety: &safety.SafetyMeta{Classifier: "mod", Label: "safe", Action: "allow", Score: 0.02, LatencyMS: 5},
	})
	exp.Trace(session.TraceEvent{
		Ts: t0.Add(20 * time.Millisecond), Agent: "helper", SessionID: "helper|studio:th-1", Thread: "th-1", Turn: "turn-1",
		Kind: "op.start", Op: "llm.chat", Model: "gpt-test", Messages: 2, Text: "prompt body",
	})
	exp.Trace(session.TraceEvent{
		Ts: t0.Add(140 * time.Millisecond), Agent: "helper", SessionID: "helper|studio:th-1", Thread: "th-1", Turn: "turn-1",
		Kind: "op.end", Op: "llm.chat", Model: "gpt-test", OK: true, DurationMS: 120,
		TokensIn: 11, TokensOut: 22, TokensReasoning: 5, Text: "model reply body",
	})
	exp.Trace(session.TraceEvent{
		Ts: t0.Add(150 * time.Millisecond), Agent: "helper", SessionID: "helper|studio:th-1", Thread: "th-1", Turn: "turn-1",
		Kind: "reply", OK: true, Text: "final answer",
	})
	exp.Trace(session.TraceEvent{
		Ts: t0.Add(160 * time.Millisecond), Agent: "helper", SessionID: "helper|studio:th-1", Thread: "th-1", Turn: "turn-1",
		Kind: "turn.end",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := exp.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if pathSeen != "/api/public/otel/v1/traces" {
		t.Fatalf("path = %q, want /api/public/otel/v1/traces", pathSeen)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("pk-lf-1:sk-lf-2"))
	if authSeen != wantAuth {
		t.Fatalf("Authorization = %q, want %q", authSeen, wantAuth)
	}
	var all []otlpSpan
	for _, r := range reqs {
		for _, rs := range r.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				all = append(all, ss.Spans...)
			}
		}
	}
	if len(all) != 4 {
		t.Fatalf("exported %d spans, want 4 (safety, op.end, reply, turn.end root)", len(all))
	}

	var root, llmSpan otlpSpan
	for _, sp := range all {
		if sp.ParentSpanID == "" {
			root = sp
		}
		if strings.HasPrefix(sp.Name, "llm.chat") {
			llmSpan = sp
		}
		for _, attr := range sp.Attributes {
			if strings.HasSuffix(attr.Key, ".input") || strings.HasSuffix(attr.Key, ".output") || attr.Key == "gen_ai.prompt" || attr.Key == "gen_ai.completion" {
				t.Fatalf("payload attribute %q leaked when IncludePayloads=false", attr.Key)
			}
		}
	}
	if root.SpanID == "" || root.Name != "agent.turn:helper" {
		t.Fatalf("missing root turn span: %+v", root)
	}
	if llmSpan.TraceID != root.TraceID || llmSpan.ParentSpanID != root.SpanID {
		t.Fatalf("llm span (%s -> parent %s) did not link to root (%s/%s)", llmSpan.TraceID, llmSpan.ParentSpanID, root.TraceID, root.SpanID)
	}
	foundTokens := false
	for _, attr := range llmSpan.Attributes {
		if attr.Key == "gen_ai.usage.output_tokens" && attr.Value.IntValue != nil && *attr.Value.IntValue == "22" {
			foundTokens = true
		}
	}
	if !foundTokens {
		t.Fatalf("llm span missing gen_ai.usage.output_tokens=22: %+v", llmSpan.Attributes)
	}
}

func TestExporterIncludePayloadsAndSamplingZero(t *testing.T) {
	var (
		mu   sync.Mutex
		reqs []otlpExportRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req otlpExportRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		reqs = append(reqs, req)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	// SampleRate 0 drops all traces cleanly without HTTP requests.
	zeroExp, err := New(Options{Provider: "otlp", Endpoint: srv.URL, SampleRate: 0, FlushInterval: time.Hour}, nil)
	if err != nil {
		t.Fatalf("New zeroExp: %v", err)
	}
	zeroExp.Trace(session.TraceEvent{Ts: time.Now(), Agent: "a", SessionID: "s1", Turn: "t1", Kind: "turn.start"})
	zeroExp.Trace(session.TraceEvent{Ts: time.Now(), Agent: "a", SessionID: "s1", Turn: "t1", Kind: "turn.end"})
	_ = zeroExp.Shutdown(context.Background())

	mu.Lock()
	if len(reqs) != 0 {
		t.Fatalf("expected 0 requests when SampleRate=0, got %d", len(reqs))
	}
	mu.Unlock()

	// IncludePayloads captures trace input and output.
	fullExp, err := New(Options{
		Provider: "otlp", Endpoint: srv.URL, SampleRate: 1, IncludePayloads: true, BatchSize: 1, FlushInterval: time.Hour,
	}, nil)
	if err != nil {
		t.Fatalf("New fullExp: %v", err)
	}
	t0 := time.Now()
	fullExp.Trace(session.TraceEvent{Ts: t0, Agent: "a", SessionID: "s1", Turn: "t2", Kind: "turn.start", Text: "hi"})
	fullExp.Trace(session.TraceEvent{Ts: t0.Add(time.Millisecond), Agent: "a", SessionID: "s1", Turn: "t2", Kind: "reply", OK: true, Text: "hello"})
	fullExp.Trace(session.TraceEvent{Ts: t0.Add(2 * time.Millisecond), Agent: "a", SessionID: "s1", Turn: "t2", Kind: "turn.end"})
	_ = fullExp.Shutdown(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(reqs) == 0 {
		t.Fatal("expected exported payloads")
	}
	sawPrompt := false
	for _, r := range reqs {
		for _, sp := range r.ResourceSpans[0].ScopeSpans[0].Spans {
			for _, attr := range sp.Attributes {
				if attr.Key == "gen_ai.prompt" && attr.Value.StringValue != nil && *attr.Value.StringValue == "hi" {
					sawPrompt = true
				}
			}
		}
	}
	if !sawPrompt {
		t.Fatal("expected gen_ai.prompt=hi on root turn span when IncludePayloads=true")
	}
}
