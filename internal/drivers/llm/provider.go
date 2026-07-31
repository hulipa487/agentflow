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
func openProvider(ctx context.Context, client *http.Client, cfg config.Model, msgs []Message, opts Opts) (<-chan event, bool, error) {
	switch cfg.Provider {
	case "anthropic":
		return anthropicOpen(ctx, client, cfg, msgs, opts)
	case "openai":
		return openaiChatOpen(ctx, client, cfg, msgs, opts)
	case "openai-responses":
		return openaiResponsesOpen(ctx, client, cfg, msgs, opts)
	}
	return nil, false, fmt.Errorf("unsupported provider %q (use anthropic, openai, or openai-responses)", cfg.Provider)
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

// splitSystem separates the leading system message (providers model it
// differently); the rest must be user/assistant turns.
func splitSystem(msgs []Message) (system string, turns []Message) {
	turns = msgs
	if len(msgs) > 0 && msgs[0].Role == "system" {
		system = msgs[0].Content
		turns = msgs[1:]
	}
	return system, turns
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // request shapes are static; a marshal failure is a bug
	}
	return b
}
