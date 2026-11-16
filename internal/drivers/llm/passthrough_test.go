package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"agentflow/internal/core/media"
)

// TestAnthropicParallelToolCallsWithoutAChoice: parallel_tool_calls with no
// tool_choice is the shape most OpenAI-format clients send. The Messages API
// carries the switch on the choice object, so the default choice has to be
// spelled out or the setting is silently lost.
func TestAnthropicParallelToolCallsWithoutAChoice(t *testing.T) {
	var body string
	m := optsManager(t, "anthropic", anthropicTextSSE, "", &body)
	if _, err := m.Chat(context.Background(), "default",
		[]Message{{Role: "user", Content: "hi"}},
		Opts{Tools: []ToolDef{{Name: "get_weather"}}, ParallelToolCalls: ptr(false)}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !strings.Contains(body, `"disable_parallel_tool_use":true`) || !strings.Contains(body, `"type":"auto"`) {
		t.Fatalf("parallel_tool_calls:false did not reach the wire as an auto choice: %s", body)
	}
}

// TestAnthropicFoldsSystemAndDeveloperTurns: OpenAI-format clients send
// "developer" turns and more than one system message, in any position. The
// Messages API rejects those roles inside the turn list; all of them belong in
// the one system field.
func TestAnthropicFoldsSystemAndDeveloperTurns(t *testing.T) {
	var body string
	m := optsManager(t, "anthropic", anthropicTextSSE, "", &body)
	if _, err := m.Chat(context.Background(), "default", []Message{
		{Role: "system", Content: "recalled facts"},
		{Role: "system", Content: "client prompt"},
		{Role: "user", Content: "hi"},
		{Role: "developer", Content: "be brief"},
	}, Opts{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	for _, bad := range []string{`"role":"system"`, `"role":"developer"`} {
		if strings.Contains(body, bad) {
			t.Errorf("request carries %s inside messages: %s", bad, body)
		}
	}
	for _, want := range []string{"recalled facts", "client prompt", "be brief"} {
		if !strings.Contains(body, want) {
			t.Errorf("instruction %q was lost: %s", want, body)
		}
	}
}

// TestTextResponseFormatIsANoOp: {"type":"text"} is the default spelled out,
// which some client libraries always send. A provider with no response_format
// must not refuse it.
func TestTextResponseFormatIsANoOp(t *testing.T) {
	for _, tc := range []struct{ provider, canned string }{
		{"anthropic", anthropicTextSSE},
		{"gemini", geminiTextJSON},
	} {
		var body string
		m := optsManager(t, tc.provider, tc.canned, "", &body)
		_, err := m.Chat(context.Background(), "default",
			[]Message{{Role: "user", Content: "hi"}},
			Opts{ResponseFormat: map[string]any{"type": "text"}})
		if err != nil {
			t.Errorf("%s refused response_format text: %v", tc.provider, err)
		}
	}
}

// TestGeminiPassthroughDropsTemperature: a loop author who sets temperature on
// a provider that cannot express it is told so; a client's request that carries
// one (most do) still gets an answer.
func TestGeminiPassthroughDropsTemperature(t *testing.T) {
	var body string
	m := optsManager(t, "gemini", geminiTextJSON, "", &body)
	msgs := []Message{{Role: "user", Content: "hi"}}
	if _, err := m.Chat(context.Background(), "default", msgs, Opts{Temperature: ptr(0.3)}); err == nil {
		t.Fatal("a loop-set temperature must still be refused on gemini")
	}
	if _, err := m.Chat(context.Background(), "default", msgs, Opts{Temperature: ptr(0.3), Passthrough: true}); err != nil {
		t.Fatalf("a forwarded request's temperature must be dropped, not refused: %v", err)
	}
}

// TestGeminiPassthroughDoesNotFetchClientURLs: the gemini provider uploads
// media itself, so a url-sourced part means the runtime fetches the URL. For a
// URL a client supplied that is a request made from inside the deployment to
// wherever the client points it; it is refused before any connection is made.
func TestGeminiPassthroughDoesNotFetchClientURLs(t *testing.T) {
	var fetched atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched.Add(1)
		_, _ = w.Write([]byte("internal"))
	}))
	defer target.Close()

	var body string
	m := optsManager(t, "gemini", geminiTextJSON, "", &body)
	msgs := []Message{{Role: "user", Content: "look", Parts: []media.Part{
		{Type: "image", MIME: "image/png", URL: target.URL + "/latest/meta-data"},
	}}}
	_, err := m.Chat(context.Background(), "default", msgs, Opts{Passthrough: true})
	if err == nil || !strings.Contains(err.Error(), "invalid_request") {
		t.Fatalf("err = %v; want an invalid_request refusal", err)
	}
	if n := fetched.Load(); n != 0 {
		t.Fatalf("the runtime fetched a client-supplied URL %d time(s)", n)
	}
}

// TestFinishReasonAndRawArguments: the reply says why the model stopped, in
// one vocabulary, and keeps a tool call's argument text exactly as sent.
func TestFinishReasonAndRawArguments(t *testing.T) {
	const args = `{"files":[],"opt":null,"big":12345678901234567}`
	openaiSSE := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"edit\",\"arguments\":" +
		`"{\"files\":[],\"opt\":null,\"big\":12345678901234567}"` + "}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4}}\n\n" +
		"data: [DONE]\n\n"
	var body string
	m := optsManager(t, "openai", openaiSSE, "", &body)
	reply, err := m.Chat(context.Background(), "default", []Message{{Role: "user", Content: "hi"}}, Opts{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if reply.FinishReason != "length" {
		t.Errorf("finish reason = %q; want length", reply.FinishReason)
	}
	if len(reply.ToolCalls) != 1 || reply.ToolCalls[0].Raw != args {
		t.Fatalf("raw arguments = %+v; want %s", reply.ToolCalls, args)
	}

	for in, want := range map[string]string{
		"end_turn": "stop", "max_tokens": "length", "tool_use": "tool_calls",
		"refusal": "content_filter", "max_output_tokens": "length", "": "", "something_new": "",
	} {
		if got := normalizeFinish(in); got != want {
			t.Errorf("normalizeFinish(%q) = %q; want %q", in, got, want)
		}
	}
	// Anything that is not a JSON object is not kept: the decoded map, which is
	// always an object, is the safer thing to hand back.
	for _, bad := range []string{"", "  ", `{"a":`, `[1]`, `"x"`} {
		if got := rawArgs(bad); got != "" {
			t.Errorf("rawArgs(%q) = %q; want empty", bad, got)
		}
	}
}
