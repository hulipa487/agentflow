package llm

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"agentflow/internal/config"
)

func testManager(baseURL, provider string) *Manager {
	return NewManager(map[string]config.Model{
		"default": {Provider: provider, Model: "m", BaseURL: baseURL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// assertToolResultTurns checks that every turn carrying a tool_result has the
// expected role, and that exactly `want` blocks appear overall.
func assertToolResultTurns(t *testing.T, body, role string, want int) {
	t.Helper()
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("request body is not JSON: %v\n%s", err, body)
	}
	seen := 0
	for _, msg := range req.Messages {
		raw, _ := json.Marshal(msg.Content)
		n := strings.Count(string(raw), `"tool_result"`)
		if n == 0 {
			continue
		}
		if msg.Role != role {
			t.Errorf("a turn carrying tool_result has role %q; Anthropic requires %q\nbody: %s",
				msg.Role, role, body)
		}
		seen += n
	}
	if seen != want {
		t.Errorf("found %d tool_result block(s); want %d\nbody: %s", seen, want, body)
	}
}

// TestAnthropicToolResultJoinsAUserTurn: Anthropic requires a tool_result to
// live in a *user* turn. The merge that folds adjacent results into one user
// message keyed on the content type alone — and the assistant tool_use branch
// builds the same []map[string]any content — so the first tool_result was
// appended to the assistant message carrying its tool_use. Anthropic rejects
// that, so every real tool round-trip 400ed. The existing assertions check
// substrings of the outgoing body, which cannot see which turn a block is in.
func TestAnthropicToolResultJoinsAUserTurn(t *testing.T) {
	var body string
	srv := captureServer(t, "anthropic", anthropicTextSSE, &body)
	defer srv.Close()

	m := testManager(srv.URL, "anthropic")
	history := []Message{
		{Role: "user", Content: "weather in Paris?"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Name: "get_weather", Args: map[string]any{"city": "Paris"}}}},
		{Role: "tool", ToolCallID: "call_1", ToolResult: map[string]any{"temp": "21C"}},
	}
	if _, err := m.Chat(context.Background(), "default", history, Opts{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	assertToolResultTurns(t, body, "user", 1)
}

// TestAnthropicAdjacentToolResultsShareOneUserTurn: folding *adjacent* results
// into a single user message is the behaviour the merge exists for. The role
// check must not cost it.
func TestAnthropicAdjacentToolResultsShareOneUserTurn(t *testing.T) {
	var body string
	srv := captureServer(t, "anthropic", anthropicTextSSE, &body)
	defer srv.Close()

	m := testManager(srv.URL, "anthropic")
	history := []Message{
		{Role: "user", Content: "weather and time in Paris?"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "call_1", Name: "get_weather", Args: map[string]any{"city": "Paris"}},
			{ID: "call_2", Name: "get_time", Args: map[string]any{"city": "Paris"}},
		}},
		{Role: "tool", ToolCallID: "call_1", ToolResult: map[string]any{"temp": "21C"}},
		{Role: "tool", ToolCallID: "call_2", ToolResult: map[string]any{"time": "09:00"}},
	}
	if _, err := m.Chat(context.Background(), "default", history, Opts{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	assertToolResultTurns(t, body, "user", 2)
}

// TestGeminiRefusesClientTools: the interactions provider does not map
// client-side function tools. Dropping the list silently left a tool-using loop
// looking broken — the model simply never calls anything and nothing says why —
// so the call is refused instead, naming the path that does work.
func TestGeminiRefusesClientTools(t *testing.T) {
	m := testManager("http://127.0.0.1:1", "gemini")
	_, err := m.Chat(context.Background(), "default",
		[]Message{{Role: "user", Content: "hi"}}, Opts{Tools: testTools})
	if err == nil {
		t.Fatal("gemini must refuse client-side function tools rather than drop them")
	}
	if !strings.Contains(err.Error(), "server_tools") {
		t.Fatalf("error %q does not name the supported path", err)
	}

	// Without tools the provider is unaffected.
	if _, err := m.Chat(context.Background(), "default",
		[]Message{{Role: "user", Content: "hi"}}, Opts{}); err == nil {
		t.Fatal("expected the unreachable base_url to fail; got no error")
	}
}

// TestStreamCarriesUsagePastAToolCall: every provider emits the tool-call frame
// immediately *before* the usage frame. StreamNext returned Done on the
// tool-call frame, so a tool-call turn reported zero tokens — and usage feeds
// the per-user ledger and the agent budget. That path also never closed the
// stream, so the entry, its cancel func, the response body and the provider
// goroutine survived until the process exited.
func TestStreamCarriesUsagePastAToolCall(t *testing.T) {
	const sse = `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":""}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"Paris\"}"}}]}}]}

data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}

data: [DONE]

`
	var body string
	srv := captureServer(t, "openai", sse, &body)
	defer srv.Close()

	m := testManager(srv.URL, "openai")
	ctx := context.Background()
	id, err := m.StreamOpen(ctx, "default", []Message{{Role: "user", Content: "weather?"}}, Opts{Tools: testTools})
	if err != nil {
		t.Fatalf("StreamOpen: %v", err)
	}

	var final Frame
	for {
		f, err := m.StreamNext(ctx, id)
		if err != nil {
			t.Fatalf("StreamNext: %v", err)
		}
		if f.Done {
			final = f
			break
		}
	}

	if len(final.ToolCalls) != 1 {
		t.Fatalf("terminal frame lost the tool call: %+v", final)
	}
	if final.Usage.Input == 0 || final.Usage.Output == 0 {
		t.Errorf("terminal frame lost the usage the provider sends after the tool call: %+v", final)
	}

	// The stream must be released once it reports done. A leaked entry is what
	// kept the provider goroutine and its response body alive.
	m.mu.Lock()
	_, still := m.streams[id]
	m.mu.Unlock()
	if still {
		t.Fatal("the stream stayed registered after reporting done")
	}
}

// TestStreamEndsOnCloseNotOnAnEmptyDelta: a usage-only frame carries no delta,
// and an empty delta reads to a Lua `for` as end-of-stream — the iterator stops
// and the caller silently loses the rest of the reply.
func TestStreamEndsOnCloseNotOnAnEmptyDelta(t *testing.T) {
	const sse = `data: {"choices":[{"delta":{"content":"one "}}]}

data: {"choices":[{"delta":{}}]}

data: {"choices":[{"delta":{"content":"two"}}]}

data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}

data: [DONE]

`
	var body string
	srv := captureServer(t, "openai", sse, &body)
	defer srv.Close()

	m := testManager(srv.URL, "openai")
	ctx := context.Background()
	id, err := m.StreamOpen(ctx, "default", []Message{{Role: "user", Content: "hi"}}, Opts{})
	if err != nil {
		t.Fatalf("StreamOpen: %v", err)
	}

	var text string
	for {
		f, err := m.StreamNext(ctx, id)
		if err != nil {
			t.Fatalf("StreamNext: %v", err)
		}
		if f.Done {
			break
		}
		if f.Delta == "" {
			t.Fatalf("an empty delta frame reached the caller; a Lua for-loop reads that as end-of-stream")
		}
		text += f.Delta
	}
	if text != "one two" {
		t.Fatalf("deltas = %q; want %q", text, "one two")
	}
}
