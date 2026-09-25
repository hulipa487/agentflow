package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"agentflow/internal/config"
	"agentflow/internal/core/media"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// Anthropic Messages API, always called with stream=true (Chat buffers it).
// https://docs.anthropic.com/en/api/messages
//
// The wire is owned by the official SDK: request encoding, SSE parsing and the
// incremental assembly of the reply (text, thinking blocks with their
// signatures, tool_use blocks with their parsed input) all come from
// anthropic-sdk-go. This file keeps only the translation between the
// provider-neutral Message/Opts shapes and the SDK's params, plus the terminal
// event mapping the rest of the runtime consumes.
//
// Two departures from the previous hand-rolled client:
//
//   - The SDK owns retries, and its default is 2. The Manager already has its
//     own policy (openWithRetry), so the SDK's is disabled rather than
//     multiplied by it.
//   - The MiniMax-compatible `video` content block has no variant in the SDK's
//     writable union, so it is built through param.Override — the SDK's own
//     raw-JSON escape hatch, which MarshalUnion marshals verbatim when no typed
//     variant is set. The extension survives the conversion.
func anthropicOpen(ctx context.Context, client *http.Client, cfg config.Model, msgs []Message, opts Opts) (<-chan event, bool, error) {
	base := resolveBase(cfg, "https://api.anthropic.com")
	system, turns, err := anthropicTurns(msgs)
	if err != nil {
		return nil, false, err
	}
	thinking, err := thinkingOf(cfg, opts)
	if err != nil {
		return nil, false, err
	}
	maxTokens := maxTokensOf(cfg, opts)

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(cfg.Model),
		MaxTokens: int64(maxTokens),
		Messages:  turns,
	}
	if system != "" {
		params.System = []anthropic.TextBlockParam{{Text: system}}
	}
	if opts.Temperature != nil {
		params.Temperature = param.NewOpt(*opts.Temperature)
	}
	if thinking != "" && thinking != ThinkingOff {
		// The Messages API has no explicit "off"; omitting the block is the
		// only way to leave thinking at the provider default.
		params.Thinking = anthropic.ThinkingConfigParamUnion{
			OfEnabled: &anthropic.ThinkingConfigEnabledParam{
				BudgetTokens: int64(anthropicBudgetFor(thinking, maxTokens)),
			},
		}
	}
	if len(opts.Tools) > 0 {
		params.Tools = anthropicTools(opts.Tools)
		if opts.ToolChoice != "" {
			params.ToolChoice = anthropicToolChoice(opts.ToolChoice)
		}
	}

	sdkOpts := []option.RequestOption{
		// The runtime resolves credentials itself: api_key is either a literal
		// or a ${VAR}/cred: reference the Manager expands before we get here.
		// Without this the SDK would also autoload ANTHROPIC_API_KEY and
		// friends from the process environment, so a model entry with no
		// api_key pointing at a third-party base_url would send the process's
		// real key to that host — where the hand-rolled client sent none.
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(base),
		option.WithHTTPClient(client),
		// The Manager's openWithRetry owns retry policy. Leaving the SDK's
		// default (2) in place would compound the two.
		option.WithMaxRetries(0),
	}
	if cfg.APIKey != "" {
		sdkOpts = append(sdkOpts, option.WithAPIKey(cfg.APIKey))
	}
	api := anthropic.NewClient(sdkOpts...)

	stream := api.Messages.NewStreaming(ctx, params)

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
		return nil, anthropicRetryable(serr), fmt.Errorf("%s/v1/messages -> %w", base, serr)
	}
	first := stream.Current()

	events := make(chan event, 16)
	go func() {
		defer close(events)
		defer stream.Close()

		// acc is the SDK's own accumulator: it applies each event to a Message,
		// assembling thinking deltas with their signature and tool_use blocks
		// from their partial-JSON fragments. That is exactly the bookkeeping this
		// file used to do by hand.
		var acc anthropic.Message

		handle := func(ev anthropic.MessageStreamEventUnion) bool {
			switch ev.Type {
			case "content_block_delta":
				if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
					select {
					case events <- event{delta: ev.Delta.Text}:
					case <-ctx.Done():
						return false
					}
				}
			case "error":
				events <- event{err: fmt.Errorf("anthropic stream error: %s", ev.RawJSON())}
				return false
			}
			// An event the accumulator cannot apply is not fatal: keep what has
			// been assembled and let the stream finish.
			_ = acc.Accumulate(ev)
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
			events <- event{err: fmt.Errorf("anthropic stream: %w", serr)}
			return
		}

		// Terminal payload, in block order. Thinking blocks are replayed
		// verbatim — the Messages API validates their signatures when a
		// multi-round tool loop continues with thinking enabled — and their
		// texts are joined for reading.
		var thinkingBlocks []map[string]any
		var thoughts []string
		var calls []ToolCall
		for _, b := range acc.Content {
			switch b.Type {
			case "thinking":
				t := b.AsThinking()
				thinkingBlocks = append(thinkingBlocks, map[string]any{
					"type": "thinking", "thinking": t.Thinking, "signature": t.Signature,
				})
				if t.Thinking != "" {
					thoughts = append(thoughts, t.Thinking)
				}
			case "redacted_thinking":
				thinkingBlocks = append(thinkingBlocks, map[string]any{
					"type": "redacted_thinking", "data": b.AsRedactedThinking().Data,
				})
			case "tool_use":
				tu := b.AsToolUse()
				calls = append(calls, ToolCall{ID: tu.ID, Name: tu.Name, Args: parseArgs(string(tu.Input))})
			}
		}
		thinkingText := strings.Join(thoughts, "\n\n")
		usage := Usage{
			Input:      int(acc.Usage.InputTokens),
			Output:     int(acc.Usage.OutputTokens),
			Cached:     int(acc.Usage.CacheReadInputTokens),
			CacheWrite: int(acc.Usage.CacheCreationInputTokens),
		}

		// Two events, tool calls first: StreamNext relies on this order to
		// report a tool-call turn's usage rather than its cost as zero.
		if len(calls) > 0 {
			events <- event{toolCalls: calls, thinking: thinkingText, thinkingBlocks: thinkingBlocks}
		}
		events <- event{usage: usage, thinking: thinkingText, thinkingBlocks: thinkingBlocks}
	}()
	return events, false, nil
}

// anthropicRetryable classifies an establishment failure. 429 and 5xx are worth
// another attempt; anything else (401, 400, a malformed base_url) is not.
func anthropicRetryable(err error) bool {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode >= 500
	}
	// A transport failure never reached the provider, so it is retryable.
	return true
}

// anthropicTools maps the provider-neutral ToolDef list onto the Messages API
// tool shape. The JSON Schema a loop supplies is passed through as-is: known
// keys are lifted onto the typed fields and anything else rides in
// ExtraFields, so a schema the SDK has never heard of survives intact.
func anthropicTools(tools []ToolDef) []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		p := &anthropic.ToolParam{
			Name:        t.Name,
			InputSchema: anthropicInputSchema(t.Parameters),
		}
		if t.Description != "" {
			p.Description = param.NewOpt(t.Description)
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: p})
	}
	return out
}

// anthropicInputSchema splits a raw JSON Schema map across the SDK's typed
// fields (properties, required) and ExtraFields (everything else, including
// keys the SDK does not model).
func anthropicInputSchema(schema map[string]any) anthropic.ToolInputSchemaParam {
	out := anthropic.ToolInputSchemaParam{}
	extra := map[string]any{}
	for k, v := range schema {
		switch k {
		case "properties":
			out.Properties = v
		case "required":
			// The typed field is []string; a schema may carry anything, so an
			// unexpected shape falls through to ExtraFields rather than being
			// dropped silently.
			if list, ok := v.([]any); ok {
				req := make([]string, 0, len(list))
				for _, item := range list {
					s, ok := item.(string)
					if !ok {
						req = nil
						break
					}
					req = append(req, s)
				}
				if req != nil {
					out.Required = req
					continue
				}
			}
			extra[k] = v
		case "type":
			// The SDK marshals its own "object" default; keeping a second copy
			// here would duplicate the key.
		default:
			extra[k] = v
		}
	}
	if len(extra) > 0 {
		out.ExtraFields = extra
	}
	return out
}

func anthropicToolChoice(choice string) anthropic.ToolChoiceUnionParam {
	switch choice {
	case "auto":
		return anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}}
	case "none":
		return anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}
	case "any":
		return anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}}
	default:
		return anthropic.ToolChoiceUnionParam{}
	}
}

// anthropicTurns converts the neutral Message list to Anthropic's shape: the
// leading system message is extracted (returned as the first result), and the
// remaining turns are reshaped so that assistant tool_calls become tool_use
// content blocks and "tool" role turns become user tool_result blocks. Adjacent
// tool results are merged into one user message, as Anthropic requires.
// Multimodal turns carry Parts as image/document content blocks. An assistant
// turn with ThinkingBlocks replays them ahead of everything else (thinking
// blocks must open the content array) — required on continuations when
// thinking is enabled; a thinking-only assistant turn also takes the array form.
func anthropicTurns(msgs []Message) (system string, turns []anthropic.MessageParam, err error) {
	if len(msgs) > 0 && msgs[0].Role == "system" {
		system = msgs[0].Content
		msgs = msgs[1:]
	}
	for _, m := range msgs {
		switch {
		case m.Role == "assistant" && (len(m.ToolCalls) > 0 || len(m.ThinkingBlocks) > 0):
			content := []anthropic.ContentBlockParamUnion{}
			// Replayed thinking blocks go first, verbatim (signatures are
			// validated by the provider).
			for _, raw := range m.ThinkingBlocks {
				if b, ok := anthropicThinkingBlock(raw); ok {
					content = append(content, b)
				}
			}
			if m.Content != "" {
				content = append(content, anthropic.ContentBlockParamUnion{
					OfText: &anthropic.TextBlockParam{Text: m.Content},
				})
			}
			for _, tc := range m.ToolCalls {
				content = append(content, anthropic.ContentBlockParamUnion{
					OfToolUse: &anthropic.ToolUseBlockParam{
						ID:    tc.ID,
						Name:  tc.Name,
						Input: tc.Args,
					},
				})
			}
			turns = append(turns, anthropic.NewAssistantMessage(content...))
		case m.Role == "tool":
			result := m.ToolResult
			if result == nil {
				result = m.Content
			}
			block := anthropic.ContentBlockParamUnion{
				OfToolResult: &anthropic.ToolResultBlockParam{
					ToolUseID: m.ToolCallID,
					Content:   anthropicToolResultContent(result),
				},
			}
			// Merge into a preceding *user* tool_result message if present, which
			// is the shape Anthropic requires: adjacent tool results share one
			// user turn.
			//
			// The role check is load-bearing. The assistant branch above also
			// builds a content-block array, so keying the merge on the content
			// type alone appended a tool_result to the *assistant* message that
			// carried the tool_use — and Anthropic rejects a tool_result in an
			// assistant turn. Every real tool round-trip 400ed; the tests missed
			// it because they assert substrings of the outgoing body, never
			// which turn a block landed on.
			if n := len(turns); n > 0 && turns[n-1].Role == anthropic.MessageParamRoleUser {
				turns[n-1].Content = append(turns[n-1].Content, block)
				continue
			}
			turns = append(turns, anthropic.NewUserMessage(block))
		default:
			if len(m.Parts) > 0 {
				content := []anthropic.ContentBlockParamUnion{}
				if m.Content != "" {
					content = append(content, anthropic.ContentBlockParamUnion{
						OfText: &anthropic.TextBlockParam{Text: m.Content},
					})
				}
				for _, p := range m.Parts {
					blk, perr := anthropicPart(p)
					if perr != nil {
						return system, nil, perr
					}
					content = append(content, blk)
				}
				turns = append(turns, anthropic.MessageParam{
					Role:    anthropic.MessageParamRole(m.Role),
					Content: content,
				})
			} else {
				turns = append(turns, anthropic.MessageParam{
					Role:    anthropic.MessageParamRole(m.Role),
					Content: []anthropic.ContentBlockParamUnion{{
						OfText: &anthropic.TextBlockParam{Text: m.Content},
					}},
				})
			}
		}
	}
	return system, turns, nil
}

// anthropicThinkingBlock rebuilds one replayed thinking block. The stored
// blocks are the raw provider payloads (see Reply.ThinkingBlocks), so an
// unrecognised type is dropped rather than guessed at.
func anthropicThinkingBlock(raw map[string]any) (anthropic.ContentBlockParamUnion, bool) {
	switch raw["type"] {
	case "thinking":
		text, _ := raw["thinking"].(string)
		sig, _ := raw["signature"].(string)
		return anthropic.ContentBlockParamUnion{
			OfThinking: &anthropic.ThinkingBlockParam{Thinking: text, Signature: sig},
		}, true
	case "redacted_thinking":
		data, _ := raw["data"].(string)
		return anthropic.ContentBlockParamUnion{
			OfRedactedThinking: &anthropic.RedactedThinkingBlockParam{Data: data},
		}, true
	default:
		return anthropic.ContentBlockParamUnion{}, false
	}
}

// anthropicToolResultContent renders a tool result as a content block array.
// The SDK models tool_result content as blocks only — it has no bare-string
// variant — so a string or an arbitrary Go value is wrapped in a text block.
// The Messages API accepts both forms; this is the one the SDK can emit.
func anthropicToolResultContent(result any) []anthropic.ToolResultBlockParamContentUnion {
	text := ""
	switch v := result.(type) {
	case nil:
		// nothing to report
	case string:
		text = v
	default:
		text = string(mustJSON(v))
	}
	if text == "" {
		return nil
	}
	return []anthropic.ToolResultBlockParamContentUnion{{
		OfText: &anthropic.TextBlockParam{Text: text},
	}}
}

// anthropicPart maps one media part to an Anthropic content block. Images
// accept base64 or URL sources; PDFs are document blocks (base64 only). Video
// follows the MiniMax M3 Anthropic-compatible extension. Audio is rejected
// honestly — the Messages API has no audio block type.
func anthropicPart(p media.Part) (anthropic.ContentBlockParamUnion, error) {
	switch p.Type {
	case "text":
		return anthropic.ContentBlockParamUnion{
			OfText: &anthropic.TextBlockParam{Text: p.Text},
		}, nil
	case "image":
		if p.Data != "" {
			return anthropic.ContentBlockParamUnion{
				OfImage: &anthropic.ImageBlockParam{
					Source: anthropic.ImageBlockParamSourceUnion{
						OfBase64: &anthropic.Base64ImageSourceParam{
							Data:      p.Data,
							MediaType: anthropic.Base64ImageSourceMediaType(p.MIME),
						},
					},
				},
			}, nil
		}
		if p.URL != "" {
			return anthropic.ContentBlockParamUnion{
				OfImage: &anthropic.ImageBlockParam{
					Source: anthropic.ImageBlockParamSourceUnion{
						OfURL: &anthropic.URLImageSourceParam{URL: p.URL},
					},
				},
			}, nil
		}
		return anthropic.ContentBlockParamUnion{}, fmt.Errorf("anthropic: image part has neither url nor data")
	case "file":
		if p.MIME != "application/pdf" {
			return anthropic.ContentBlockParamUnion{}, fmt.Errorf("anthropic: document blocks support application/pdf only (got %q)", p.MIME)
		}
		if p.Data == "" {
			return anthropic.ContentBlockParamUnion{}, fmt.Errorf("anthropic: pdf part requires inline base64 data")
		}
		return anthropic.ContentBlockParamUnion{
			OfDocument: &anthropic.DocumentBlockParam{
				Source: anthropic.DocumentBlockParamSourceUnion{
					OfBase64: &anthropic.Base64PDFSourceParam{Data: p.Data},
				},
			},
		}, nil
	case "video":
		// MiniMax M3's Anthropic-compatible API (https://platform.minimax.cn/
		// docs/api-reference/text-anthropic-api): type:"video" with a source
		// that may be base64 inline data, a URL, or an mm_file:// reference.
		// The Messages API proper has no video block and the SDK's writable
		// union has no variant for it, so the block is built through
		// param.Override — the SDK's raw-JSON escape hatch, which MarshalUnion
		// marshals verbatim when no typed variant is set.
		src := map[string]any{"type": "base64", "media_type": p.MIME, "data": p.Data}
		if p.Data == "" && p.URL != "" {
			src = map[string]any{"type": "url", "url": p.URL}
		}
		return param.Override[anthropic.ContentBlockParamUnion](map[string]any{
			"type": "video", "source": src,
		}), nil
	default:
		return anthropic.ContentBlockParamUnion{}, fmt.Errorf("anthropic does not support %s parts", p.Type)
	}
}
