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
// (POST {base}/interactions), the request/response endpoint that supports
// server-side tools such as Google Search grounding via
// tools:[{"type":"google_search"}]. base_url should include the version prefix
// (e.g. https://generativelanguage.googleapis.com/v1beta); the runtime appends
// /interactions. The model string rides in the body's "model" field, not the
// URL; the key goes in the x-goog-api-key header.
//
// This API is non-streaming (no SSE), so the reply is emitted as a single text
// delta followed by usage. The conversation flattens into one "input" string
// (system context first) — unless any turn carries media parts, in which case
// "input" becomes an array of typed parts ({"type":"text"} and media parts
// referencing uploaded files). Media never goes inline: every media part is
// uploaded to the Gemini Files API (POST /upload/v1beta/files, resumable
// protocol) and referenced by URI — {"type":"image|audio|video|document",
// "uri", "mime_type"}. Files are auto-deleted by Google after 48 hours; a
// process-level cache dedupes uploads of the same content within that window.
// Client-side function tools are not mapped (the interactions tool schema
// differs); only cfg.ServerTools are sent, as {"type":<name>} entries. The
// interactions API rejects max_output_tokens, so no token cap is sent.
func geminiOpen(ctx context.Context, client *http.Client, cfg config.Model, msgs []Message, opts Opts) (<-chan event, bool, error) {
	base := resolveBase(cfg, "https://generativelanguage.googleapis.com/v1beta")
	url := base + "/interactions"

	input, err := geminiInput(ctx, newGeminiFiles(base, cfg.APIKey, client), msgs)
	if err != nil {
		return nil, false, err
	}
	thinking, err := thinkingOf(cfg, opts)
	if err != nil {
		return nil, false, err
	}
	body := map[string]any{
		"model": cfg.Model,
		"input": input,
	}
	if thinking != "" {
		if budget, ok := geminiThinkingBudget[thinking]; ok {
			tc := map[string]any{"thinking_budget": budget}
			if thinking != ThinkingOff {
				// Thought summaries are returned only when asked for; ask
				// whenever thinking is actually on. With off (budget 0) or
				// unset there is nothing to summarize, so the request stays
				// byte-identical to before.
				tc["include_thoughts"] = true
			}
			body["thinking_config"] = tc
		}
	}
	if len(cfg.ServerTools) > 0 {
		tools := make([]map[string]any, 0, len(cfg.ServerTools))
		for _, s := range cfg.ServerTools {
			tools = append(tools, map[string]any{"type": s})
		}
		body["tools"] = tools
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

	// Content parts stay generic: a part flagged thought:true carries
	// reasoning (returned only when include_thoughts was requested) and is
	// captured verbatim as a thinking block; plain text parts are the answer.
	var out struct {
		Steps []struct {
			Type    string           `json:"type"`
			Content []map[string]any `json:"content"`
		} `json:"steps"`
		Usage struct {
			InputTokens       int `json:"input_tokens"`
			OutputTokens      int `json:"output_tokens"`
			TotalInputTokens  int `json:"total_input_tokens"`
			TotalOutputTokens int `json:"total_output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false, fmt.Errorf("gemini: decode response: %w", err)
	}

	// The answer is the concatenation of non-thought text blocks in
	// model_output steps; thought-flagged parts (any step) are the reasoning.
	var text string
	var thoughtBlocks []map[string]any
	var thoughts []string
	for _, st := range out.Steps {
		for _, c := range st.Content {
			if thought, _ := c["thought"].(bool); thought {
				if t, ok := c["text"].(string); ok && t != "" {
					thoughts = append(thoughts, t)
				}
				thoughtBlocks = append(thoughtBlocks, c)
				continue
			}
			if st.Type == "model_output" {
				if t, ok := c["text"].(string); ok && c["type"] == "text" {
					text += t
				}
			}
		}
	}
	thoughtText := strings.Join(thoughts, "\n\n")

	// Usage fields differ across interactions responses: prefer the explicit
	// input/output pair, falling back to the total_* pair.
	inTok := out.Usage.InputTokens
	if inTok == 0 {
		inTok = out.Usage.TotalInputTokens
	}
	outTok := out.Usage.OutputTokens
	if outTok == 0 {
		outTok = out.Usage.TotalOutputTokens
	}

	events := make(chan event, 2)
	go func() {
		defer close(events)
		if text != "" {
			events <- event{delta: text}
		}
		events <- event{usage: Usage{Input: inTok, Output: outTok}, thinking: thoughtText, thinkingBlocks: thoughtBlocks}
	}()
	return events, false, nil
}

// geminiPartTypes maps our media part classes onto the interactions API's
// typed part names (PDFs and other files are "document" parts).
var geminiPartTypes = map[string]string{
	"image": "image",
	"audio": "audio",
	"video": "video",
	"file":  "document",
}

// geminiInput renders the conversation for the interactions API. With no
// media it stays the historical single flattened string ("role: content"
// lines, system first). With any media part it returns an array of typed
// parts — text parts keep the role-prefixed rendering, media parts are
// uploaded to the Files API and referenced by URI.
func geminiInput(ctx context.Context, files *geminiFiles, msgs []Message) (any, error) {
	if !hasMedia(msgs) {
		return flattenText(msgs), nil
	}
	var parts []map[string]any
	appendTurnText := func(role, text string) {
		if text == "" {
			return
		}
		if role == "" {
			role = "user"
		}
		parts = append(parts, map[string]any{"type": "text", "text": fmt.Sprintf("%s: %s", role, text)})
	}
	for i, m := range msgs {
		role := m.Role
		if i == 0 && role == "system" {
			role = "system"
		}
		content := m.Content
		if m.Role == "tool" && m.ToolResult != nil {
			content = string(mustJSON(m.ToolResult))
		}
		appendTurnText(role, content)
		for _, p := range m.Parts {
			switch p.Type {
			case "text":
				if p.Text != "" {
					parts = append(parts, map[string]any{"type": "text", "text": p.Text})
				}
			case "image", "audio", "video", "file":
				mime := p.MIME
				if mime == "" {
					mime = "application/octet-stream"
				}
				uri, err := files.reference(ctx, p, mime)
				if err != nil {
					return nil, err
				}
				parts = append(parts, map[string]any{
					"type":      geminiPartTypes[p.Type],
					"uri":       uri,
					"mime_type": mime,
				})
			default:
				return nil, fmt.Errorf("gemini: unsupported part type %q", p.Type)
			}
		}
	}
	return parts, nil
}

// flattenText is the legacy no-media rendering: one "role: content" block per
// non-empty turn, system first.
func flattenText(msgs []Message) string {
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
