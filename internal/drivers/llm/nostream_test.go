package llm

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"agentflow/internal/config"
)

// A model with `stream: false` is called with one ordinary request, for
// endpoints whose streaming breaks on some responses (a proxy that cannot
// relay streamed tool calls). The reply carries the same text, tool calls and
// usage a stream would have.
func TestOpenAIChatWithoutStreaming(t *testing.T) {
	var sawStream any = "unset"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if v, ok := body["stream"]; ok {
			sawStream = v
		} else {
			sawStream = nil
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"let me look","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer srv.Close()

	off := false
	m := NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "m", BaseURL: srv.URL, Stream: &off},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	reply, err := m.Chat(context.Background(), "default",
		[]Message{{Role: "user", Content: "weather?"}}, Opts{Tools: testTools, ToolChoice: "auto"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if sawStream == true {
		t.Fatal("the request still asked for a stream")
	}
	if reply.Text != "let me look" {
		t.Errorf("text = %q", reply.Text)
	}
	if len(reply.ToolCalls) != 1 || reply.ToolCalls[0].Name != "get_weather" || reply.ToolCalls[0].Args["city"] != "Paris" {
		t.Fatalf("tool calls = %+v", reply.ToolCalls)
	}
	if reply.Usage.Input != 10 || reply.Usage.Output != 5 {
		t.Errorf("usage = %+v", reply.Usage)
	}
}
