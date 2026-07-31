package llm

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentflow/internal/config"
)

// sseServer returns an httptest server that asserts the request carries the
// given body substrings, then streams the canned SSE body.
func sseServer(t *testing.T, wantBody []string, sse string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		raw, _ := json.Marshal(body)
		for _, want := range wantBody {
			if !strings.Contains(string(raw), want) {
				t.Errorf("request body missing %q\nbody: %s", want, raw)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
}

func chatOnce(t *testing.T, srv *httptest.Server, provider string, msgs []Message, tools []ToolDef) (string, []ToolCall) {
	t.Helper()
	m := NewManager(map[string]config.Model{
		"default": {Provider: provider, Model: "m", BaseURL: srv.URL},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	text, calls, _, err := m.Chat(context.Background(), "default", msgs, Opts{Tools: tools, ToolChoice: "auto"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	return text, calls
}

var testTools = []ToolDef{{
	Name:        "get_weather",
	Description: "Get weather for a city",
	Parameters: map[string]any{
		"type":       "object",
		"properties": map[string]any{"city": map[string]any{"type": "string"}},
		"required":   []string{"city"},
	},
}}

func TestOpenAIToolCallStream(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_weather","arguments":""}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"ci"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"Paris\"}"}}]}}]}

data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}

data: [DONE]

`
	srv := sseServer(t, []string{`"tools"`, `"get_weather"`, `"tool_choice"`}, sse)
	defer srv.Close()

	text, calls := chatOnce(t, srv, "openai",
		[]Message{{Role: "user", Content: "weather?"}}, testTools)
	if text != "" {
		t.Errorf("expected no text, got %q", text)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d (%v)", len(calls), calls)
	}
	if calls[0].ID != "call_1" || calls[0].Name != "get_weather" {
		t.Errorf("unexpected call: %+v", calls[0])
	}
	if calls[0].Args["city"] != "Paris" {
		t.Errorf("expected args city=Paris, got %v", calls[0].Args)
	}
}

func TestAnthropicToolCallStream(t *testing.T) {
	sse := `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":12}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"Lyon\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":7}}

`
	srv := sseServer(t, []string{`"input_schema"`, `"get_weather"`}, sse)
	defer srv.Close()

	text, calls := chatOnce(t, srv, "anthropic",
		[]Message{{Role: "user", Content: "weather?"}}, testTools)
	if text != "" {
		t.Errorf("expected no text, got %q", text)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d (%v)", len(calls), calls)
	}
	if calls[0].ID != "toolu_1" || calls[0].Name != "get_weather" {
		t.Errorf("unexpected call: %+v", calls[0])
	}
	if calls[0].Args["city"] != "Lyon" {
		t.Errorf("expected args city=Lyon, got %v", calls[0].Args)
	}
}

func TestResponsesToolCallStream(t *testing.T) {
	sse := `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"fc_1","name":"get_weather"}}

data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"city\":"}

data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"Nice\"}"}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"fc_1","name":"get_weather","arguments":"{\"city\":\"Nice\"}"}}

data: {"type":"response.completed","response":{"usage":{"input_tokens":9,"output_tokens":4}}}

`
	srv := sseServer(t, []string{`"get_weather"`}, sse)
	defer srv.Close()

	text, calls := chatOnce(t, srv, "openai-responses",
		[]Message{{Role: "user", Content: "weather?"}}, testTools)
	if text != "" {
		t.Errorf("expected no text, got %q", text)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d (%v)", len(calls), calls)
	}
	if calls[0].ID != "fc_1" || calls[0].Name != "get_weather" {
		t.Errorf("unexpected call: %+v", calls[0])
	}
	if calls[0].Args["city"] != "Nice" {
		t.Errorf("expected args city=Nice, got %v", calls[0].Args)
	}
}

// TestToolTurnEcho verifies each provider reshapes an assistant tool-call turn
// plus the tool result into its native follow-up representation.
func TestToolTurnEcho(t *testing.T) {
	history := []Message{
		{Role: "user", Content: "weather in Paris?"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "call_1", Name: "get_weather", Args: map[string]any{"city": "Paris"}}}},
		{Role: "tool", ToolCallID: "call_1", ToolResult: map[string]any{"temp": "21C"}},
	}

	cases := []struct {
		provider string
		sse      string
		want     []string
	}{
		{"openai", openAITextSSE, []string{`"tool_calls"`, `"call_1"`, `"role":"tool"`, `"tool_call_id":"call_1"`}},
		{"anthropic", anthropicTextSSE, []string{`"tool_use"`, `"tool_result"`, `"tool_use_id":"call_1"`}},
		{"openai-responses", responsesTextSSE, []string{`"function_call"`, `"function_call_output"`, `"call_id":"call_1"`}},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			srv := sseServer(t, tc.want, tc.sse)
			defer srv.Close()
			text, _ := chatOnce(t, srv, tc.provider, history, testTools)
			if text == "" {
				t.Errorf("expected final answer text, got empty")
			}
		})
	}
}

const openAITextSSE = `data: {"choices":[{"delta":{"content":"It is 21C."}}]}

data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":20,"completion_tokens":6}}

data: [DONE]

`

const anthropicTextSSE = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":20}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"It is 21C."}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":6}}

`

const responsesTextSSE = `data: {"type":"response.output_text.delta","delta":"It is 21C."}

data: {"type":"response.completed","response":{"usage":{"input_tokens":20,"output_tokens":6}}}

`
