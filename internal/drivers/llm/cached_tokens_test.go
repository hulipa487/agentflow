package llm

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"agentflow/internal/config"
)

// TestUsageCachedTokens: prompt-cache tokens are captured per provider. A cache
// hit is invisible in input/output — the provider counts the prompt the same
// way either way — so without these fields the ledger cannot tell a cheap turn
// from an expensive one.
func TestUsageCachedTokens(t *testing.T) {
	cases := []struct {
		name        string
		prov        string
		canned      string
		wantCached  int
		wantWritten int // cache creation: Anthropic only
	}{
		{
			name: "anthropic reports cache read and creation",
			prov: "anthropic",
			canned: `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":11,"cache_read_input_tokens":900,"cache_creation_input_tokens":120}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`,
			wantCached:  900,
			wantWritten: 120,
		},
		{
			name: "anthropic reports the split on message_delta instead",
			prov: "anthropic",
			canned: `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":11}}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":7,"cache_read_input_tokens":640,"cache_creation_input_tokens":0}}

event: message_stop
data: {"type":"message_stop"}

`,
			wantCached: 640,
		},
		{
			name: "openai chat reports cached prompt tokens",
			prov: "openai",
			canned: `data: {"choices":[{"delta":{"content":"x"}}]}

data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":1000,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":768}}}

data: [DONE]

`,
			wantCached: 768,
		},
		{
			name: "openai responses reports cached input tokens",
			prov: "openai-responses",
			canned: `data: {"type":"response.completed","response":{"usage":{"input_tokens":1000,"output_tokens":20,"input_tokens_details":{"cached_tokens":512}}}}

data: [DONE]

`,
			wantCached: 512,
		},
		{
			name:       "gemini reports cached content tokens",
			prov:       "gemini",
			canned:     `{"steps":[{"type":"model_output","content":[{"type":"text","text":"hi"}]}],"usage":{"input_tokens":1000,"output_tokens":6,"cachedContentTokenCount":640}}`,
			wantCached: 640,
		},
		{
			name: "a provider that reports no cache stays at zero",
			prov: "openai",
			canned: `data: {"choices":[{"delta":{"content":"x"}}]}

data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}

data: [DONE]

`,
			wantCached: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body string
			srv := captureServer(t, tc.prov, tc.canned, &body)
			defer srv.Close()
			m := NewManager(map[string]config.Model{
				// The genai client's Gemini backend has no keyless mode; the other
				// providers ignore it and the mock never checks it.
				"default": {Provider: tc.prov, Model: "m", BaseURL: srv.URL, APIKey: "test-key"},
			}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			reply, err := m.Chat(context.Background(), "default",
				[]Message{{Role: "user", Content: "hi"}}, Opts{})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if reply.Usage.Cached != tc.wantCached {
				t.Errorf("cached = %d, want %d (usage %+v)", reply.Usage.Cached, tc.wantCached, reply.Usage)
			}
			if reply.Usage.CacheWrite != tc.wantWritten {
				t.Errorf("cache_write = %d, want %d (usage %+v)", reply.Usage.CacheWrite, tc.wantWritten, reply.Usage)
			}
		})
	}
}
