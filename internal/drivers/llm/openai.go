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
		"messages": openaiMessages(msgs), // openai takes system inline
		"stream":   true,
		"stream_options": map[string]any{
			"include_usage": true,
		},
	}
	if len(opts.Tools) > 0 {
		body["tools"] = openaiTools(opts.Tools)
		if opts.ToolChoice != "" {
			body["tool_choice"] = opts.ToolChoice
		}
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
		// Tool-call accumulation: one entry per delta index. The first delta
		// for an index carries function.name; subsequent deltas carry
		// fragments of function.arguments that must be concatenated.
		type acc struct {
			id   string
			name string
			args strings.Builder
		}
		accByIndex := map[int]*acc{}
		var order []int
		for data := range sseDatas(ctx, resp.Body) {
			if data == "[DONE]" {
				break
			}
			var ev struct {
				Choices []struct {
					Delta struct {
						Content   string `json:"content"`
						ToolCalls []struct {
							Index    int    `json:"index"`
							ID        string `json:"id"`
							Function  struct {
								Name      string `json:"name"`
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
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
			if len(ev.Choices) > 0 {
				d := ev.Choices[0].Delta
				if d.Content != "" {
					select {
					case events <- event{delta: d.Content}:
					case <-ctx.Done():
						return
					}
				}
				for _, tc := range d.ToolCalls {
					a, ok := accByIndex[tc.Index]
					if !ok {
						a = &acc{id: tc.ID, name: tc.Function.Name}
						accByIndex[tc.Index] = a
						order = append(order, tc.Index)
					}
					if tc.ID != "" {
						a.id = tc.ID
					}
					if tc.Function.Name != "" {
						a.name = tc.Function.Name
					}
					if tc.Function.Arguments != "" {
						a.args.WriteString(tc.Function.Arguments)
					}
				}
			}
		}
		if len(order) > 0 {
			calls := make([]ToolCall, 0, len(order))
			for _, i := range order {
				a := accByIndex[i]
				calls = append(calls, ToolCall{ID: a.id, Name: a.name, Args: parseArgs(a.args.String())})
			}
			events <- event{toolCalls: calls}
		}
		events <- event{usage: usage}
	}()
	return events, false, nil
}

// openaiMessages reshapes the provider-neutral Message list into OpenAI's
// native tool-turn representation: an assistant turn with tool_calls becomes
// {role:"assistant", content, tool_calls:[...]}; a "tool" role turn becomes
// {role:"tool", tool_call_id, content:json(tool_result)}. Plain turns pass
// through unchanged.
func openaiMessages(msgs []Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			tcs := make([]map[string]any, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				tcs = append(tcs, map[string]any{
					"id":   tc.ID,
					"type": "function",
					"function": map[string]any{
						"name":      tc.Name,
						"arguments": string(mustJSON(tc.Args)),
					},
				})
			}
			out = append(out, map[string]any{"role": "assistant", "content": m.Content, "tool_calls": tcs})
		case m.Role == "tool":
			content := m.Content
			if m.ToolResult != nil {
				content = string(mustJSON(m.ToolResult))
			}
			out = append(out, map[string]any{"role": "tool", "tool_call_id": m.ToolCallID, "content": content})
		default:
			out = append(out, map[string]any{"role": m.Role, "content": m.Content})
		}
	}
	return out
}
