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

// openaiResponsesOpen implements the OpenAI Responses API with SSE streaming.
// https://platform.openai.com/docs/api-reference/responses
//
// The Responses API is OpenAI's newer endpoint that replaces Chat Completions
// for structured output, tool use, and reasoning models. It takes an "input"
// array of items (messages, function calls, function outputs) and returns a
// different event stream shape.
//
// base_url should include /v1; the runtime appends /responses.

func openaiResponsesOpen(ctx context.Context, client *http.Client, cfg config.Model, msgs []Message, opts Opts) (<-chan event, bool, error) {
	base := resolveBase(cfg, "https://api.openai.com/v1")
	url := base + "/responses"

	// The Responses API takes an "input" array of items. System messages map to
	// a top-level "instructions" field; assistant tool_calls become function_call
	// items and "tool" turns become function_call_output items.
	system, items := responsesItems(msgs)

	body := map[string]any{
		"model":  cfg.Model,
		"input":  items,
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
	if len(opts.Tools) > 0 {
		// Responses uses a flat function shape (no "function" wrapper).
		tools := make([]map[string]any, 0, len(opts.Tools))
		for _, t := range opts.Tools {
			tools = append(tools, map[string]any{
				"type":        "function",
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			})
		}
		body["tools"] = tools
		if opts.ToolChoice != "" {
			body["tool_choice"] = opts.ToolChoice
		}
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
		// Function-call accumulation keyed by output_index: arguments arrive as
		// fragments to concatenate; id/name come from the first delta that has
		// them (or from response.output_item.done).
		type acc struct {
			id   string
			name string
			args strings.Builder
		}
		accByIndex := map[int]*acc{}
		var order []int
		getAcc := func(i int) *acc {
			a, ok := accByIndex[i]
			if !ok {
				a = &acc{}
				accByIndex[i] = a
				order = append(order, i)
			}
			return a
		}
		for data := range sseDatas(ctx, resp.Body) {
			if data == "[DONE]" {
				break
			}
			var ev struct {
				Type        string `json:"type"`
				OutputIndex int    `json:"output_index"`
				// response.output_text.delta / response.function_call_arguments.delta
				Delta string `json:"delta"`
				// id/name arrive on response.output_item.added and
				// response.output_item.done
				CallID string `json:"call_id"`
				Name   string `json:"name"`
				Item   *struct {
					Type      string `json:"type"`
					CallID    string `json:"call_id"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"item"`
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
			case "response.output_item.added":
				if ev.Item != nil && ev.Item.Type == "function_call" {
					a := getAcc(ev.OutputIndex)
					a.id = ev.Item.CallID
					a.name = ev.Item.Name
				}
			case "response.function_call_arguments.delta":
				a := getAcc(ev.OutputIndex)
				if ev.CallID != "" {
					a.id = ev.CallID
				}
				if ev.Name != "" {
					a.name = ev.Name
				}
				a.args.WriteString(ev.Delta)
			case "response.output_item.done":
				if ev.Item != nil && ev.Item.Type == "function_call" {
					a := getAcc(ev.OutputIndex)
					a.id = ev.Item.CallID
					a.name = ev.Item.Name
					// done carries the full arguments string; if no deltas were
					// seen (some proxies skip them), use it directly.
					if a.args.Len() == 0 {
						a.args.WriteString(ev.Item.Arguments)
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

// responsesItems converts the provider-neutral Message list to the Responses
// API "input" shape: the leading system message becomes "instructions"
// (returned as the first result); an assistant turn with tool_calls becomes
// function_call items; a "tool" turn becomes a function_call_output item.
// Plain turns become {type:"message", role, content} items.
func responsesItems(msgs []Message) (system string, items []map[string]any) {
	if len(msgs) > 0 && msgs[0].Role == "system" {
		system = msgs[0].Content
		msgs = msgs[1:]
	}
	for _, m := range msgs {
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			if m.Content != "" {
				items = append(items, map[string]any{"type": "message", "role": "assistant", "content": m.Content})
			}
			for _, tc := range m.ToolCalls {
				items = append(items, map[string]any{
					"type":      "function_call",
					"call_id":   tc.ID,
					"name":      tc.Name,
					"arguments": string(mustJSON(tc.Args)),
				})
			}
		case m.Role == "tool":
			output := m.Content
			if m.ToolResult != nil {
				output = string(mustJSON(m.ToolResult))
			}
			items = append(items, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  output,
			})
		default:
			items = append(items, map[string]any{"type": "message", "role": m.Role, "content": m.Content})
		}
	}
	return system, items
}
