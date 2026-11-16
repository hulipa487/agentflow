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
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/responses"
	"github.com/openai/openai-go/shared"

	"agentflow/internal/config"
)

// toolChoiceName splits Opts.ToolChoice into a provider mode and a function
// name. "function:get_weather" is the named form; every other value is a mode
// the provider translates itself.
func toolChoiceName(choice string) (mode, name string) {
	if n, ok := strings.CutPrefix(choice, "function:"); ok {
		return "function", n
	}
	return choice, ""
}

// openaiResponseFormat translates the OpenAI-shaped response_format object to
// the Chat Completions SDK union. An object it cannot express is an error: the
// caller asked for a structured answer, and sending the request without it
// would produce an unstructured one while looking successful.
func openaiResponseFormat(m map[string]any) (openai.ChatCompletionNewParamsResponseFormatUnion, error) {
	var out openai.ChatCompletionNewParamsResponseFormatUnion
	typ, _ := m["type"].(string)
	switch typ {
	case "text":
		out.OfText = &shared.ResponseFormatTextParam{}
	case "json_object":
		out.OfJSONObject = &shared.ResponseFormatJSONObjectParam{}
	case "json_schema":
		js, ok := m["json_schema"].(map[string]any)
		if !ok {
			return out, fmt.Errorf("response_format.type is json_schema but response_format.json_schema is missing")
		}
		name, _ := js["name"].(string)
		if name == "" {
			return out, fmt.Errorf("response_format.json_schema.name is required")
		}
		p := shared.ResponseFormatJSONSchemaJSONSchemaParam{Name: name, Schema: js["schema"]}
		if strict, ok := js["strict"].(bool); ok {
			p.Strict = param.NewOpt(strict)
		}
		if desc, ok := js["description"].(string); ok && desc != "" {
			p.Description = param.NewOpt(desc)
		}
		out.OfJSONSchema = &shared.ResponseFormatJSONSchemaParam{JSONSchema: p}
	default:
		return out, fmt.Errorf("response_format.type %q is not supported (use text, json_object or json_schema)", typ)
	}
	return out, nil
}

// responsesResponseFormat translates the same object to the Responses API's
// text.format union. The shape is flatter there: json_schema's fields sit at
// the top of the format object.
func responsesResponseFormat(m map[string]any) (responses.ResponseFormatTextConfigUnionParam, error) {
	var out responses.ResponseFormatTextConfigUnionParam
	typ, _ := m["type"].(string)
	switch typ {
	case "text":
		out.OfText = &shared.ResponseFormatTextParam{}
	case "json_object":
		out.OfJSONObject = &shared.ResponseFormatJSONObjectParam{}
	case "json_schema":
		js, ok := m["json_schema"].(map[string]any)
		if !ok {
			// Accept the flat Responses shape too.
			js = m
		}
		name, _ := js["name"].(string)
		if name == "" {
			return out, fmt.Errorf("response_format.json_schema.name is required")
		}
		schema, _ := js["schema"].(map[string]any)
		p := responses.ResponseFormatTextJSONSchemaConfigParam{Name: name, Schema: schema}
		if strict, ok := js["strict"].(bool); ok {
			p.Strict = param.NewOpt(strict)
		}
		if desc, ok := js["description"].(string); ok && desc != "" {
			p.Description = param.NewOpt(desc)
		}
		out.OfJSONSchema = &p
	default:
		return out, fmt.Errorf("response_format.type %q is not supported (use text, json_object or json_schema)", typ)
	}
	return out, nil
}

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
	if mode, name := toolChoiceName(opts.ToolChoice); mode == "function" {
		if name == "" {
			return nil, false, fmt.Errorf("tool_choice function: needs a tool name (function:<name>)")
		}
		if len(opts.Tools) == 0 {
			return nil, false, fmt.Errorf("tool_choice names function %q but no tools were supplied", name)
		}
	}
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
