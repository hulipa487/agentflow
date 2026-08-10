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

// geminiOpen implements the Gemini Interactions API
// (POST {base}/interactions), a non-streaming endpoint that supports native
// server-side tools such as Google Search grounding via
// tools:[{"type":"google_search"}]. base_url should include the version
// prefix (e.g. https://generativelanguage.googleapis.com/v1beta); the runtime
// appends /interactions.
//
// Unlike the OpenAI providers, this API is request/response (no SSE), so the
// whole reply is emitted as a single text delta followed by usage. The model
// string travels in the body's "model" field, not the URL path. Messages are
// flattened into a single "input" string: system context first, then the
// conversation. Client-side function tools are not mapped (the interactions
// schema differs); only cfg.ServerTools (e.g. google_search) are sent.
func geminiOpen(ctx context.Context, client *http.Client, cfg config.Model, msgs []Message, opts Opts) (<-chan event, bool, error) {
	base := resolveBase(cfg, "https://generativelanguage.googleapis.com/v1beta")
	url := base + "/interactions"

	body := map[string]any{
		"model": cfg.Model,
		"input": geminiInput(msgs),
	}
	if len(cfg.ServerTools) > 0 {
		tools := make([]map[string]any, 0, len(cfg.ServerTools))
		for _, s := range cfg.ServerTools {
			tools = append(tools, map[string]any{"type": s})
		}
		body["tools"] = tools
	}
	if mt := maxTokensOf(cfg, opts); mt > 0 {
		body["max_output_tokens"] = mt
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
		// Gemini accepts the key as a header; some proxies expect ?key=.
		req.Header.Set("x-goog-api-key", cfg.APIKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		retryable, serr := statusError(resp.StatusCode, b)
		return nil, retryable, fmt.Errorf("%s -> %w", url, serr)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, false, err
	}

	// Parse the steps array; the final answer is the model_output step's text.
	var out struct {
		Steps []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"steps"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false, fmt.Errorf("gemini: decode response: %w", err)
	}
	var text string
	for _, st := range out.Steps {
		if st.Type != "model_output" {
			continue
		}
		for _, c := range st.Content {
			if c.Type == "text" {
				text += c.Text
			}
		}
	}

	events := make(chan event, 2)
	go func() {
		defer close(events)
		if text != "" {
			events <- event{delta: text}
		}
		events <- event{usage: Usage{Input: out.Usage.InputTokens, Output: out.Usage.OutputTokens}}
	}()
	return events, false, nil
}

// geminiInput flattens the conversation into a single input string for the
// interactions API. System messages are prefixed as context; each turn is
// rendered "role: content". Tool turns are inlined as their result text.
func geminiInput(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		content := m.Content
		if m.Role == "tool" && m.ToolResult != nil {
			content = string(mustJSON(m.ToolResult))
		}
		if content == "" {
			continue
		}
		role := m.Role
		if role == "" {
			role = "user"
		}
		fmt.Fprintf(&b, "%s: %s\n\n", role, content)
	}
	return strings.TrimSpace(b.String())
}
