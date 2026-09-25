package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"agentflow/internal/config"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/shared"
)

// OpenAI Chat Completions API, always called with stream=true.
// https://platform.openai.com/docs/api-reference/chat/create
//
// The wire is owned by the official SDK: request encoding, SSE framing and the
// chunk decoding all come from openai-go. This file keeps only the translation
// between the provider-neutral Message/Opts shapes and the SDK's params, plus
// the terminal event mapping the rest of the runtime consumes.
//
// base_url still includes /v1 (matching the OpenAI SDK convention), and the
// runtime still asks the SDK for chat/completions relative to it, so
// compatible servers (Ollama, vLLM, LiteLLM, OpenRouter) resolve correctly
// without a doubled /v1. option.WithBaseURL only appends a trailing slash to
// the configured path, and the SDK's own default (https://api.openai.com/v1/)
// has the same shape, so passing the base through unchanged preserves the
// contract: http://127.0.0.1:1234 -> /chat/completions,
// http://host/v1 -> /v1/chat/completions.
//
// Two departures from the previous hand-rolled client:
//
//   - The SDK owns retries, and its default is 2. The Manager already has its
//     own policy (openWithRetry), so the SDK's is disabled rather than
//     multiplied by it.
//   - Request shapes the SDK's typed unions do not model — the video_url and
//     file_url content parts, and bare {"type":<name>} server-tool entries —
//     are built through param.Override rather than by hand-writing the request.
//     That is the SDK's own raw-JSON escape hatch: MarshalUnion marshals the
//     override verbatim when no typed variant is set. It keeps the MiniMax/
//     Kimi/GLM conventions expressible through the SDK instead of dropping
//     them. See rawContentPart and openaiServerTool.
func openaiChatOpen(ctx context.Context, client *http.Client, cfg config.Model, msgs []Message, opts Opts) (<-chan event, bool, error) {
	base := resolveBase(cfg, "https://api.openai.com/v1")
	msgsOut, err := openaiMessages(msgs)
	if err != nil {
		return nil, false, err
	}
	thinking, err := thinkingOf(cfg, opts)
	if err != nil {
		return nil, false, err
	}

	params := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(cfg.Model),
		Messages: msgsOut,
		// The chunk that carries `usage` only arrives when it is asked for.
		StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: param.NewOpt(true)},
	}
	if len(opts.Tools) > 0 || len(cfg.ServerTools) > 0 {
		params.Tools = openaiChatTools(opts.Tools)
		// Provider-native server tools ride alongside the function tools as
		// bare entries; openaiServerTool explains how the SDK carries them.
		for _, s := range cfg.ServerTools {
			params.Tools = append(params.Tools, openaiServerTool(s))
		}
		if opts.ToolChoice != "" {
			params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{
				OfAuto: param.NewOpt(opts.ToolChoice),
			}
		}
	}
	if opts.Temperature != nil {
		params.Temperature = param.NewOpt(*opts.Temperature)
	}
	if mt := maxTokensOf(cfg, opts); mt > 0 {
		params.MaxTokens = param.NewOpt(int64(mt))
	}
	if thinking != "" {
		if effort, ok := openaiEffort[thinking]; ok {
			params.ReasoningEffort = shared.ReasoningEffort(effort)
		}
	}

	sdkOpts := append([]option.RequestOption{
		option.WithBaseURL(base),
		option.WithHTTPClient(client),
		// The Manager's openWithRetry owns retry policy. Leaving the SDK's
		// default (2) in place would compound the two.
		option.WithMaxRetries(0),
	}, openaiAuthOptions(cfg.APIKey)...)
	api := openai.NewClient(sdkOpts...)

	stream := api.Chat.Completions.NewStreaming(ctx, params)

	// The request is issued inside NewStreaming, but its failure only surfaces
	// on the first read. Peek that read here so a bad key, a 429 or a 5xx is
	// reported as an *establishment* failure, which openWithRetry classifies and
	// retries. Discovered after the channel is handed back it would instead be a
	// mid-stream error, and those are terminal by contract.
	if !stream.Next() {
		serr := stream.Err()
		if serr == nil {
			serr = errors.New("stream closed before any event")
		}
		_ = stream.Close()
		return nil, openaiRetryable(serr), fmt.Errorf("%s/chat/completions -> %w", base, serr)
	}
	first := stream.Current()

	events := make(chan event, 16)
	go func() {
		defer close(events)
		defer stream.Close()

		var usage Usage
		// Reasoning content (xAI/DeepSeek/OpenRouter-style) streams as
		// reasoning_content fragments alongside the answer; OpenRouter names
		// the same field "reasoning". The SDK has no typed field for either,
		// so it is read off each chunk's raw JSON. Reasoning is a bare string
		// on this shape — sending it back is an error on DeepSeek-style APIs —
		// so it is never serialized into a request. No blocks exist here.
		var reason strings.Builder
		// Tool-call accumulation: one entry per delta index. The first delta
		// for an index carries function.name; subsequent deltas carry
		// fragments of function.arguments that must be concatenated.
		type acc struct {
			id   string
			name string
			args strings.Builder
		}
		accByIndex := map[int64]*acc{}
		var order []int64

		handle := func(c openai.ChatCompletionChunk) bool {
			reason.WriteString(openaiReasoningDelta(c.RawJSON()))
			// JSON.Usage.Valid() is false for the `"usage": null` the provider
			// sends on every chunk but the last, which is exactly the previous
			// client's `ev.Usage != nil` test.
			if c.JSON.Usage.Valid() {
				usage.Input = int(c.Usage.PromptTokens)
				usage.Output = int(c.Usage.CompletionTokens)
				usage.Cached = int(c.Usage.PromptTokensDetails.CachedTokens)
				usage.Reasoning = int(c.Usage.CompletionTokensDetails.ReasoningTokens)
			}
			if len(c.Choices) == 0 {
				return true
			}
			d := c.Choices[0].Delta
			if d.Content != "" {
				select {
				case events <- event{delta: d.Content}:
				case <-ctx.Done():
					return false
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
			return true
		}

		if !handle(first) {
			return
		}
		for stream.Next() {
			if !handle(stream.Current()) {
				return
			}
		}
		if serr := stream.Err(); serr != nil {
			events <- event{err: fmt.Errorf("openai stream: %w", serr)}
			return
		}

		// Two events, tool calls first: StreamNext relies on this order to
		// report a tool-call turn's usage rather than its cost as zero.
		if len(order) > 0 {
			calls := make([]ToolCall, 0, len(order))
			for _, i := range order {
				a := accByIndex[i]
				calls = append(calls, ToolCall{ID: a.id, Name: a.name, Args: parseArgs(a.args.String())})
			}
			events <- event{toolCalls: calls, thinking: reason.String()}
		}
		events <- event{usage: usage, thinking: reason.String()}
	}()
	return events, false, nil
}

// openaiRetryable classifies an establishment failure. 429 and 5xx are worth
// another attempt; anything else (401, 400, a malformed base_url) is not.
//
// The SDK only attaches a status code to *openai.Error. Anything else is a
// transport failure or a response the SDK could not decode into its error
// shape (a proxy's non-JSON 5xx page, say); neither reached the provider's
// own error path, so both get the benefit of the doubt — the same call
// anthropicRetryable makes.
func openaiRetryable(err error) bool {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode >= 500
	}
	return true
}

// openaiReasoningDelta pulls the reasoning text out of one raw chunk.
//
// ChatCompletionChunkChoiceDelta has no typed field for it: reasoning_content
// is xAI/DeepSeek's extension and `reasoning` is OpenRouter's alias for the
// same content, both outside OpenAI's schema. The chunk's own JSON is the only
// place they survive, so it is read from there. The two are concatenated in
// stream order, matching the previous client, which wrote both when a provider
// sent both.
func openaiReasoningDelta(raw string) string {
	var ev struct {
		Choices []struct {
			Delta struct {
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(raw), &ev); err != nil || len(ev.Choices) == 0 {
		return ""
	}
	return ev.Choices[0].Delta.ReasoningContent + ev.Choices[0].Delta.Reasoning
}

// openaiChatTools converts the provider-agnostic ToolDef list to the Chat
// Completions request shape. The JSON Schema a loop supplies is passed through
// verbatim as shared.FunctionParameters, so a schema the SDK has never heard
// of survives intact.
func openaiChatTools(tools []ToolDef) []openai.ChatCompletionToolParam {
	out := make([]openai.ChatCompletionToolParam, 0, len(tools))
	for _, t := range tools {
		out = append(out, openai.ChatCompletionToolParam{
			Function: shared.FunctionDefinitionParam{
				Name:        t.Name,
				Description: param.NewOpt(t.Description),
				Parameters:  shared.FunctionParameters(t.Parameters),
			},
		})
	}
	return out
}

// openaiAuthOptions returns the credential options for an OpenAI-shaped client
// (chat completions, responses, embeddings).
//
// openai.NewClient unconditionally prepends DefaultClientOptions(), which reads
// OPENAI_API_KEY, OPENAI_ORG_ID and OPENAI_PROJECT_ID out of the process
// environment; the options a caller passes are applied *after* those. An
// explicit base_url and api_key therefore always win — but an empty api_key
// would silently inherit the environment's key, and a model entry with no
// api_key pointing at a third-party base_url would then send the process's real
// OpenAI key to that host. The hand-rolled client sent no Authorization header
// at all in that case, so the inherited credentials are cleared rather than
// kept. The org and project ids identify the account too, and go the same way.
func openaiAuthOptions(apiKey string) []option.RequestOption {
	if apiKey != "" {
		return []option.RequestOption{option.WithAPIKey(apiKey)}
	}
	return []option.RequestOption{
		option.WithHeaderDel("authorization"),
		option.WithHeaderDel("OpenAI-Organization"),
		option.WithHeaderDel("OpenAI-Project"),
	}
}

// openaiMessages reshapes the provider-neutral Message list into OpenAI's
// native tool-turn representation: an assistant turn with tool_calls becomes
// {role:"assistant", content, tool_calls:[...]}; a "tool" role turn becomes
// {role:"tool", tool_call_id, content:json(tool_result)}. Plain turns pass
// through unchanged. Multimodal turns become content arrays of text /
// image_url / input_audio parts (vision + audio models).
//
// The system message rides inline as a system turn (the Chat Completions API
// has no top-level instructions field). A role the schema does not model is
// refused rather than guessed at.
func openaiMessages(msgs []Message) ([]openai.ChatCompletionMessageParamUnion, error) {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(msgs))
	for _, m := range msgs {
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			tcs := make([]openai.ChatCompletionMessageToolCallParam, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				tcs = append(tcs, openai.ChatCompletionMessageToolCallParam{
					ID: tc.ID,
					Function: openai.ChatCompletionMessageToolCallFunctionParam{
						Name:      tc.Name,
						Arguments: string(mustJSON(tc.Args)),
					},
				})
			}
			out = append(out, openai.ChatCompletionMessageParamUnion{
				OfAssistant: &openai.ChatCompletionAssistantMessageParam{
					Content: openai.ChatCompletionAssistantMessageParamContentUnion{
						OfString: param.NewOpt(m.Content),
					},
					ToolCalls: tcs,
				},
			})
		case m.Role == "tool":
			content := m.Content
			if m.ToolResult != nil {
				content = string(mustJSON(m.ToolResult))
			}
			out = append(out, openai.ChatCompletionMessageParamUnion{
				OfTool: &openai.ChatCompletionToolMessageParam{
					ToolCallID: m.ToolCallID,
					Content: openai.ChatCompletionToolMessageParamContentUnion{
						OfString: param.NewOpt(content),
					},
				},
			})
		default:
			turn, err := openaiTurn(m)
			if err != nil {
				return nil, err
			}
			out = append(out, turn)
		}
	}
	return out, nil
}

// openaiTurn builds one non-tool turn. The role decides which content union
// applies, because the SDK types them separately: only a user turn may carry
// media parts, while system/developer/assistant content is text-only in the
// Chat Completions schema.
func openaiTurn(m Message) (openai.ChatCompletionMessageParamUnion, error) {
	if len(m.Parts) == 0 {
		return openaiTextTurn(m.Role, m.Content)
	}
	if m.Role == "user" || m.Role == "" {
		parts, err := openaiContentParts(m)
		if err != nil {
			return openai.ChatCompletionMessageParamUnion{}, err
		}
		return openai.ChatCompletionMessageParamUnion{
			OfUser: &openai.ChatCompletionUserMessageParam{
				Content: openai.ChatCompletionUserMessageParamContentUnion{OfArrayOfContentParts: parts},
			},
		}, nil
	}
	texts, err := openaiTextParts(m)
	if err != nil {
		return openai.ChatCompletionMessageParamUnion{}, err
	}
	switch m.Role {
	case "system":
		return openai.ChatCompletionMessageParamUnion{
			OfSystem: &openai.ChatCompletionSystemMessageParam{Content: openai.ChatCompletionSystemMessageParamContentUnion{OfArrayOfContentParts: texts}},
		}, nil
	case "developer":
		return openai.ChatCompletionMessageParamUnion{
			OfDeveloper: &openai.ChatCompletionDeveloperMessageParam{Content: openai.ChatCompletionDeveloperMessageParamContentUnion{OfArrayOfContentParts: texts}},
		}, nil
	case "assistant":
		parts := make([]openai.ChatCompletionAssistantMessageParamContentArrayOfContentPartUnion, 0, len(texts))
		for i := range texts {
			parts = append(parts, openai.ChatCompletionAssistantMessageParamContentArrayOfContentPartUnion{OfText: &texts[i]})
		}
		return openai.ChatCompletionMessageParamUnion{
			OfAssistant: &openai.ChatCompletionAssistantMessageParam{Content: openai.ChatCompletionAssistantMessageParamContentUnion{OfArrayOfContentParts: parts}},
		}, nil
	}
	return openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("openai (chat completions) does not support role %q", m.Role)
}

// openaiTextTurn builds a plain text turn for one role.
func openaiTextTurn(role, content string) (openai.ChatCompletionMessageParamUnion, error) {
	switch role {
	case "system":
		return openai.ChatCompletionMessageParamUnion{OfSystem: &openai.ChatCompletionSystemMessageParam{Content: openai.ChatCompletionSystemMessageParamContentUnion{OfString: param.NewOpt(content)}}}, nil
	case "developer":
		return openai.ChatCompletionMessageParamUnion{OfDeveloper: &openai.ChatCompletionDeveloperMessageParam{Content: openai.ChatCompletionDeveloperMessageParamContentUnion{OfString: param.NewOpt(content)}}}, nil
	case "assistant":
		return openai.ChatCompletionMessageParamUnion{OfAssistant: &openai.ChatCompletionAssistantMessageParam{Content: openai.ChatCompletionAssistantMessageParamContentUnion{OfString: param.NewOpt(content)}}}, nil
	case "user", "":
		return openai.ChatCompletionMessageParamUnion{OfUser: &openai.ChatCompletionUserMessageParam{Content: openai.ChatCompletionUserMessageParamContentUnion{OfString: param.NewOpt(content)}}}, nil
	}
	return openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("openai (chat completions) does not support role %q", role)
}

// openaiContentParts builds the Chat Completions content array for one
// multimodal *user* turn. Images take image_url (data: URI or remote URL);
// audio takes input_audio (base64, wav/mp3); video takes video_url (URL,
// base64, or a provider file ref like mm_file:// / ms:// — the MiniMax/Kimi/GLM
// convention); files take file_url (URL source only — GLM). Unsupported sources
// error honestly.
//
// video_url and file_url have no typed variant in the SDK's content-part union,
// so they go through param.Override — the SDK's sanctioned raw-JSON path, which
// MarshalUnion marshals verbatim whenever no typed variant is set (see the
// param package's MarshalUnion). That is what keeps the multimodal conventions
// of OpenAI-compatible providers expressible without hand-rolling the request.
func openaiContentParts(m Message) ([]openai.ChatCompletionContentPartUnionParam, error) {
	var out []openai.ChatCompletionContentPartUnionParam
	if m.Content != "" {
		out = append(out, openai.ChatCompletionContentPartUnionParam{
			OfText: &openai.ChatCompletionContentPartTextParam{Text: m.Content},
		})
	}
	for _, p := range m.Parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				out = append(out, openai.ChatCompletionContentPartUnionParam{
					OfText: &openai.ChatCompletionContentPartTextParam{Text: p.Text},
				})
			}
		case "image":
			url := p.URL
			if url == "" {
				if p.Data == "" {
					return nil, fmt.Errorf("openai: image part has neither url nor data")
				}
				url = dataURI(p)
			}
			out = append(out, openai.ChatCompletionContentPartUnionParam{
				OfImageURL: &openai.ChatCompletionContentPartImageParam{
					ImageURL: openai.ChatCompletionContentPartImageImageURLParam{URL: url},
				},
			})
		case "audio":
			if p.Data == "" {
				return nil, fmt.Errorf("openai: audio part requires inline base64 data")
			}
			format := audioFormat(p.MIME)
			if format == "" {
				return nil, fmt.Errorf("openai: input_audio supports wav and mp3 only (got %q)", p.MIME)
			}
			out = append(out, openai.ChatCompletionContentPartUnionParam{
				OfInputAudio: &openai.ChatCompletionContentPartInputAudioParam{
					InputAudio: openai.ChatCompletionContentPartInputAudioInputAudioParam{Data: p.Data, Format: format},
				},
			})
		case "video":
			// De-facto standard across MiniMax / Kimi / GLM (and now OpenAI
			// itself): video_url with a remote URL, base64, or a provider
			// file reference (mm_file://, ms://). Passed through verbatim.
			url := p.URL
			if url == "" {
				if p.Data == "" {
					return nil, fmt.Errorf("openai: video part has neither url nor data")
				}
				url = dataURI(p)
			}
			out = append(out, rawContentPart(map[string]any{
				"type": "video_url", "video_url": map[string]any{"url": url},
			}))
		case "file":
			// GLM supports file_url (pdf/txt/word/xlsx) with a URL source.
			// Base64 is not accepted by the reference implementations, so
			// require a URL.
			if p.URL == "" {
				return nil, fmt.Errorf("openai: file part requires a url source (glm file_url; base64 not supported)")
			}
			out = append(out, rawContentPart(map[string]any{
				"type": "file_url", "file_url": map[string]any{"url": p.URL},
			}))
		default:
			return nil, fmt.Errorf("openai (chat completions) does not support %s parts; use the responses provider or pre-process the file", p.Type)
		}
	}
	if len(out) == 0 {
		out = append(out, openai.ChatCompletionContentPartUnionParam{
			OfText: &openai.ChatCompletionContentPartTextParam{Text: ""},
		})
	}
	return out, nil
}

// openaiTextParts flattens a multimodal turn to the text-only content parts
// that the system/developer/assistant content unions accept. A media part on
// one of those roles has no expression in the Chat Completions schema, so it
// is refused by name rather than silently dropped.
func openaiTextParts(m Message) ([]openai.ChatCompletionContentPartTextParam, error) {
	var out []openai.ChatCompletionContentPartTextParam
	if m.Content != "" {
		out = append(out, openai.ChatCompletionContentPartTextParam{Text: m.Content})
	}
	for _, p := range m.Parts {
		if p.Type != "text" {
			return nil, fmt.Errorf("openai (chat completions): the %s role accepts text content only; move the %s part to a user turn", m.Role, p.Type)
		}
		if p.Text != "" {
			out = append(out, openai.ChatCompletionContentPartTextParam{Text: p.Text})
		}
	}
	if len(out) == 0 {
		out = append(out, openai.ChatCompletionContentPartTextParam{Text: ""})
	}
	return out, nil
}

// rawContentPart builds a content part the SDK's typed union does not model,
// through param.Override — the SDK's documented escape hatch for raw JSON.
// ChatCompletionContentPartUnionParam marshals exactly OfText, OfImageURL,
// OfInputAudio and OfFile, so the de-facto video_url convention (and GLM's
// file_url) have no typed variant; with no variant set, the union's
// MarshalJSON falls through to the override and marshals it verbatim.
func rawContentPart(v map[string]any) openai.ChatCompletionContentPartUnionParam {
	return param.Override[openai.ChatCompletionContentPartUnionParam](v)
}

// openaiServerTool builds one provider-native server-tool entry. Most server
// tools (web_search, x_search, ...) take a bare {"type":<name>} with no
// function payload — the provider supplies and executes the tool — which the
// SDK's tool struct cannot represent (its Type is pinned to "function"), so it
// goes through param.Override. An unknown name falls back to a function-shaped
// stub so a typo does not silently produce a tool-less request.
func openaiServerTool(name string) openai.ChatCompletionToolParam {
	switch name {
	case "web_search", "x_search", "web_search_preview", "code_interpreter", "file_search", "image_generation", "mcp":
		return param.Override[openai.ChatCompletionToolParam](map[string]any{"type": name})
	default:
		return openai.ChatCompletionToolParam{
			Function: shared.FunctionDefinitionParam{Name: name},
		}
	}
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
