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

// geminiOpen implements the Gemini generateContent API
// (POST {base}/models/{model}:generateContent), the classic request/response
// endpoint that supports Google Search grounding via tools:[{"google_search":{}}].
// base_url should include the version prefix (e.g.
// https://generativelanguage.googleapis.com/v1beta); the runtime appends
// /models/{model}:generateContent. The API key goes in the x-goog-api-key
// header.
//
// This API is non-streaming (no SSE), so the reply is emitted as a single text
// delta followed by usage. Conversation turns flatten into contents[].parts;
// the optional system message becomes system_instruction. Client-side function
// tools are not mapped (the functionDeclarations schema differs); only
// cfg.ServerTools are sent, as native grounding-tool objects — e.g.
// server_tools:[google_search] -> tools:[{"google_search":{}}]. A server tool
// whose name carries no special object shape is sent as {"<name>":{}}.
func geminiOpen(ctx context.Context, client *http.Client, cfg config.Model, msgs []Message, opts Opts) (<-chan event, bool, error) {
	base := resolveBase(cfg, "https://generativelanguage.googleapis.com/v1beta")
	url := base + "/models/" + cfg.Model + ":generateContent"

	system, contents := geminiContents(msgs)
	body := map[string]any{"contents": contents}
	if system != "" {
		body["system_instruction"] = map[string]any{"parts": []map[string]any{{"text": system}}}
	}
	if len(cfg.ServerTools) > 0 {
		tools := make([]map[string]any, 0, len(cfg.ServerTools))
		for _, s := range cfg.ServerTools {
			tools = append(tools, map[string]any{s: map[string]any{}})
		}
		body["tools"] = tools
	}
	if opts.Temperature != nil {
		body["generationConfig"] = map[string]any{"temperature": *opts.Temperature}
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

	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			GroundingMetadata *struct {
				GroundingChunks []struct {
					Web *struct {
						URI   string `json:"uri"`
						Title string `json:"title"`
					} `json:"web"`
				} `json:"groundingChunks"`
			} `json:"groundingMetadata"`
		} `json:"candidates"`
		UsageMetadata *struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false, fmt.Errorf("gemini: decode response: %w", err)
	}

	var text string
	var sources []string
	if len(out.Candidates) > 0 {
		c := out.Candidates[0]
		for _, p := range c.Content.Parts {
			text += p.Text
		}
		if c.GroundingMetadata != nil {
			for _, ch := range c.GroundingMetadata.GroundingChunks {
				if ch.Web != nil && ch.Web.URI != "" {
					sources = append(sources, ch.Web.URI)
				}
			}
		}
	}
	// Append grounding sources so the caller sees citations alongside the answer.
	if len(sources) > 0 {
		text += "\n\nSources:\n"
		for _, s := range sources {
			text += "- " + s + "\n"
		}
		text = strings.TrimRight(text, "\n")
	}

	usage := Usage{}
	if out.UsageMetadata != nil {
		usage.Input = out.UsageMetadata.PromptTokenCount
		usage.Output = out.UsageMetadata.CandidatesTokenCount
	}

	events := make(chan event, 2)
	go func() {
		defer close(events)
		if text != "" {
			events <- event{delta: text}
		}
		events <- event{usage: usage}
	}()
	return events, false, nil
}

// geminiContents flattens the conversation into generateContent's contents
// array. The leading system message is returned separately (it maps to
// system_instruction); subsequent turns become {role, parts:[{text}]}, with
// assistant->model and tool results inlined as text. Roles must alternate
// user/model; consecutive same-role turns are merged.
func geminiContents(msgs []Message) (system string, contents []map[string]any) {
	if len(msgs) > 0 && msgs[0].Role == "system" {
		system = msgs[0].Content
		msgs = msgs[1:]
	}
	for _, m := range msgs {
		content := m.Content
		if m.Role == "tool" && m.ToolResult != nil {
			content = string(mustJSON(m.ToolResult))
		}
		if content == "" {
			continue
		}
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		// Merge into the previous part if the role repeats (generateContent
		// requires strict user/model alternation).
		if n := len(contents); n > 0 && contents[n-1]["role"] == role {
			parts := contents[n-1]["parts"].([]map[string]any)
			contents[n-1]["parts"] = append(parts, map[string]any{"text": content})
			continue
		}
		contents = append(contents, map[string]any{
			"role":  role,
			"parts": []map[string]any{{"text": content}},
		})
	}
	return system, contents
}
