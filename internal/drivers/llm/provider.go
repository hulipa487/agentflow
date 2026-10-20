package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go"

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
	return retryable, &httpStatusError{status: status, msg: fmt.Sprintf("provider returned %d: %s", status, snippet)}
}

// httpStatusError is a provider failure the hand-rolled paths (rerank, the
// Gemini status fallback) report. It keeps the status so isUnauthorized can
// read it without parsing the message.
type httpStatusError struct {
	status int
	msg    string
}

func (e *httpStatusError) Error() string { return e.msg }

// isUnauthorized reports whether a provider rejected the request's credential
// (HTTP 401), across every SDK error shape this package produces.
func isUnauthorized(err error) bool {
	if err == nil {
		return false
	}
	var se *httpStatusError
	if errors.As(err, &se) {
		return se.status == http.StatusUnauthorized
	}
	var oe *openai.Error
	if errors.As(err, &oe) {
		return oe.StatusCode == http.StatusUnauthorized
	}
	var ae *anthropic.Error
	if errors.As(err, &ae) {
		return ae.StatusCode == http.StatusUnauthorized
	}
	return geminiStatus(err) == http.StatusUnauthorized
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
