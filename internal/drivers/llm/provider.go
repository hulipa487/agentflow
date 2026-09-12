package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"agentflow/internal/config"
)

// resolveBase returns the configured base URL (trailing slash trimmed so the
// provider path joins cleanly), falling back to the provider's official host
// only when base_url is unset. For API-compatible proxies/local endpoints,
// set base_url explicitly — it always wins.
func resolveBase(cfg config.Model, def string) string {
	if cfg.BaseURL == "" {
		return def
	}
	return strings.TrimSuffix(cfg.BaseURL, "/")
}

// openProvider routes to the provider implementation.
// Supported providers:
//   - "anthropic"    — Anthropic Messages API (also compatible proxies)
//   - "openai"       — OpenAI Chat Completions API (/v1/chat/completions)
//   - "openai-responses" — OpenAI Responses API (/v1/responses)
//   - "gemini"       — Gemini Interactions API (/v1beta/interactions)
func openProvider(ctx context.Context, client *http.Client, cfg config.Model, msgs []Message, opts Opts) (<-chan event, bool, error) {
	switch cfg.Provider {
	case "anthropic":
		return anthropicOpen(ctx, client, cfg, msgs, opts)
	case "openai":
		return openaiChatOpen(ctx, client, cfg, msgs, opts)
	case "openai-responses":
		return openaiResponsesOpen(ctx, client, cfg, msgs, opts)
	case "gemini":
		return geminiOpen(ctx, client, cfg, msgs, opts)
	}
	return nil, false, fmt.Errorf("unsupported provider %q (use anthropic, openai, openai-responses, or gemini)", cfg.Provider)
}

// statusError classifies an HTTP error status. Retryable on 429 and 5xx.
func statusError(status int, body []byte) (retryable bool, err error) {
	snippet := body
	if len(snippet) > 300 {
		snippet = snippet[:300]
	}
	retryable = status == 429 || status >= 500
	return retryable, fmt.Errorf("provider returned %d: %s", status, snippet)
}

func maxTokensOf(cfg config.Model, opts Opts) int {
	if opts.MaxTokens > 0 {
		return opts.MaxTokens
	}
	if cfg.MaxTokens > 0 {
		return cfg.MaxTokens
	}
	return 4096
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // request shapes are static; a marshal failure is a bug
	}
	return b
}

// parseArgs unmarshals a tool-call arguments JSON string into a map. An empty
// or malformed body yields an empty map (the loop still sees the call name).
func parseArgs(raw string) map[string]any {
	args := map[string]any{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return args
	}
	_ = json.Unmarshal([]byte(raw), &args)
	return args
}

// openaiTools converts the provider-agnostic ToolDef list to the OpenAI Chat
// Completions request shape: [{type:"function",function:{name,description,parameters}}].
func openaiTools(tools []ToolDef) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			},
		})
	}
	return out
}

// serverToolEntry builds the native request-body entry for a provider-side
// tool. Most server tools (OpenAI/xAI web_search, x_search, etc.) take a bare
// {type:<name>} entry with no function payload — the provider supplies and
// executes the tool. Unknown names fall back to a function-shaped stub so a
// typo doesn't silently produce a tool-less request.
func serverToolEntry(name string) map[string]any {
	switch name {
	case "web_search", "x_search", "web_search_preview", "code_interpreter", "file_search", "image_generation", "mcp":
		return map[string]any{"type": name}
	default:
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}
	}
}

// mergeServerTools appends provider-native server-side tool entries (from
// cfg.ServerTools) to the client-side function tools list. Server tools run
// inside the provider's completion; they carry no parameters from the client.
func mergeServerTools(fns []map[string]any, server []string) []map[string]any {
	for _, s := range server {
		fns = append(fns, serverToolEntry(s))
	}
	return fns
}
