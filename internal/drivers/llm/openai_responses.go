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

// openaiResponsesOpen implements the OpenAI Responses API with SSE streaming.
// https://platform.openai.com/docs/api-reference/responses
//
// The Responses API is OpenAI's newer endpoint that replaces Chat Completions
// for structured output, tool use, and reasoning models. It takes the same
// messages array but returns a different event stream shape.
//
// base_url should include /v1; the runtime appends /responses.

func openaiResponsesOpen(ctx context.Context, client *http.Client, cfg config.Model, msgs []Message, opts Opts) (<-chan event, bool, error) {
	base := resolveBase(cfg, "https://api.openai.com/v1")
	url := base + "/responses"

	// The Responses API takes an "input" array (role/content messages) rather
	// than "messages". System messages map to a top-level "instructions" field.
	system, turns := splitSystem(msgs)

	body := map[string]any{
		"model":  cfg.Model,
		"input":  turns,
		"stream": true,
	}
	if system != "" {
		body["instructions"] = system
	}
	if opts.Temperature != nil {
		body["temperature"] = *opts.Temperature
	}
	if mt := maxTokensOf(cfg, opts); mt > 0 {
		body["max_output_tokens"] = mt
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
				Type string `json:"type"`
				// response.output_text.delta carries the text chunk
				Delta string `json:"delta"`
				// response.completed carries usage
				Response struct {
					Usage struct {
						InputTokens  int `json:"input_tokens"`
						OutputTokens int `json:"output_tokens"`
					} `json:"usage"`
				} `json:"response"`
				// some compatible proxies nest usage at the top level
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				continue
			}
			switch ev.Type {
			case "response.output_text.delta":
				if ev.Delta != "" {
					select {
					case events <- event{delta: ev.Delta}:
					case <-ctx.Done():
						return
					}
				}
			case "response.completed":
				if ev.Response.Usage.InputTokens > 0 {
					usage.Input = ev.Response.Usage.InputTokens
				}
				if ev.Response.Usage.OutputTokens > 0 {
					usage.Output = ev.Response.Usage.OutputTokens
				}
				if ev.Usage.InputTokens > 0 {
					usage.Input = ev.Usage.InputTokens
				}
				if ev.Usage.OutputTokens > 0 {
					usage.Output = ev.Usage.OutputTokens
				}
			case "error":
				events <- event{err: fmt.Errorf("openai-responses stream error: %s", data)}
				return
			}
		}
		events <- event{usage: usage}
	}()
	return events, false, nil
}
