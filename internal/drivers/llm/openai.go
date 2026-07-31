package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"agentflow/internal/config"
)

// openaiChatOpen implements the OpenAI Chat Completions API with SSE streaming.
// https://platform.openai.com/docs/api-reference/chat/create
//
// base_url should include /v1 (matching the OpenAI SDK convention); the
// runtime appends only /chat/completions so compatible servers (Ollama, vLLM,
// LiteLLM, OpenRouter) resolve correctly without a doubled /v1.

func openaiChatOpen(ctx context.Context, client *http.Client, cfg config.Model, msgs []Message, opts Opts) (<-chan event, bool, error) {
	base := resolveBase(cfg, "https://api.openai.com/v1")
	url := base + "/chat/completions"

	body := map[string]any{
		"model":    cfg.Model,
		"messages": msgs, // openai takes system inline
		"stream":   true,
		"stream_options": map[string]any{
			"include_usage": true,
		},
	}
	if opts.Temperature != nil {
		body["temperature"] = *opts.Temperature
	}
	if mt := maxTokensOf(cfg, opts); mt > 0 {
		body["max_tokens"] = mt
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(mustJSON(body)))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("content-type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("authorization", "Bearer "+cfg.APIKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, true, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		retryable, serr := statusError(resp.StatusCode, b)
		return nil, retryable, fmt.Errorf("%s -> %w", url, serr)
	}

	events := make(chan event, 16)
	go func() {
		defer close(events)
		defer resp.Body.Close()
		var usage Usage
		for data := range sseDatas(ctx, resp.Body) {
			if data == "[DONE]" {
				break
			}
			var ev struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
				Usage *struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				continue
			}
			if ev.Usage != nil {
				usage.Input = ev.Usage.PromptTokens
				usage.Output = ev.Usage.CompletionTokens
			}
			if len(ev.Choices) > 0 && ev.Choices[0].Delta.Content != "" {
				select {
				case events <- event{delta: ev.Choices[0].Delta.Content}:
				case <-ctx.Done():
					return
				}
			}
		}
		events <- event{usage: usage}
	}()
	return events, false, nil
}
