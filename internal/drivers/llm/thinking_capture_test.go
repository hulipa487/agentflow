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

// captureServer records the request body and answers with a canned reply
// (SSE for the streaming providers, plain JSON for gemini).
func captureServer(t *testing.T, provider, canned string, body *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*body = string(b)
		if provider == "gemini" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(canned))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(canned))
	}))
}

// TestThinkingCapturePerProvider: whatever reasoning content a provider sends
// is surfaced on the reply — joined text for reading, raw blocks where the
// provider defines them — without the caller asking for anything beyond the
// thinking level.
func TestThinkingCapturePerProvider(t *testing.T) {
	cases := []struct {
		name       string
		prov       string
		canned     string
		thinking   string // per-call level
		wantText   string
		wantThink  string
		wantBlocks string   // "" = none; otherwise mustJSON(substrings) of the blocks
		wantReq    []string // substrings required in the request body
		absentReq  []string
	}{
		{
			name: "anthropic thinking + redacted blocks",
			prov: "anthropic",
			canned: `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":11}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Why "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"2+2?"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-abc"}}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"OPAQUE"}}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"text"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"4"}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`,
			thinking:   "low",
			wantText:   "4",
			wantThink:  "Why 2+2?",
			wantBlocks: `[{"signature":"sig-abc","thinking":"Why 2+2?","type":"thinking"},{"data":"OPAQUE","type":"redacted_thinking"}]`,
		},
		{
			name: "openai reasoning_content with token details",
			prov: "openai",
			canned: `data: {"choices":[{"delta":{"reasoning_content":"chain "}}]}

data: {"choices":[{"delta":{"reasoning_content":"of thought"}}]}

data: {"choices":[{"delta":{"content":"answer"}}]}

data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":10,"completion_tokens":25,"completion_tokens_details":{"reasoning_tokens":17}}}

data: [DONE]

`,
			wantText:   "answer",
			wantThink:  "chain of thought",
			wantBlocks: "",
		},
		{
			name: "openai-router reasoning field alias",
			prov: "openai",
			canned: `data: {"choices":[{"delta":{"reasoning":"via alias"}}]}

data: {"choices":[{"delta":{"content":"ok"}}]}

data: [DONE]

`,
			wantText:   "ok",
			wantThink:  "via alias",
			wantBlocks: "",
		},
		{
			name: "responses reasoning items captured verbatim",
			prov: "openai-responses",
			canned: `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}

data: {"type":"response.reasoning_summary_text.delta","delta":"ponder"}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"pondering"}]}}

data: {"type":"response.output_text.delta","delta":"ok"}

data: {"type":"response.completed","response":{"usage":{"input_tokens":9,"output_tokens":14,"output_tokens_details":{"reasoning_tokens":8}}}}

data: [DONE]

`,
			wantText:   "ok",
			wantThink:  "pondering",
			wantBlocks: `"id":"rs_1"`,
		},
		{
			name:       "gemini thought parts",
			prov:       "gemini",
			canned:     `{"steps":[{"type":"model_output","content":[{"type":"text","text":"silent step","thought":true},{"type":"text","text":"hi there"}]}],"usage":{"input_tokens":5,"output_tokens":6}}`,
			thinking:   "low",
			wantText:   "hi there",
			wantThink:  "silent step",
			wantBlocks: `"thought":true`,
			wantReq:    []string{`"include_thoughts":true`},
		},
		{
			name:       "gemini without a level asks for nothing",
			prov:       "gemini",
			canned:     `{"steps":[{"type":"model_output","content":[{"type":"text","text":"hi"}]}],"usage":{"input_tokens":5,"output_tokens":6}}`,
			wantText:   "hi",
			wantThink:  "",
			wantBlocks: "",
			absentReq:  []string{`"thinking_config"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body string
			srv := captureServer(t, tc.prov, tc.canned, &body)
			defer srv.Close()
			m := NewManager(map[string]config.Model{
				"default": {Provider: tc.prov, Model: "m", BaseURL: srv.URL},
			}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			reply, err := m.Chat(context.Background(), "default",
				[]Message{{Role: "user", Content: "hi"}}, Opts{Thinking: tc.thinking})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if reply.Text != tc.wantText {
				t.Errorf("text %q, want %q", reply.Text, tc.wantText)
			}
			if reply.Thinking != tc.wantThink {
				t.Errorf("thinking %q, want %q", reply.Thinking, tc.wantThink)
			}
			if tc.wantBlocks == "" {
				if len(reply.ThinkingBlocks) != 0 {
					t.Errorf("want no blocks, got %s", mustJSON(reply.ThinkingBlocks))
				}
			} else if !strings.Contains(string(mustJSON(reply.ThinkingBlocks)), tc.wantBlocks) {
				t.Errorf("blocks missing %q, got %s", tc.wantBlocks, mustJSON(reply.ThinkingBlocks))
			}
			for _, want := range tc.wantReq {
				if !strings.Contains(body, want) {
					t.Errorf("request missing %q\nbody: %s", want, body)
				}
			}
			for _, absent := range tc.absentReq {
				if strings.Contains(body, absent) {
					t.Errorf("request must not contain %q\nbody: %s", absent, body)
				}
			}
		})
	}
}

// TestUsageReasoningTokens: the OpenAI-shaped providers report a separate
// reasoning-token count when the provider sends the details object; 0 means
// the provider folds thinking into output (anthropic, gemini).
func TestUsageReasoningTokens(t *testing.T) {
	cases := []struct {
		prov   string
		canned string
		want   int
	}{
		{"openai", `data: {"choices":[{"delta":{"content":"x"}}]}

data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":1,"completion_tokens":9,"completion_tokens_details":{"reasoning_tokens":6}}}

data: [DONE]

`, 6},
		{"openai-responses", `data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":9,"output_tokens_details":{"reasoning_tokens":6}}}}

data: [DONE]

`, 6},
	}
	for _, tc := range cases {
		t.Run(tc.prov, func(t *testing.T) {
			var body string
			srv := captureServer(t, tc.prov, tc.canned, &body)
			defer srv.Close()
			m := NewManager(map[string]config.Model{
				"default": {Provider: tc.prov, Model: "m", BaseURL: srv.URL},
			}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			reply, err := m.Chat(context.Background(), "default", []Message{{Role: "user", Content: "hi"}}, Opts{})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if reply.Usage.Reasoning != tc.want {
				t.Errorf("reasoning tokens %d, want %d", reply.Usage.Reasoning, tc.want)
			}
		})
	}
}

// TestThinkingCaptureStreamTerminal: the stream path surfaces the same
// terminal thinking payload on its done frame.
func TestThinkingCaptureStreamTerminal(t *testing.T) {
	var body string
	srv := captureServer(t, "openai", `data: {"choices":[{"delta":{"reasoning_content":"deep"}}]}

data: {"choices":[{"delta":{"content":"ok"}}]}

data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":1,"completion_tokens":3}}

data: [DONE]

`, &body)
	defer srv.Close()
	m := NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	id, err := m.StreamOpen(context.Background(), "default", []Message{{Role: "user", Content: "hi"}}, Opts{})
	if err != nil {
		t.Fatalf("StreamOpen: %v", err)
	}
	var text string
	var last Frame
	for {
		frame, err := m.StreamNext(context.Background(), id)
		if err != nil {
			t.Fatalf("StreamNext: %v", err)
		}
		text += frame.Delta
		last = frame
		if frame.Done {
			break
		}
	}
	if text != "ok" {
		t.Errorf("text %q", text)
	}
	if last.Thinking != "deep" {
		t.Errorf("terminal frame thinking %q", last.Thinking)
	}
	if last.Usage.Output != 3 {
		t.Errorf("terminal frame usage %+v", last.Usage)
	}
}
