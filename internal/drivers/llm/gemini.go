package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"agentflow/internal/config"

	"google.golang.org/genai"
	"google.golang.org/genai/interactions/models/apierrors"
	"google.golang.org/genai/interactions/models/interactions"
	"google.golang.org/genai/interactions/models/operations"
	"google.golang.org/genai/interactions/retry"
)

// defaultGeminiBase is the endpoint prefix this provider defaults to, version
// segment included — the shape base_url has always carried here.
const defaultGeminiBase = "https://generativelanguage.googleapis.com/v1beta"

// defaultGeminiAPIVersion is the version segment of defaultGeminiBase: what the
// SDK is told to address when the configured base_url has no version segment of
// its own to lift out (see splitGeminiBase).
const defaultGeminiAPIVersion = "v1beta"

// geminiOpen implements the Gemini Interactions API
// (POST {base}/interactions), the request/response endpoint that supports
// server-side tools such as Google Search grounding via
// tools:[{"type":"google_search"}]. base_url should include the version prefix
// (e.g. https://generativelanguage.googleapis.com/v1beta).
//
// The wire is owned by the official SDK (google.golang.org/genai): URL
// composition, the API-key header, request encoding, the typed response decode
// and the API error types all come from it. This file keeps only the
// translation between the provider-neutral Message/Opts shapes and the SDK's
// request types, plus the terminal event mapping the rest of the runtime
// consumes.
//
// Three places where the SDK cannot carry what this provider's contract needs,
// and what is done about each:
//
//   - The thinking knob is generation_config.thinking_level. The SDK's
//     GenerationConfig models no thinking_budget/include_thoughts pair — it
//     spells "return thought summaries" as thinking_summaries — so the level
//     maps onto ThinkingLevel and a non-off level additionally asks for
//     summaries (see geminiThinkingLevel). Off has no exact expression: the
//     level ladder starts at "minimal".
//   - A thought-flagged text part and the non-total_ usage aliases are dropped
//     by the SDK's typed unions on decode (verified: Content is a union
//     discriminated on `type`, and Usage models only the total_* keys). Both are
//     load-bearing — the flag is how the reply's reasoning is separated from its
//     answer, and the token counts feed the per-user ledger and the agent budget
//     — so they are read back off the raw response bytes the SDK's own transport
//     delivered (see geminiRecorder).
//   - opts.Temperature: the interactions request models no temperature at all,
//     so a call that sets one is refused rather than sent without it.
//
// This API is non-streaming (no SSE), so the reply is emitted as a single text
// delta followed by usage. The conversation flattens into one "input" string
// (system context first) — unless any turn carries media parts, in which case
// "input" becomes an array of typed parts. Media never goes inline: every media
// part is uploaded to the Gemini Files API and referenced by URI. Files are
// auto-deleted by Google after 48 hours; a process-level cache dedupes uploads
// of the same content within that window. Client-side function tools are not
// mapped (the interactions tool schema differs); a call that supplies them is
// refused rather than silently sent without. cfg.ServerTools are sent as typed
// Tool entries.
func geminiOpen(ctx context.Context, client *http.Client, cfg config.Model, msgs []Message, opts Opts) (<-chan event, bool, error) {
	// Client-side function tools are not mapped for this provider: the
	// interactions tool schema differs from the OpenAI shape. Dropping the list
	// silently left a tool-using loop looking broken — the model simply never
	// calls anything, and nothing says why — so refuse instead and name the
	// path that does work.
	if len(opts.Tools) > 0 {
		return nil, false, fmt.Errorf("gemini: client-side function tools are not supported by the interactions provider (%d supplied); use server_tools, or a provider that maps them (anthropic, openai, openai-responses)", len(opts.Tools))
	}
	// opts.Temperature has no expression in the interactions request: neither
	// CreateModelInteraction nor its GenerationConfig models a temperature, and
	// the SDK marshals only the fields it declares, so there is nowhere to put
	// it. Refused rather than dropped for the same reason as the tools above —
	// a knob that silently does nothing reads as a working config.
	if opts.Temperature != nil {
		return nil, false, errors.New("gemini: the interactions request models no temperature; unset it, or use a provider that maps it (anthropic, openai, openai-responses)")
	}
	thinking, err := thinkingOf(cfg, opts)
	if err != nil {
		return nil, false, err
	}

	base := resolveBase(cfg, defaultGeminiBase)
	baseURL, apiVersion := splitGeminiBase(base)

	// The SDK's own client does the talking, but with the runtime's HTTP client
	// underneath: the Manager's per-request context still governs the timeout,
	// and any transport the runtime installed is still used. A clone of it
	// carries rec, which keeps the raw response bytes (see geminiRecorder) —
	// ClientConfig.HTTPClient is a concrete *http.Client, so the recorder has to
	// sit at the transport layer. The backend is pinned to the Gemini Developer
	// API so the decision never rides on the GOOGLE_GENAI_USE_VERTEXAI
	// environment variable — an API key is the whole credential here. An empty
	// api_key is the one case where the SDK would fall back to the environment's
	// key where the hand-rolled client sent no key header at all; anthropic.go
	// and openai.go defer to the same behaviour.
	if client == nil {
		client = &http.Client{}
	}
	rec := &geminiRecorder{inner: transportOf(client)}
	// A shallow copy of the client, not a mutation of it: the runtime's client
	// may be shared with other drivers, and only this call's requests should go
	// past the recorder.
	sdkClient := *client
	sdkClient.Transport = rec
	gc, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:     cfg.APIKey,
		Backend:    genai.BackendGeminiAPI,
		HTTPClient: &sdkClient,
		HTTPOptions: genai.HTTPOptions{
			BaseURL:    baseURL,
			APIVersion: apiVersion,
		},
	})
	if err != nil {
		return nil, false, fmt.Errorf("gemini: init client: %w", err)
	}

	input, err := geminiInput(ctx, newGeminiFiles(gc, cfg.APIKey, client), msgs)
	if err != nil {
		return nil, false, err
	}

	interaction := interactions.CreateModelInteraction{
		Model: interactions.Model(cfg.Model),
		Input: input,
	}
	if level, ok := geminiThinkingLevel[thinking]; ok {
		interaction.GenerationConfig = &interactions.GenerationConfig{ThinkingLevel: &level}
		if thinking != ThinkingOff {
			// Thought summaries are returned only when asked for; ask whenever
			// thinking is actually on. With off there is nothing to summarize,
			// so the summaries knob stays unset.
			summaries := interactions.ThinkingSummariesAuto
			interaction.GenerationConfig.ThinkingSummaries = &summaries
		}
	}
	if len(cfg.ServerTools) > 0 {
		interaction.Tools = geminiTools(cfg.ServerTools)
	}

	req := operations.CreateInteractionRequest{
		APIVersion: &apiVersion,
		Body:       operations.NewCreateInteractionRequestBody(interaction),
	}
	// The Manager's openWithRetry owns retry policy. Left at its default the SDK
	// would retry 429/5xx four times with backoff *and* retry connection errors,
	// compounding the two policies and turning an unreachable base_url into a
	// half-minute hang before the first failure is reported.
	resp, err := gc.Interactions.Create(ctx, req, operations.WithRetries(retry.Config{Strategy: "none"}))
	if err != nil {
		code := geminiStatus(err)
		if code == 0 {
			// A transport failure never reached the provider, so it is
			// retryable, and the SDK's error is the whole story.
			return nil, true, fmt.Errorf("%s -> %w", base+"/interactions", err)
		}
		retryable, serr := statusError(code, []byte(err.Error()))
		return nil, retryable, fmt.Errorf("%s -> %w", base+"/interactions", serr)
	}

	out, err := decodeGeminiReply(rec.lastBody())
	if err != nil {
		return nil, false, err
	}

	// The answer is the concatenation of non-thought text blocks in
	// model_output steps; thought-flagged parts and thought steps (any step) are
	// the reasoning, kept verbatim for replay.
	var text string
	var thoughtBlocks []map[string]any
	var thoughts []string
	for _, st := range out.Steps {
		stepType, _ := st["type"].(string)
		// A thought step is the API's own shape for reasoning — the SDK models it
		// as ThoughtStep, a summary of text/image parts — and is captured whole.
		if stepType == "thought" {
			if summary, ok := st["summary"].([]any); ok {
				for _, item := range summary {
					s, _ := item.(map[string]any)
					if t, _ := s["text"].(string); t != "" && s["type"] != "image" {
						thoughts = append(thoughts, t)
					}
				}
			}
			thoughtBlocks = append(thoughtBlocks, st)
			continue
		}
		content, _ := st["content"].([]any)
		for _, item := range content {
			c, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if thought, _ := c["thought"].(bool); thought {
				if t, ok := c["text"].(string); ok && t != "" {
					thoughts = append(thoughts, t)
				}
				thoughtBlocks = append(thoughtBlocks, c)
				continue
			}
			if stepType == "model_output" {
				if t, ok := c["text"].(string); ok && c["type"] == "text" {
					text += t
				}
			}
		}
	}
	thoughtText := strings.Join(thoughts, "\n\n")

	// Usage: the SDK's typed total_* fields are the shape the API's own schema
	// defines, so they win; the aliases its Usage type does not model
	// (input_tokens/output_tokens, and the legacy camelCase
	// cachedContentTokenCount) are then consulted off the raw response, so an
	// endpoint speaking the older shape still reaches the ledger rather than
	// reporting zero.
	var typed *interactions.Usage
	if resp.Interaction != nil {
		typed = resp.Interaction.Usage
	}
	inTok := intOrZero(typed.GetTotalInputTokens())
	if inTok == 0 {
		inTok = out.Usage.InputTokens
	}
	if inTok == 0 {
		inTok = out.Usage.TotalInputTokens
	}
	outTok := intOrZero(typed.GetTotalOutputTokens())
	if outTok == 0 {
		outTok = out.Usage.OutputTokens
	}
	if outTok == 0 {
		outTok = out.Usage.TotalOutputTokens
	}
	cached := intOrZero(typed.GetTotalCachedTokens())
	if cached == 0 {
		cached = out.Usage.CachedContentTokenCount
	}

	events := make(chan event, 2)
	go func() {
		defer close(events)
		if text != "" {
			events <- event{delta: text}
		}
		events <- event{usage: Usage{Input: inTok, Output: outTok, Cached: cached}, thinking: thoughtText, thinkingBlocks: thoughtBlocks}
	}()
	return events, false, nil
}

// splitGeminiBase splits the configured endpoint prefix into the SDK's two
// knobs, BaseURL and APIVersion: the SDK composes {BaseURL}/{APIVersion}
// /interactions, while this provider's base_url is documented as the full
// version-prefixed prefix that /interactions is appended to. So the last path
// segment is lifted out as the API version — for
// https://generativelanguage.googleapis.com/v1beta that is a real version, and
// for any other prefix with a path it still reproduces {base}/interactions
// exactly. A prefix with no path of its own (an httptest address, say) has no
// segment to lift, so the version this provider targets is used and the URL
// gains the /v1beta the SDK's endpoint template requires.
func splitGeminiBase(base string) (baseURL, apiVersion string) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return base, defaultGeminiAPIVersion
	}
	path := strings.Trim(u.Path, "/")
	if path == "" {
		return base, defaultGeminiAPIVersion
	}
	origin := u.Scheme + "://" + u.Host
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return origin + "/" + path[:i], path[i+1:]
	}
	return origin, path
}

// geminiTools maps cfg.ServerTools onto the SDK's Tool union. google_search has
// a real member; every other configured name rides through the union's
// UnknownRaw member as the bare {"type":<name>} entry the hand-rolled client
// sent, so a name the runtime invents still reaches the provider instead of
// being dropped.
func geminiTools(names []string) []interactions.Tool {
	tools := make([]interactions.Tool, 0, len(names))
	for _, s := range names {
		if s == "google_search" {
			tools = append(tools, interactions.NewTool(interactions.GoogleSearch{}))
			continue
		}
		tools = append(tools, interactions.NewToolUnknown(mustJSON(map[string]any{"type": s})))
	}
	return tools
}

// geminiStatus extracts the HTTP status from an SDK error. 0 means the error
// never carried one — a transport failure, or a response shape the SDK could not
// classify.
func geminiStatus(err error) int {
	var apiErr *apierrors.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	var clientErr *apierrors.CreateInteractionClientError
	if errors.As(err, &clientErr) && clientErr.HTTPMeta.Response != nil {
		return clientErr.HTTPMeta.Response.StatusCode
	}
	var serverErr *apierrors.CreateInteractionServerError
	if errors.As(err, &serverErr) && serverErr.HTTPMeta.Response != nil {
		return serverErr.HTTPMeta.Response.StatusCode
	}
	return 0
}

// transportOf is the round tripper the runtime's client would use, so the
// recorder can sit in front of it rather than replacing it (a nil Transport
// means http.DefaultTransport).
func transportOf(c *http.Client) http.RoundTripper {
	if c.Transport != nil {
		return c.Transport
	}
	return http.DefaultTransport
}

// geminiRecorder is the round tripper the SDK's client is handed, wrapped so the
// last response body stays readable after the SDK has consumed it. The SDK's
// typed unions drop a thought-flagged text part and the non-total_ usage aliases
// on decode (see geminiOpen), and neither survives being re-marshalled from the
// typed value, so the driver needs the bytes themselves. It is a pass-through in
// every other respect: the body handed back to the SDK is a fresh reader over
// the same bytes, and the SDK's own retries simply overwrite the snapshot.
type geminiRecorder struct {
	inner http.RoundTripper

	mu   sync.Mutex
	body []byte
}

func (r *geminiRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.inner.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	b, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	_ = resp.Body.Close()
	if rerr != nil {
		return nil, fmt.Errorf("gemini: read response: %w", rerr)
	}
	r.mu.Lock()
	r.body = b
	r.mu.Unlock()
	resp.Body = io.NopCloser(bytes.NewReader(b))
	return resp, nil
}

// lastBody returns the most recent response body the recorder saw.
func (r *geminiRecorder) lastBody() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body
}

// geminiRawResponse is the little of the interactions reply that is read off the
// raw bytes: the steps (thought flags included, which the SDK's Content union
// drops) and the usage aliases its Usage type does not model. Steps and their
// content stay generic maps so a thought block is replayed exactly as it
// arrived.
type geminiRawResponse struct {
	Steps []map[string]any `json:"steps"`
	Usage struct {
		InputTokens             int `json:"input_tokens"`
		OutputTokens            int `json:"output_tokens"`
		TotalInputTokens        int `json:"total_input_tokens"`
		TotalOutputTokens       int `json:"total_output_tokens"`
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
	} `json:"usage"`
}

func decodeGeminiReply(raw []byte) (geminiRawResponse, error) {
	var out geminiRawResponse
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("gemini: decode response: %w", err)
	}
	return out, nil
}

// intOrZero dereferences an optional token count (the SDK's usage fields are
// pointers so that "absent" and "zero" are distinguishable).
func intOrZero(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

// geminiInput renders the conversation for the interactions API. With no
// media it stays the historical single flattened string ("role: content"
// lines, system first). With any media part it returns the typed-parts array —
// text parts keep the role-prefixed rendering, media parts are uploaded to the
// Files API and referenced by URI.
func geminiInput(ctx context.Context, files *geminiFiles, msgs []Message) (*interactions.InteractionsInput, error) {
	if !hasMedia(msgs) {
		in := interactions.NewInteractionsInput(flattenText(msgs))
		return &in, nil
	}
	var parts []interactions.Content
	appendTurnText := func(role, text string) {
		if text == "" {
			return
		}
		if role == "" {
			role = "user"
		}
		parts = append(parts, interactions.NewContent(interactions.TextContent{Text: fmt.Sprintf("%s: %s", role, text)}))
	}
	for _, m := range msgs {
		role := m.Role
		content := m.Content
		if m.Role == "tool" && m.ToolResult != nil {
			content = string(mustJSON(m.ToolResult))
		}
		appendTurnText(role, content)
		for _, p := range m.Parts {
			switch p.Type {
			case "text":
				if p.Text != "" {
					parts = append(parts, interactions.NewContent(interactions.TextContent{Text: p.Text}))
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
				part, err := geminiMediaPart(p.Type, uri, mime)
				if err != nil {
					return nil, err
				}
				parts = append(parts, part)
			default:
				return nil, fmt.Errorf("gemini: unsupported part type %q", p.Type)
			}
		}
	}
	in := interactions.NewInteractionsInput(parts)
	return &in, nil
}

// geminiMediaPart builds the typed media content for an uploaded file. The part
// classes map onto the interactions API's content members one to one, except
// that PDFs and other files are "document" parts.
func geminiMediaPart(kind, uri, mime string) (interactions.Content, error) {
	switch kind {
	case "image":
		m := interactions.ImageContentMimeType(mime)
		return interactions.NewContent(interactions.ImageContent{URI: &uri, MimeType: &m}), nil
	case "audio":
		m := interactions.AudioContentMimeType(mime)
		return interactions.NewContent(interactions.AudioContent{URI: &uri, MimeType: &m}), nil
	case "video":
		m := interactions.VideoContentMimeType(mime)
		return interactions.NewContent(interactions.VideoContent{URI: &uri, MimeType: &m}), nil
	case "file":
		m := interactions.DocumentContentMimeType(mime)
		return interactions.NewContent(interactions.DocumentContent{URI: &uri, MimeType: &m}), nil
	}
	return interactions.Content{}, fmt.Errorf("gemini: unsupported part type %q", kind)
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
