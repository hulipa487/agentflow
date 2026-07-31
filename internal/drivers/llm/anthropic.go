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

// Anthropic Messages API, always called with stream=true (Chat buffers it).
// https://docs.anthropic.com/en/api/messages

func anthropicOpen(ctx context.Context, client *http.Client, cfg config.Model, msgs []Message, opts Opts) (<-chan event, bool, error) {
	base := resolveBase(cfg, "https://api.anthropic.com")
	url := base + "/v1/messages"
	system, turns := splitSystem(msgs)

	body := map[string]any{
		"model":      cfg.Model,
		"max_tokens": maxTokensOf(cfg, opts),
		"messages":   turns,
		"stream":     true,
	}
	if system != "" {
		body["system"] = system
	}
	if opts.Temperature != nil {
		body["temperature"] = *opts.Temperature
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(mustJSON(body)))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("content-type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("x-api-key", cfg.APIKey)
	}
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := client.Do(req)
	if err != nil {
		return nil, true, err // transport errors are retryable
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
			var ev struct {
				Type  string `json:"type"`
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
				Message struct {
					Usage struct {
						InputTokens int `json:"input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				continue // keepalives / unknown shapes
			}
			switch ev.Type {
			case "message_start":
				usage.Input = ev.Message.Usage.InputTokens
			case "content_block_delta":
				if ev.Delta.Text != "" {
					select {
					case events <- event{delta: ev.Delta.Text}:
					case <-ctx.Done():
						return
					}
				}
			case "message_delta":
				usage.Output = ev.Usage.OutputTokens
			case "error":
				events <- event{err: fmt.Errorf("anthropic stream error: %s", data)}
				return
			}
		}
		events <- event{usage: usage}
	}()
	return events, false, nil
}
