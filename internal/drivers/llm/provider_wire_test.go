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

func testManager(baseURL, provider string) *Manager {
	return NewManager(map[string]config.Model{
		// The genai client's Gemini backend has no keyless mode; the other
		// providers ignore it and the mocks never check it.
		"default": {Provider: provider, Model: "m", BaseURL: baseURL, APIKey: "test-key"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestOpenAIBaseURLKeepsItsV1Prefix: base_url carries /v1 (the OpenAI SDK
// convention) and the runtime joins only the endpoint path onto it, so
// compatible servers (Ollama, vLLM, LiteLLM, OpenRouter) resolve without a
// doubled /v1. The official SDK joins a *relative* path onto the base it was
// given, and option.WithBaseURL only normalises a trailing slash, so passing
// the configured base straight through preserves the old
// base+"/chat/completions" arithmetic exactly. Nothing else pins the final
// URL, and the URL is the whole contract, so pin it here: a base without a
// path (an httptest address, say) must still land on /chat/completions rather
// than /v1/chat/completions.
func TestOpenAIBaseURLKeepsItsV1Prefix(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		switch {
		case strings.HasSuffix(r.URL.Path, "/embeddings"):
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}],"usage":{"prompt_tokens":1}}`))
		case strings.HasSuffix(r.URL.Path, "/responses"):
			responsesEmptyReply()(w, r)
		default:
			openaiEmptyReply()(w, r)
		}
	}))
	defer srv.Close()

	cases := []struct {
		name     string
		provider string
		base     string
		want     string
		run      func(m *Manager) error
	}{
		{"chat keeps /v1", "openai", srv.URL + "/v1", "/v1/chat/completions",
			func(m *Manager) error {
				_, err := m.Chat(context.Background(), "default", []Message{{Role: "user", Content: "hi"}}, Opts{})
				return err
			}},
		{"responses keeps /v1", "openai-responses", srv.URL + "/v1", "/v1/responses",
			func(m *Manager) error {
				_, err := m.Chat(context.Background(), "default", []Message{{Role: "user", Content: "hi"}}, Opts{})
				return err
			}},
		{"embeddings keeps /v1", "openai", srv.URL + "/v1", "/v1/embeddings",
			func(m *Manager) error {
				_, _, err := m.Embed(context.Background(), "default", []string{"a"})
				return err
			}},
		// A base with no path of its own (an httptest address) must not gain a
		// /v1 — that is what a doubled /v1 would look like to Ollama/vLLM.
		{"chat without /v1 stays bare", "openai", srv.URL, "/chat/completions",
			func(m *Manager) error {
				_, err := m.Chat(context.Background(), "default", []Message{{Role: "user", Content: "hi"}}, Opts{})
				return err
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path = ""
			m := NewManager(map[string]config.Model{
				"default": {Provider: tc.provider, Model: "m", BaseURL: tc.base},
			}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err := tc.run(m); err != nil {
				t.Fatalf("request: %v", err)
			}
			if path != tc.want {
				t.Fatalf("request path %q, want %q", path, tc.want)
			}
		})
	}
}

// TestOpenAIChatSendsServerTools: a provider-native server tool is a bare
// {"type":<name>} entry with no function payload — the provider supplies and
// executes it — which the SDK's typed tool struct cannot represent (its Type is
// pinned to "function"), so it is built through param.Override. What matters is
// that it reaches the wire verbatim: the runtime's contract is that
// server_tools carries provider-specific names and only the native entry is
// injected.
func TestOpenAIChatSendsServerTools(t *testing.T) {
	var body string
	srv := captureServer(t, "openai", openAITextSSE, &body)
	defer srv.Close()

	m := NewManager(map[string]config.Model{
		"default": {Provider: "openai", Model: "m", BaseURL: srv.URL, ServerTools: []string{"web_search", "x_search"}},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := m.Chat(context.Background(), "default", []Message{{Role: "user", Content: "hi"}}, Opts{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	for _, want := range []string{`"type":"web_search"`, `"type":"x_search"`} {
		if !strings.Contains(body, want) {
			t.Errorf("request missing %q\nbody: %s", want, body)
		}
	}
}

// TestResponsesServerTools: server tools go out under their configured name
// rather than mapped onto the SDK's typed variants. That matters in both
// directions — OpenAI spells its Responses web-search tool web_search_preview,
// so a typed mapping would rewrite a configured web_search; and an xAI
// extension like x_search has no variant at all, so a typed mapping would have
// to refuse it. The config reference promises provider-specific strings pass
// through, so all three go out as written.
func TestResponsesServerTools(t *testing.T) {
	var body string
	srv := captureServer(t, "openai-responses", responsesTextSSE, &body)
	defer srv.Close()

	m := NewManager(map[string]config.Model{
		"default": {Provider: "openai-responses", Model: "m", BaseURL: srv.URL,
			ServerTools: []string{"web_search", "x_search", "code_interpreter"}},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := m.Chat(context.Background(), "default", []Message{{Role: "user", Content: "hi"}}, Opts{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	for _, want := range []string{`"type":"web_search"`, `"type":"x_search"`, `"type":"code_interpreter"`} {
		if !strings.Contains(body, want) {
			t.Errorf("request missing %q\nbody: %s", want, body)
		}
	}
	if strings.Contains(body, "web_search_preview") {
		t.Errorf("the configured name was rewritten to the SDK's spelling\nbody: %s", body)
	}
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

// TestGeminiBaseURLPath: base_url is documented as the version-prefixed prefix
// that /interactions is appended to, while the SDK composes
// {BaseURL}/{APIVersion}/interactions instead — so the configured prefix is
// split at its last path segment and handed over as those two knobs. The URL is
// the whole contract for a compatible endpoint, so pin what each shape resolves
// to: the default host must land on the same /v1beta/interactions the
// hand-rolled client posted to, a prefix that already names a version keeps it,
// and a prefix with a path of its own keeps that path in front of /interactions.
func TestGeminiBaseURLPath(t *testing.T) {
	// The default is checked without a request: it is the same
	// {origin}/{version}/interactions the hand-rolled client built.
	if b, v := splitGeminiBase(defaultGeminiBase); b != "https://generativelanguage.googleapis.com" || v != "v1beta" {
		t.Errorf("default base splits to (%q, %q)", b, v)
	}

	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"steps":[{"type":"model_output","content":[{"type":"text","text":"hi"}]}],"usage":{"total_input_tokens":1,"total_output_tokens":1}}`))
	}))
	defer srv.Close()

	cases := []struct{ name, base, want string }{
		{"a bare address gains the version this provider targets", srv.URL, "/v1beta/interactions"},
		{"a versioned prefix keeps its version", srv.URL + "/v1beta", "/v1beta/interactions"},
		{"a path prefix keeps its path", srv.URL + "/gemini", "/gemini/interactions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path = ""
			m := testManager(tc.base, "gemini")
			if _, err := m.Chat(context.Background(), "default", []Message{{Role: "user", Content: "hi"}}, Opts{}); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if path != tc.want {
				t.Fatalf("request path %q, want %q", path, tc.want)
			}
		})
	}
}

// TestGeminiUsageReachesTheReply: usage is not decoration — it feeds the
// per-user ledger and the agent budget, so a reply whose counts the driver
// fails to read bills as a free turn. The genai Usage type models only the
// total_* keys (total_input_tokens / total_output_tokens / total_cached_tokens,
// which is the schema the Interactions API publishes); the input_tokens /
// output_tokens pair and the legacy camelCase cachedContentTokenCount are not
// fields on it and are dropped on decode, which is a silent zero for anything
// reading them. Both spellings are pinned here, reading the reply the runtime
// actually hands back.
func TestGeminiUsageReachesTheReply(t *testing.T) {
	cases := []struct {
		name   string
		canned string
		in     int
		out    int
		cached int
	}{
		{
			name:   "the total_* shape the API schema defines",
			canned: `{"steps":[{"type":"model_output","content":[{"type":"text","text":"hi"}]}],"usage":{"total_input_tokens":11,"total_output_tokens":7,"total_cached_tokens":5}}`,
			in:     11, out: 7, cached: 5,
		},
		{
			name:   "the alias shape a compatible endpoint may send",
			canned: `{"steps":[{"type":"model_output","content":[{"type":"text","text":"hi"}]}],"usage":{"input_tokens":11,"output_tokens":7,"cachedContentTokenCount":5}}`,
			in:     11, out: 7, cached: 5,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body string
			srv := captureServer(t, "gemini", tc.canned, &body)
			defer srv.Close()
			m := testManager(srv.URL, "gemini")
			reply, err := m.Chat(context.Background(), "default",
				[]Message{{Role: "user", Content: "hi"}}, Opts{})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if reply.Usage.Input != tc.in || reply.Usage.Output != tc.out || reply.Usage.Cached != tc.cached {
				t.Fatalf("usage %+v, want input=%d output=%d cached=%d", reply.Usage, tc.in, tc.out, tc.cached)
			}
		})
	}
}

// TestOpenAISystemTurn: the two OpenAI providers place the system turn by
// different routes — Chat Completions has no instructions field, so it rides
// inline as a system role; the Responses API lifts it to top-level
// instructions. Neither is covered elsewhere, and the system prompt is the
// field every caller sets, so a wrong role mapping would silently drop it.
func TestOpenAISystemTurn(t *testing.T) {
	cases := []struct {
		provider string
		canned   string
		want     string
		absent   string
	}{
		// Inline for chat... and never duplicated into the Responses
		// instructions key.
		{"openai", openAITextSSE, `"role":"system"`, `"instructions"`},
		{"openai-responses", responsesTextSSE, `"instructions":"be brief"`, `"role":"system"`},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			var body string
			srv := captureServer(t, tc.provider, tc.canned, &body)
			defer srv.Close()
			reply, err := testManager(srv.URL, tc.provider).Chat(context.Background(), "default", []Message{
				{Role: "system", Content: "be brief"},
				{Role: "user", Content: "hi"},
			}, Opts{})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if reply.Text == "" {
				t.Errorf("expected the reply text, got none")
			}
			if !strings.Contains(body, tc.want) {
				t.Errorf("body missing %q\nbody: %s", tc.want, body)
			}
			if strings.Contains(body, tc.absent) {
				t.Errorf("body must not contain %q\nbody: %s", tc.absent, body)
			}
		})
	}
}

// TestOpenAIRealisticStreams feeds each OpenAI-backed streaming provider one
// realistic stream, for one reason: the official SDK's SSE reader aborts the
// whole stream — erroring the reply — when a payload will not decode into its
// types, and every other fixture in this package is hand-written to exactly
// the shape the driver reads. A too-thin fixture would therefore stay green
// while production failed on the first real response. A real provider sends
// far more: chunk metadata, a role-only opening delta, content_part events,
// and a `part` object the SDK models as its own sub-union.
func TestOpenAIRealisticStreams(t *testing.T) {
	const chatStream = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","system_fingerprint":"fp","choices":[{"index":0,"delta":{"role":"assistant","content":""},"logprobs":null,"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","system_fingerprint":"fp","choices":[{"index":0,"delta":{"content":"hi"},"logprobs":null,"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","system_fingerprint":"fp","choices":[{"index":0,"delta":{},"logprobs":null,"finish_reason":"stop"}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","system_fingerprint":"fp","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4,"prompt_tokens_details":{"cached_tokens":2,"audio_tokens":0},"completion_tokens_details":{"reasoning_tokens":0,"audio_tokens":0,"accepted_prediction_tokens":0,"rejected_prediction_tokens":0}}}

data: [DONE]

`
	const responsesStream = `data: {"type":"response.created","response":{"id":"resp_1","object":"response","created_at":1,"status":"in_progress","model":"gpt-4o","output":[],"parallel_tool_calls":true,"tools":[],"error":null,"incomplete_details":null,"instructions":null,"metadata":{},"temperature":1,"top_p":1,"max_output_tokens":null,"previous_response_id":null,"reasoning":{"effort":null,"summary":null},"text":{"format":{"type":"text"}},"truncation":"disabled","usage":null,"user":null}}

data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","status":"in_progress","content":[],"role":"assistant"}}

data: {"type":"response.content_part.added","item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","annotations":[],"text":""}}

data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hi","logprobs":[]}

data: {"type":"response.output_text.done","item_id":"msg_1","output_index":0,"content_index":0,"text":"hi","logprobs":[]}

data: {"type":"response.content_part.done","item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","annotations":[],"text":"hi"}}

data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","status":"completed","content":[{"type":"output_text","annotations":[],"text":"hi"}],"role":"assistant"}}

data: {"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt-4o","output":[],"parallel_tool_calls":true,"tools":[],"error":null,"incomplete_details":null,"instructions":null,"metadata":{},"temperature":1,"top_p":1,"max_output_tokens":null,"previous_response_id":null,"reasoning":{"effort":null,"summary":null},"text":{"format":{"type":"text"}},"truncation":"disabled","usage":{"input_tokens":9,"input_tokens_details":{"cached_tokens":4},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":11},"user":null}}

data: [DONE]

`
	cases := []struct {
		provider string
		sse      string
		wantIn   int
		wantCach int
	}{
		{"openai", chatStream, 3, 2},
		{"openai-responses", responsesStream, 9, 4},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			var body string
			srv := captureServer(t, tc.provider, tc.sse, &body)
			defer srv.Close()
			reply, err := testManager(srv.URL, tc.provider).Chat(context.Background(), "default",
				[]Message{{Role: "user", Content: "hi"}}, Opts{})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if reply.Text != "hi" {
				t.Errorf("text %q, want %q", reply.Text, "hi")
			}
			// Cached prompt tokens ride along on both shapes; losing them would
			// cost the ledger a cache hit it already paid for.
			if reply.Usage.Input != tc.wantIn {
				t.Errorf("input tokens %d, want %d", reply.Usage.Input, tc.wantIn)
			}
			if reply.Usage.Cached != tc.wantCach {
				t.Errorf("cached tokens %d, want %d", reply.Usage.Cached, tc.wantCach)
			}
		})
	}
}

// TestOpenAIRealisticResponsesToolStream is the function-call counterpart: the
// arguments arrive as deltas that must concatenate, and the done item carries
// the call_id/name the deltas themselves do not.
func TestOpenAIRealisticResponsesToolStream(t *testing.T) {
	const sse = `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","status":"in_progress","arguments":"","call_id":"call_1","name":"get_weather"}}

data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"city\":"}

data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"\"Paris\"}"}

data: {"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"arguments":"{\"city\":\"Paris\"}"}

data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","status":"completed","arguments":"{\"city\":\"Paris\"}","call_id":"call_1","name":"get_weather"}}

data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":9,"output_tokens":2,"total_tokens":11}}}

data: [DONE]

`
	var body string
	srv := captureServer(t, "openai-responses", sse, &body)
	defer srv.Close()
	reply, err := testManager(srv.URL, "openai-responses").Chat(context.Background(), "default",
		[]Message{{Role: "user", Content: "hi"}}, Opts{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(reply.ToolCalls) != 1 {
		t.Fatalf("tool calls %+v", reply.ToolCalls)
	}
	if c := reply.ToolCalls[0]; c.ID != "call_1" || c.Name != "get_weather" || c.Args["city"] != "Paris" {
		t.Fatalf("tool call %+v", c)
	}
}

// TestResponsesToolCallIdentOnTheDelta: OpenAI puts call_id and name on the
// output_item events. OpenAI-compatible proxies put them on the arguments
// delta instead, where the SDK's event union has no field for them — so
// without reading the event's own JSON the call would arrive nameless, and a
// loop cannot dispatch a call with no name. The hand-rolled reader had this
// tolerance; this pins that it survived the SDK conversion.
func TestResponsesToolCallIdentOnTheDelta(t *testing.T) {
	const sse = `data: {"type":"response.function_call_arguments.delta","output_index":0,"call_id":"call_9","name":"get_weather","delta":"{\"city\":\"Rome\"}"}

data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}

data: [DONE]

`
	var body string
	srv := captureServer(t, "openai-responses", sse, &body)
	defer srv.Close()
	reply, err := testManager(srv.URL, "openai-responses").Chat(context.Background(), "default",
		[]Message{{Role: "user", Content: "hi"}}, Opts{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(reply.ToolCalls) != 1 {
		t.Fatalf("tool calls %+v", reply.ToolCalls)
	}
	if c := reply.ToolCalls[0]; c.ID != "call_9" || c.Name != "get_weather" || c.Args["city"] != "Rome" {
		t.Fatalf("tool call %+v", c)
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

// TestGeminiServerTools: cfg.ServerTools reach the request as typed Tool
// entries. google_search has a real member in the SDK's Tool union, and every
// other configured name rides the union's UnknownRaw member as the bare
// {"type":<name>} entry the hand-rolled client sent — so a name the runtime
// invents still reaches the provider instead of being dropped. Pinned here
// because that union marshals through a discriminator/raw-split that would
// otherwise change the wire silently.
func TestGeminiServerTools(t *testing.T) {
	var body string
	srv := captureServer(t, "gemini",
		`{"steps":[{"type":"model_output","content":[{"type":"text","text":"hi"}]}],"usage":{"total_input_tokens":1,"total_output_tokens":1}}`,
		&body)
	defer srv.Close()

	m := NewManager(map[string]config.Model{
		"default": {Provider: "gemini", Model: "m", BaseURL: srv.URL, APIKey: "test-key",
			ServerTools: []string{"google_search", "url_context"}},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := m.Chat(context.Background(), "default", []Message{{Role: "user", Content: "hi"}}, Opts{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	for _, want := range []string{`"type":"google_search"`, `"type":"url_context"`} {
		if !strings.Contains(body, want) {
			t.Errorf("request missing %q\nbody: %s", want, body)
		}
	}
}
