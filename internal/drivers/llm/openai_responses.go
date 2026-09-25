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
	"github.com/openai/openai-go/responses"
	"github.com/openai/openai-go/shared"
)

// OpenAI Responses API, always called with stream=true.
// https://platform.openai.com/docs/api-reference/responses
//
// The Responses API is OpenAI's newer endpoint that replaces Chat Completions
// for structured output, tool use, and reasoning models. It takes an "input"
// array of items (messages, function calls, function outputs) and returns a
// different event stream shape.
//
// The wire is owned by the official SDK: request encoding, SSE framing and the
// event decoding come from openai-go. This file keeps only the translation
// between the provider-neutral Message/Opts shapes and the SDK's params, plus
// the terminal event mapping the rest of the runtime consumes.
//
// base_url includes /v1, and the runtime asks the SDK for responses relative
// to it — the same contract as before (see openaiChatOpen for why passing the
// configured base straight to option.WithBaseURL preserves it).
//
// Two departures from the previous hand-rolled client:
//
//   - The SDK owns retries, and its default is 2. The Manager already has its
//     own policy (openWithRetry), so the SDK's is disabled.
//   - The input_video and input_audio content parts have no variant in the
//     SDK's writable input-content union (input_text / input_image /
//     input_file), so they are built through param.Override — its raw-JSON
//     escape hatch. Both extensions survive the conversion; see
//     rawResponsesPart.
func openaiResponsesOpen(ctx context.Context, client *http.Client, cfg config.Model, msgs []Message, opts Opts) (<-chan event, bool, error) {
	base := resolveBase(cfg, "https://api.openai.com/v1")

	// The Responses API takes an "input" array of items. System messages map to
	// a top-level "instructions" field; assistant tool_calls become function_call
	// items and "tool" turns become function_call_output items.
	system, items, err := responsesItems(msgs)
	if err != nil {
		return nil, false, err
	}
	thinking, err := thinkingOf(cfg, opts)
	if err != nil {
		return nil, false, err
	}

	params := responses.ResponseNewParams{
		Model: shared.ResponsesModel(cfg.Model),
		Input: responses.ResponseNewParamsInputUnion{OfInputItemList: items},
	}
	if system != "" {
		params.Instructions = param.NewOpt(system)
	}
	if opts.Temperature != nil {
		params.Temperature = param.NewOpt(*opts.Temperature)
	}
	if mt := maxTokensOf(cfg, opts); mt > 0 {
		params.MaxOutputTokens = param.NewOpt(int64(mt))
	}
	if thinking != "" {
		if effort, ok := openaiEffort[thinking]; ok {
			params.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffort(effort)}
		}
	}
	if len(opts.Tools) > 0 || len(cfg.ServerTools) > 0 {
		tools := make([]responses.ToolUnionParam, 0, len(opts.Tools)+len(cfg.ServerTools))
		// Responses uses a flat function shape (no "function" wrapper).
		for _, t := range opts.Tools {
			tools = append(tools, responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{
				Name:        t.Name,
				Description: param.NewOpt(t.Description),
				Parameters:  t.Parameters,
			}})
		}
		for _, s := range cfg.ServerTools {
			tools = append(tools, responsesServerTool(s))
		}
		params.Tools = tools
		if opts.ToolChoice != "" {
			params.ToolChoice = responses.ResponseNewParamsToolChoiceUnion{
				OfToolChoiceMode: param.NewOpt(responses.ToolChoiceOptions(opts.ToolChoice)),
			}
		}
	}

	sdkOpts := []option.RequestOption{
		option.WithBaseURL(base),
		option.WithHTTPClient(client),
		// The Manager's openWithRetry owns retry policy. Leaving the SDK's
		// default (2) in place would compound the two.
		option.WithMaxRetries(0),
	}
	sdkOpts = append(sdkOpts, openaiAuthOptions(cfg.APIKey)...)
	api := openai.NewClient(sdkOpts...)

	stream := api.Responses.NewStreaming(ctx, params)

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
		return nil, openaiRetryable(serr), fmt.Errorf("%s/responses -> %w", base, serr)
	}
	first := stream.Current()

	events := make(chan event, 16)
	go func() {
		defer close(events)
		defer stream.Close()

		var usage Usage
		// Reasoning items (type:"reasoning") arrive whole on
		// response.output_item.done and are captured verbatim for reply
		// transparency; reasoning summary deltas stream the same text and
		// serve as the fallback for proxies that skip the item events.
		var reasoningItems []map[string]any
		var itemTexts []string
		var summary strings.Builder
		// Function-call accumulation keyed by output_index: arguments arrive as
		// fragments to concatenate; id/name come from the first delta that has
		// them (or from response.output_item.done).
		type acc struct {
			id   string
			name string
			args strings.Builder
		}
		accByIndex := map[int64]*acc{}
		var order []int64
		getAcc := func(i int64) *acc {
			a, ok := accByIndex[i]
			if !ok {
				a = &acc{}
				accByIndex[i] = a
				order = append(order, i)
			}
			return a
		}

		handle := func(ev responses.ResponseStreamEventUnion) bool {
			switch ev.Type {
			case "response.output_text.delta":
				if d := ev.Delta.OfString; d != "" {
					select {
					case events <- event{delta: d}:
					case <-ctx.Done():
						return false
					}
				}
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				summary.WriteString(ev.Delta.OfString)
			case "response.output_item.added":
				if ev.Item.Type == "function_call" {
					a := getAcc(ev.OutputIndex)
					a.id = ev.Item.CallID
					a.name = ev.Item.Name
				}
			case "response.function_call_arguments.delta":
				a := getAcc(ev.OutputIndex)
				// The SDK's event union models item_id and output_index only on
				// this event — call_id and name arrive on
				// response.output_item.added/.done. Some compatible proxies put
				// them on the delta instead, so the raw event JSON is read as a
				// fallback, which is the tolerance the hand-rolled reader had.
				if a.id == "" || a.name == "" {
					if id, name := responsesDeltaIdent(ev.RawJSON()); id != "" || name != "" {
						if a.id == "" {
							a.id = id
						}
						if a.name == "" {
							a.name = name
						}
					}
				}
				a.args.WriteString(ev.Delta.OfString)
			case "response.output_item.done":
				switch ev.Item.Type {
				case "function_call":
					a := getAcc(ev.OutputIndex)
					a.id = ev.Item.CallID
					a.name = ev.Item.Name
					// done carries the full arguments string; if no deltas were
					// seen (some proxies skip them), use it directly.
					if a.args.Len() == 0 {
						a.args.WriteString(ev.Item.Arguments)
					}
				case "reasoning":
					// Verbatim capture: the item round-trips as a thinking
					// block (replay needs the provider's own shape). The typed
					// Summary field covers the documented shape; the raw map
					// also carries any section the SDK does not model.
					if raw := ev.Item.RawJSON(); raw != "" {
						var item map[string]any
						if err := json.Unmarshal([]byte(raw), &item); err == nil {
							reasoningItems = append(reasoningItems, item)
							itemTexts = append(itemTexts, reasoningItemText(item)...)
						}
					}
				}
			case "response.completed":
				applyResponsesUsage(&usage, ev)
			case "error":
				events <- event{err: fmt.Errorf("openai-responses stream error: %s", ev.RawJSON())}
				return false
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
			events <- event{err: fmt.Errorf("openai-responses stream: %w", serr)}
			return
		}

		// Prefer the text assembled from whole reasoning items (the
		// provider's own grouping); fall back to the delta accumulation.
		thinking := strings.Join(itemTexts, "\n\n")
		if thinking == "" {
			thinking = summary.String()
		}

		// Two events, tool calls first: StreamNext relies on this order to
		// report a tool-call turn's usage rather than its cost as zero.
		if len(order) > 0 {
			calls := make([]ToolCall, 0, len(order))
			for _, i := range order {
				a := accByIndex[i]
				calls = append(calls, ToolCall{ID: a.id, Name: a.name, Args: parseArgs(a.args.String())})
			}
			events <- event{toolCalls: calls, thinking: thinking, thinkingBlocks: reasoningItems}
		}
		events <- event{usage: usage, thinking: thinking, thinkingBlocks: reasoningItems}
	}()
	return events, false, nil
}

// applyResponsesUsage copies token counts off a response.completed event. The
// nested response.usage is canonical; some compatible proxies nest the same
// object at the top level of the event instead, so that is read as a fallback.
// The event's own JSON is the only place the top-level copy survives — the
// SDK's event union models the nested one only.
func applyResponsesUsage(usage *Usage, ev responses.ResponseStreamEventUnion) {
	set := func(in, out, cached, reasoning int64) {
		if in > 0 {
			usage.Input = int(in)
		}
		if out > 0 {
			usage.Output = int(out)
		}
		if cached > 0 {
			usage.Cached = int(cached)
		}
		if reasoning > 0 {
			usage.Reasoning = int(reasoning)
		}
	}
	u := ev.Response.Usage
	set(u.InputTokens, u.OutputTokens, u.InputTokensDetails.CachedTokens, u.OutputTokensDetails.ReasoningTokens)

	if raw := ev.RawJSON(); raw != "" {
		var ev struct {
			Usage struct {
				InputTokens        int64 `json:"input_tokens"`
				OutputTokens       int64 `json:"output_tokens"`
				InputTokensDetails struct {
					CachedTokens int64 `json:"cached_tokens"`
				} `json:"input_tokens_details"`
				OutputTokensDetails struct {
					ReasoningTokens int64 `json:"reasoning_tokens"`
				} `json:"output_tokens_details"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(raw), &ev); err == nil {
			set(ev.Usage.InputTokens, ev.Usage.OutputTokens, ev.Usage.InputTokensDetails.CachedTokens, ev.Usage.OutputTokensDetails.ReasoningTokens)
		}
	}
}

// responsesDeltaIdent pulls call_id and name off a raw
// response.function_call_arguments.delta event. OpenAI does not send them
// there — the SDK's union has no field for them on that event — but
// OpenAI-compatible proxies do, so they are read from the event's own JSON.
func responsesDeltaIdent(raw string) (callID, name string) {
	var ev struct {
		CallID string `json:"call_id"`
		Name   string `json:"name"`
	}
	if raw == "" || json.Unmarshal([]byte(raw), &ev) != nil {
		return "", ""
	}
	return ev.CallID, ev.Name
}

// reasoningItemText joins the text sections of one raw reasoning item. The
// provider's own grouping is preferred over the delta accumulation; `content`
// is read alongside the documented `summary` because some compatible proxies
// send the text there.
func reasoningItemText(item map[string]any) []string {
	var parts []string
	for _, sec := range []string{"summary", "content"} {
		if arr, ok := item[sec].([]any); ok {
			for _, p := range arr {
				if pm, ok := p.(map[string]any); ok {
					if t, ok := pm["text"].(string); ok && t != "" {
						parts = append(parts, t)
					}
				}
			}
		}
	}
	return parts
}

// responsesServerTool maps a configured server_tools name onto the SDK's typed
// tool union.
//
// The union is typed, with no generic {"type":<name>} escape hatch, so a name
// the SDK has no variant for cannot be sent at all. Two consequences worth
// naming: OpenAI's Responses web-search tool is spelled `web_search_preview`
// in the SDK (a config naming `web_search` therefore goes on the wire as
// web_search_preview), and x_search — an xAI extension — has no variant. An
// unmappable name is refused with a message naming what is supported, the way
// gemini.go refuses client-side tools, rather than silently sending a request
// without the tool the caller asked for.
// responsesServerTool builds one provider-native server-tool entry. The
// runtime's contract is that `server_tools` carries provider-specific names and
// only the native entry is forwarded — the provider supplies and executes the
// tool — so the name goes out as a bare {"type":<name>}, built through
// param.Override.
//
// The typed union is deliberately not used here. It models only the handful of
// tools OpenAI documents, which would refuse an xAI name like x_search; and it
// spells web search "web_search_preview", which would silently rewrite a
// configured web_search. Forwarding the configured name verbatim is what this
// provider has always done and what the config reference promises.
func responsesServerTool(name string) responses.ToolUnionParam {
	return param.Override[responses.ToolUnionParam](map[string]any{"type": name})
}

// responsesItems converts the provider-neutral Message list to the Responses
// API "input" shape: the leading system message becomes "instructions"
// (returned as the first result); an assistant turn with tool_calls becomes
// function_call items; a "tool" turn becomes a function_call_output item.
// Plain turns become message items; multimodal turns become message items
// whose content is an array of input_text / input_image / input_file parts.
func responsesItems(msgs []Message) (system string, items []responses.ResponseInputItemUnionParam, err error) {
	if len(msgs) > 0 && msgs[0].Role == "system" {
		system = msgs[0].Content
		msgs = msgs[1:]
	}
	items = []responses.ResponseInputItemUnionParam{}
	for _, m := range msgs {
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			if m.Content != "" {
				items = append(items, responsesText("assistant", m.Content))
			}
			for _, tc := range m.ToolCalls {
				items = append(items, responses.ResponseInputItemUnionParam{
					OfFunctionCall: &responses.ResponseFunctionToolCallParam{
						CallID:    tc.ID,
						Name:      tc.Name,
						Arguments: string(mustJSON(tc.Args)),
					},
				})
			}
		case m.Role == "tool":
			output := m.Content
			if m.ToolResult != nil {
				output = string(mustJSON(m.ToolResult))
			}
			items = append(items, responses.ResponseInputItemUnionParam{
				OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
					CallID: m.ToolCallID,
					Output: output,
				},
			})
		default:
			if len(m.Parts) > 0 {
				content, cerr := responsesContentParts(m)
				if cerr != nil {
					return system, nil, cerr
				}
				items = append(items, responses.ResponseInputItemUnionParam{
					OfMessage: &responses.EasyInputMessageParam{
						Role: responses.EasyInputMessageRole(m.Role),
						Content: responses.EasyInputMessageContentUnionParam{
							OfInputItemContentList: content,
						},
					},
				})
			} else {
				items = append(items, responsesText(m.Role, m.Content))
			}
		}
	}
	return system, items, nil
}

// responsesText builds one plain-text message item.
func responsesText(role, content string) responses.ResponseInputItemUnionParam {
	return responses.ResponseInputItemUnionParam{
		OfMessage: &responses.EasyInputMessageParam{
			Role:    responses.EasyInputMessageRole(role),
			Content: responses.EasyInputMessageContentUnionParam{OfString: param.NewOpt(content)},
		},
	}
}

// rawResponsesPart builds an input-content part the SDK's typed union does not
// model, through param.Override — its documented escape hatch for raw JSON.
// ResponseInputContentUnionParam marshals only input_text, input_image and
// input_file, so input_video (MiniMax mm_file://, Kimi ms://) and input_audio
// have no typed variant; with none set, the union's MarshalJSON falls through
// to the override and marshals it verbatim.
func rawResponsesPart(v map[string]any) responses.ResponseInputContentUnionParam {
	return param.Override[responses.ResponseInputContentUnionParam](v)
}

// responsesContentParts builds the content array for one multimodal message
// item: input_text, input_image (URL or data: URI), input_file (PDF base64 or a
// URL), input_video (URL or provider file reference) and input_audio (base64
// wav/mp3).
func responsesContentParts(m Message) (responses.ResponseInputMessageContentListParam, error) {
	var out responses.ResponseInputMessageContentListParam
	if m.Content != "" {
		out = append(out, responses.ResponseInputContentUnionParam{
			OfInputText: &responses.ResponseInputTextParam{Text: m.Content},
		})
	}
	for _, p := range m.Parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				out = append(out, responses.ResponseInputContentUnionParam{
					OfInputText: &responses.ResponseInputTextParam{Text: p.Text},
				})
			}
		case "image":
			url := p.URL
			if url == "" {
				if p.Data == "" {
					return nil, fmt.Errorf("openai-responses: image part has neither url nor data")
				}
				url = dataURI(p)
			}
			out = append(out, responses.ResponseInputContentUnionParam{
				OfInputImage: &responses.ResponseInputImageParam{ImageURL: param.NewOpt(url)},
			})
		case "file":
			file := &responses.ResponseInputFileParam{}
			switch {
			case p.Data != "":
				file.FileData = param.NewOpt(dataURI(p))
			case p.URL != "":
				// A provider file reference or a plain file URL; the Responses
				// input_file part takes either form as file_url.
				file.FileURL = param.NewOpt(p.URL)
			default:
				return nil, fmt.Errorf("openai-responses: file part requires data or url")
			}
			if p.Name != "" {
				file.Filename = param.NewOpt(p.Name)
			}
			out = append(out, responses.ResponseInputContentUnionParam{OfInputFile: file})
		case "video":
			// Responses video inputs accept a URL or provider file reference
			// (MiniMax mm_file://, Kimi ms://), not inline base64. The SDK's
			// input-content union has no input_video variant, so the part is
			// built through param.Override — its raw-JSON escape hatch, which
			// MarshalUnion marshals verbatim when no typed variant is set.
			if p.URL == "" {
				return nil, fmt.Errorf("openai-responses: video part requires a url source")
			}
			out = append(out, rawResponsesPart(map[string]any{
				"type": "input_video", "video_url": p.URL, "file_id": p.URL,
			}))
		case "audio":
			if p.Data == "" {
				return nil, fmt.Errorf("openai-responses: audio part requires inline base64 data")
			}
			format := audioFormat(p.MIME)
			if format == "" {
				return nil, fmt.Errorf("openai-responses: input_audio supports wav and mp3 only (got %q)", p.MIME)
			}
			out = append(out, rawResponsesPart(map[string]any{
				"type": "input_audio", "input_audio": map[string]any{"data": p.Data, "format": format},
			}))
		default:
			return nil, fmt.Errorf("openai-responses does not support %s parts", p.Type)
		}
	}
	if len(out) == 0 {
		out = append(out, responses.ResponseInputContentUnionParam{
			OfInputText: &responses.ResponseInputTextParam{Text: ""},
		})
	}
	return out, nil
}
