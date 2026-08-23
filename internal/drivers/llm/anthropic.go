package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"agentflow/internal/config"
	"agentflow/internal/core/media"
)

// Anthropic Messages API, always called with stream=true (Chat buffers it).
// https://docs.anthropic.com/en/api/messages

func anthropicOpen(ctx context.Context, client *http.Client, cfg config.Model, msgs []Message, opts Opts) (<-chan event, bool, error) {
	base := resolveBase(cfg, "https://api.anthropic.com")
	url := base + "/v1/messages"
	system, turns, err := anthropicTurns(msgs)
	if err != nil {
		return nil, false, err
	}

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
	if len(opts.Tools) > 0 {
		tools := make([]map[string]any, 0, len(opts.Tools))
		for _, t := range opts.Tools {
			tools = append(tools, map[string]any{
				"name":         t.Name,
				"description":  t.Description,
				"input_schema": t.Parameters,
			})
		}
		body["tools"] = tools
		if opts.ToolChoice != "" {
			body["tool_choice"] = map[string]any{"type": opts.ToolChoice}
		}
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
		// Anthropic emits content blocks indexed by position. A tool_use block
		// starts with id+name, then receives input_delta.partial_json fragments.
		type blk struct {
			id   string
			name string
			args strings.Builder
		}
		blocks := map[int]*blk{}
		var order []int
		for data := range sseDatas(ctx, resp.Body) {
			var ev struct {
				Type         string `json:"type"`
				Index        int    `json:"index"`
				ContentBlock *struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
					Text string `json:"text"`
				} `json:"content_block"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
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
			case "content_block_start":
				if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
					blocks[ev.Index] = &blk{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
					order = append(order, ev.Index)
				}
			case "content_block_delta":
				switch ev.Delta.Type {
				case "text_delta":
					if ev.Delta.Text != "" {
						select {
						case events <- event{delta: ev.Delta.Text}:
						case <-ctx.Done():
							return
						}
					}
				case "input_json_delta":
					if b, ok := blocks[ev.Index]; ok {
						b.args.WriteString(ev.Delta.PartialJSON)
					}
				}
			case "message_delta":
				usage.Output = ev.Usage.OutputTokens
			case "error":
				events <- event{err: fmt.Errorf("anthropic stream error: %s", data)}
				return
			}
		}
		if len(order) > 0 {
			calls := make([]ToolCall, 0, len(order))
			for _, i := range order {
				b := blocks[i]
				calls = append(calls, ToolCall{ID: b.id, Name: b.name, Args: parseArgs(b.args.String())})
			}
			events <- event{toolCalls: calls}
		}
		events <- event{usage: usage}
	}()
	return events, false, nil
}

// anthropicTurns converts the neutral Message list to Anthropic's shape: the
// leading system message is extracted (returned as the first result), and the
// remaining turns are reshaped so that assistant tool_calls become tool_use
// content blocks and "tool" role turns become user tool_result blocks. Adjacent
// tool results are merged into one user message, as Anthropic requires.
// Multimodal turns carry Parts as image/document content blocks.
func anthropicTurns(msgs []Message) (system string, turns []map[string]any, err error) {
	if len(msgs) > 0 && msgs[0].Role == "system" {
		system = msgs[0].Content
		msgs = msgs[1:]
	}
	for _, m := range msgs {
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			content := []map[string]any{}
			if m.Content != "" {
				content = append(content, map[string]any{"type": "text", "text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				content = append(content, map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Name,
					"input": tc.Args,
				})
			}
			turns = append(turns, map[string]any{"role": "assistant", "content": content})
		case m.Role == "tool":
			result := m.ToolResult
			if result == nil {
				result = m.Content
			}
			block := map[string]any{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": result}
			// Merge into a preceding user tool_result message if present.
			if n := len(turns); n > 0 {
				if prev, ok := turns[n-1]["content"].([]map[string]any); ok {
					turns[n-1]["content"] = append(prev, block)
					continue
				}
			}
			turns = append(turns, map[string]any{"role": "user", "content": []map[string]any{block}})
		default:
			if len(m.Parts) > 0 {
				content := []map[string]any{}
				if m.Content != "" {
					content = append(content, map[string]any{"type": "text", "text": m.Content})
				}
				for _, p := range m.Parts {
					blk, perr := anthropicPart(p)
					if perr != nil {
						return system, nil, perr
					}
					content = append(content, blk)
				}
				turns = append(turns, map[string]any{"role": m.Role, "content": content})
			} else {
				turns = append(turns, map[string]any{"role": m.Role, "content": m.Content})
			}
		}
	}
	return system, turns, nil
}

// anthropicPart maps one media part to an Anthropic content block. Images
// accept base64 or URL sources; PDFs are document blocks (base64 only).
// Audio and video are rejected honestly — the Messages API has no block type
// for them.
func anthropicPart(p media.Part) (map[string]any, error) {
	switch p.Type {
	case "text":
		return map[string]any{"type": "text", "text": p.Text}, nil
	case "image":
		src := map[string]any{"type": "base64", "media_type": p.MIME, "data": p.Data}
		if p.Data == "" && p.URL != "" {
			src = map[string]any{"type": "url", "url": p.URL}
		}
		return map[string]any{"type": "image", "source": src}, nil
	case "file":
		if p.MIME != "application/pdf" {
			return nil, fmt.Errorf("anthropic: document blocks support application/pdf only (got %q)", p.MIME)
		}
		if p.Data == "" {
			return nil, fmt.Errorf("anthropic: pdf part requires inline base64 data")
		}
		return map[string]any{
			"type": "document",
			"source": map[string]any{
				"type":       "base64",
				"media_type": "application/pdf",
				"data":       p.Data,
			},
		}, nil
	default:
		return nil, fmt.Errorf("anthropic does not support %s parts", p.Type)
	}
}
