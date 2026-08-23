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
	msgsOut, err := openaiMessages(msgs)
	if err != nil {
		return nil, false, err
	}

	body := map[string]any{
		"model":    cfg.Model,
		"messages": msgsOut, // openai takes system inline
		"stream":   true,
		"stream_options": map[string]any{
			"include_usage": true,
		},
	}
	if len(opts.Tools) > 0 || len(cfg.ServerTools) > 0 {
		body["tools"] = mergeServerTools(openaiTools(opts.Tools), cfg.ServerTools)
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
							ID       string `json:"id"`
							Function struct {
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
// through unchanged. Multimodal turns become content arrays of text /
// image_url / input_audio parts (vision + audio models).
func openaiMessages(msgs []Message) ([]map[string]any, error) {
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
			if len(m.Parts) > 0 {
				content, err := openaiContentParts(m)
				if err != nil {
					return nil, err
				}
				out = append(out, map[string]any{"role": m.Role, "content": content})
			} else {
				out = append(out, map[string]any{"role": m.Role, "content": m.Content})
			}
		}
	}
	return out, nil
}

// openaiContentParts builds the Chat Completions content array for one
// multimodal turn. Images take image_url (data: URI or remote URL); audio
// takes input_audio (base64, wav/mp3). PDFs and video are rejected honestly
// — Chat Completions has no inline form for them (they need the Files API).
func openaiContentParts(m Message) ([]map[string]any, error) {
	var out []map[string]any
	if m.Content != "" {
		out = append(out, map[string]any{"type": "text", "text": m.Content})
	}
	for _, p := range m.Parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				out = append(out, map[string]any{"type": "text", "text": p.Text})
			}
		case "image":
			url := p.URL
			if url == "" {
				if p.Data == "" {
					return nil, fmt.Errorf("openai: image part has neither url nor data")
				}
				url = dataURI(p)
			}
			out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
		case "audio":
			if p.Data == "" {
				return nil, fmt.Errorf("openai: audio part requires inline base64 data")
			}
			format := audioFormat(p.MIME)
			if format == "" {
				return nil, fmt.Errorf("openai: input_audio supports wav and mp3 only (got %q)", p.MIME)
			}
			out = append(out, map[string]any{
				"type":        "input_audio",
				"input_audio": map[string]any{"data": p.Data, "format": format},
			})
		default:
			return nil, fmt.Errorf("openai (chat completions) does not support %s parts; use the responses provider or pre-process the file", p.Type)
		}
	}
	if len(out) == 0 {
		out = append(out, map[string]any{"type": "text", "text": ""})
	}
	return out, nil
}

// audioFormat maps a MIME type to OpenAI's input_audio format string.
func audioFormat(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "audio/wav", "audio/wave", "audio/x-wav":
		return "wav"
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	default:
		return ""
	}
}
