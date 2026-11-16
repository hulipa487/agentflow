package llm

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"agentflow/internal/config"
)

func ptr[T any](v T) *T { return &v }

// geminiTextJSON is a minimal interactions response: one text step and usage.
const geminiTextJSON = `{"steps":[{"type":"model_output","content":[{"type":"text","text":"hi"}]}],"usage":{"input_tokens":5,"output_tokens":6}}`

func optsManager(t *testing.T, provider, canned, baseURL string, body *string) *Manager {
	t.Helper()
	srv := captureServer(t, provider, canned, body)
	t.Cleanup(srv.Close)
	return NewManager(map[string]config.Model{
		"default": {Provider: provider, Model: "m", BaseURL: srv.URL, APIKey: "test-key"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestOpenAIChatOptsWire pins the Chat Completions serialization of every new
// option. A field that is accepted but never reaches the wire is a setting that
// silently does nothing; this is the check that says otherwise.
func TestOpenAIChatOptsWire(t *testing.T) {
	var body string
	m := optsManager(t, "openai", openAITextSSE, "", &body)
	_, err := m.Chat(context.Background(), "default",
		[]Message{{Role: "user", Content: "hi"}},
		Opts{
			Tools:             []ToolDef{{Name: "get_weather"}},
			ToolChoice:        "function:get_weather",
			Temperature:       ptr(0.2),
			TopP:              ptr(0.5),
			ParallelToolCalls: ptr(false),
			Stop:              []string{"END"},
			Seed:              ptr(int64(42)),
			ResponseFormat: map[string]any{
				"type": "json_schema",
				"json_schema": map[string]any{
					"name":   "answer",
					"schema": map[string]any{"type": "object"},
					"strict": true,
				},
			},
		})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	for _, want := range []string{
		`"top_p":0.5`,
		`"parallel_tool_calls":false`,
		`"stop":["END"]`,
		`"seed":42`,
		`"response_format":{"json_schema":{"name":"answer","strict":true,"schema":{"type":"object"}},"type":"json_schema"}`,
		`"tool_choice":{"function":{"name":"get_weather"},"type":"function"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("request missing %s\nbody: %s", want, body)
		}
	}
}

// TestOpenAIChatRequiredToolChoice: the string "required" is passed through.
func TestOpenAIChatRequiredToolChoice(t *testing.T) {
	var body string
	m := optsManager(t, "openai", openAITextSSE, "", &body)
	if _, err := m.Chat(context.Background(), "default",
		[]Message{{Role: "user", Content: "hi"}},
		Opts{Tools: []ToolDef{{Name: "get_weather"}}, ToolChoice: "required"}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !strings.Contains(body, `"tool_choice":"required"`) {
		t.Fatalf("request missing required tool_choice: %s", body)
	}
}

// TestResponsesOptsWire: the Responses API maps top_p, parallel calls, the text
// format and a named tool choice; stop and seed have no equivalent there and
// are dropped silently, per the plan.
func TestResponsesOptsWire(t *testing.T) {
	var body string
	m := optsManager(t, "openai-responses", responsesTextSSE, "", &body)
	_, err := m.Chat(context.Background(), "default",
		[]Message{{Role: "user", Content: "hi"}},
		Opts{
			Tools:             []ToolDef{{Name: "get_weather"}},
			ToolChoice:        "function:get_weather",
			TopP:              ptr(0.4),
			ParallelToolCalls: ptr(true),
			Stop:              []string{"END"},
			Seed:              ptr(int64(7)),
			ResponseFormat: map[string]any{
				"type": "json_schema",
				"json_schema": map[string]any{
					"name":   "answer",
					"schema": map[string]any{"type": "object"},
				},
			},
		})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	for _, want := range []string{
		`"top_p":0.4`,
		`"parallel_tool_calls":true`,
		`"text":{"format":{"name":"answer","schema":{"type":"object"},"type":"json_schema"}}`,
		`"tool_choice":{"name":"get_weather","type":"function"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("request missing %s\nbody: %s", want, body)
		}
	}
	for _, absent := range []string{`"stop"`, `"seed"`} {
		if strings.Contains(body, absent) {
			t.Errorf("Responses request carries %s, which the API does not model: %s", absent, body)
		}
	}
}

// TestAnthropicOptsWire: top_p, stop sequences and the tool-choice modes map;
// response_format is refused rather than silently ignored.
func TestAnthropicOptsWire(t *testing.T) {
	var body string
	m := optsManager(t, "anthropic", anthropicTextSSE, "", &body)
	_, err := m.Chat(context.Background(), "default",
		[]Message{{Role: "user", Content: "hi"}},
		Opts{
			Tools:             []ToolDef{{Name: "get_weather"}},
			ToolChoice:        "function:get_weather",
			TopP:              ptr(0.3),
			Stop:              []string{"END"},
			ParallelToolCalls: ptr(false),
		})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	for _, want := range []string{
		`"top_p":0.3`,
		`"stop_sequences":["END"]`,
		`"tool_choice":{"name":"get_weather","disable_parallel_tool_use":true,"type":"tool"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("request missing %s\nbody: %s", want, body)
		}
	}

	_, err = m.Chat(context.Background(), "default",
		[]Message{{Role: "user", Content: "hi"}},
		Opts{ResponseFormat: map[string]any{"type": "json_object"}})
	if err == nil || !strings.Contains(err.Error(), "response_format has no equivalent") {
		t.Fatalf("response_format = %v; want a clear refusal", err)
	}
}

// TestAnthropicRequiredToolChoice: "required" maps to Anthropic's "any".
func TestAnthropicRequiredToolChoice(t *testing.T) {
	var body string
	m := optsManager(t, "anthropic", anthropicTextSSE, "", &body)
	if _, err := m.Chat(context.Background(), "default",
		[]Message{{Role: "user", Content: "hi"}},
		Opts{Tools: []ToolDef{{Name: "x"}}, ToolChoice: "required"}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !strings.Contains(body, `"tool_choice":{"type":"any"}`) {
		t.Fatalf("request missing any tool_choice: %s", body)
	}
}

// TestGeminiOptsWire: seed and stop sequences are expressible; response_format
// is refused.
func TestGeminiOptsWire(t *testing.T) {
	var body string
	m := optsManager(t, "gemini", geminiTextJSON, "", &body)
	if _, err := m.Chat(context.Background(), "default",
		[]Message{{Role: "user", Content: "hi"}},
		Opts{Seed: ptr(int64(9)), Stop: []string{"END"}, TopP: ptr(0.1)}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	for _, want := range []string{`"seed":9`, `"stop_sequences":["END"]`} {
		if !strings.Contains(body, want) {
			t.Errorf("request missing %s\nbody: %s", want, body)
		}
	}
	if strings.Contains(body, `"top_p"`) {
		t.Errorf("gemini request carries top_p, which the API does not model: %s", body)
	}
	_, err := m.Chat(context.Background(), "default",
		[]Message{{Role: "user", Content: "hi"}},
		Opts{ResponseFormat: map[string]any{"type": "json_object"}})
	if err == nil || !strings.Contains(err.Error(), "response_format has no equivalent") {
		t.Fatalf("response_format = %v; want a clear refusal", err)
	}
}

// TestNamedToolChoiceNeedsTools is provider-neutral: naming a function without
// supplying any is a request bug, refused before a provider sees it.
func TestNamedToolChoiceNeedsTools(t *testing.T) {
	var body string
	m := optsManager(t, "openai", openAITextSSE, "", &body)
	_, err := m.Chat(context.Background(), "default",
		[]Message{{Role: "user", Content: "hi"}},
		Opts{ToolChoice: "function:get_weather"})
	if err == nil || !strings.Contains(err.Error(), "no tools were supplied") {
		t.Fatalf("named choice without tools = %v; want a refusal", err)
	}
	if body != "" {
		t.Fatalf("a refused request reached the provider: %s", body)
	}
}

// TestResponseFormatUnknownShape: an object the provider cannot express is an
// error, not a silently-unstructured answer.
func TestResponseFormatUnknownShape(t *testing.T) {
	var body string
	m := optsManager(t, "openai", openAITextSSE, "", &body)
	_, err := m.Chat(context.Background(), "default",
		[]Message{{Role: "user", Content: "hi"}},
		Opts{ResponseFormat: map[string]any{"type": "yaml"}})
	if err == nil || !strings.Contains(err.Error(), "response_format.type") {
		t.Fatalf("unknown response_format = %v; want a clear refusal", err)
	}
}
