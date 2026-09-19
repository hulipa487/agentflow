package llm

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentflow/internal/config"
)

// thinkingServer records the request body and answers with the canned reply
// for one provider's wire shape (SSE for the three streaming providers,
// plain JSON for gemini's non-streaming Interactions API).
func thinkingServer(t *testing.T, provider string, body *[][]byte) *httptest.Server {
	t.Helper()
	replies := map[string]string{
		"anthropic":        anthropicTextSSE,
		"openai":           openAITextSSE,
		"openai-responses": responsesTextSSE,
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*body = append(*body, b)
		if provider == "gemini" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"steps":[{"type":"model_output","content":[{"type":"text","text":"hi"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(replies[provider]))
	}))
}

func chatThinking(t *testing.T, srv *httptest.Server, provider, modelCfg string, opts Opts) error {
	t.Helper()
	m := NewManager(map[string]config.Model{
		"default": {Provider: provider, Model: "m", BaseURL: srv.URL, Thinking: modelCfg},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, _, _, err := m.Chat(context.Background(), "default",
		[]Message{{Role: "user", Content: "hi"}}, opts)
	return err
}

func TestThinkingRequestBodies(t *testing.T) {
	cases := []struct {
		name   string
		prov   string
		cfg    string
		opts   string
		maxTok int
		want   []string
		absent []string
	}{
		// Anthropic: a level becomes budget_tokens on an enabled block, clamped
		// into the request's output budget (thinking and the answer share
		// max_tokens).
		{"anthropic high clamps to max_tokens", "anthropic", "", "high", 0, []string{`"thinking":{"budget_tokens":3072,"type":"enabled"}`}, nil},
		{"anthropic max unclamped", "anthropic", "", "max", 200000, []string{`"budget_tokens":131072`}, nil},
		{"anthropic low floors at 1024", "anthropic", "", "low", 1024, []string{`"budget_tokens":1024`}, nil},
		{"anthropic off sends nothing", "anthropic", "", "off", 0, nil, []string{`"thinking"`}},
		// OpenAI: levels map onto reasoning_effort; off is "none", max tops
		// out at high.
		{"openai medium", "openai", "", "medium", 0, []string{`"reasoning_effort":"medium"`}, nil},
		{"openai off is none", "openai", "", "off", 0, []string{`"reasoning_effort":"none"`}, nil},
		{"openai max tops out at high", "openai", "", "max", 0, []string{`"reasoning_effort":"high"`}, nil},
		{"openai unset sends nothing", "openai", "", "", 0, nil, []string{`"reasoning`}},
		// Responses API nests the effort.
		{"responses xhigh", "openai-responses", "", "xhigh", 0, []string{`"reasoning":{"effort":"xhigh"}`}, nil},
		// Gemini: a token budget; 0 disables thinking.
		{"gemini high", "gemini", "", "high", 0, []string{`"thinking_budget":16384`}, nil},
		{"gemini off disables", "gemini", "", "off", 0, []string{`"thinking_budget":0`}, nil},
		// The per-model default applies with no per-call override.
		{"model default applies", "openai", "high", "", 0, []string{`"reasoning_effort":"high"`}, nil},
		// ...and the per-call override beats the model default.
		{"per-call overrides model", "openai", "high", "low", 0, []string{`"reasoning_effort":"low"`}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var bodies [][]byte
			srv := thinkingServer(t, tc.prov, &bodies)
			defer srv.Close()
			opts := Opts{Thinking: tc.opts, MaxTokens: tc.maxTok}
			if err := chatThinking(t, srv, tc.prov, tc.cfg, opts); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if len(bodies) != 1 {
				t.Fatalf("expected 1 request, got %d", len(bodies))
			}
			raw := string(bodies[0])
			for _, want := range tc.want {
				if !strings.Contains(raw, want) {
					t.Errorf("request body missing %q\nbody: %s", want, raw)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(raw, absent) {
					t.Errorf("request body must not contain %q\nbody: %s", absent, raw)
				}
			}
		})
	}
}

func TestThinkingInvalidLevelFails(t *testing.T) {
	var bodies [][]byte
	srv := thinkingServer(t, "openai", &bodies)
	defer srv.Close()

	err := chatThinking(t, srv, "openai", "", Opts{Thinking: "banana"})
	if err == nil || !strings.Contains(err.Error(), "invalid thinking level") {
		t.Fatalf("expected an invalid-thinking-level error, got %v", err)
	}
	if len(bodies) != 0 {
		t.Errorf("an invalid level must not reach the provider; got %d requests", len(bodies))
	}

	err = chatThinking(t, srv, "openai", "huge", Opts{})
	if err == nil || !strings.Contains(err.Error(), "invalid thinking level") {
		t.Fatalf("an invalid per-model default must fail too, got %v", err)
	}
}
