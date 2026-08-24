// Package caps adapts drivers to session op handlers. Phase 1: llm.*.
package caps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"agentflow/internal/core/budget"
	"agentflow/internal/core/media"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/llm"
)

// maxInlineMedia bounds a single resolved media part handed to a provider
// (decoded bytes). Larger blobs fail the call with an explicit error instead
// of silently OOM-ing a worker.
const maxInlineMedia = 20 << 20 // 20 MiB

// LLMHandlers exposes the llm driver as session op handlers. ms is the blob
// store used to resolve part handles to inline base64 just before the
// provider request; nil disables handle resolution (handle parts error).
func LLMHandlers(m *llm.Manager, ms *media.Store) map[string]session.OpHandler {
	toLLM := func(ms0 []session.ChatMessage) ([]llm.Message, error) {
		out := make([]llm.Message, len(ms0))
		for i, mm := range ms0 {
			out[i] = llm.Message{
				Role:       mm.Role,
				Content:    mm.Content,
				ToolCallID: mm.ToolCallID,
				ToolResult: mm.ToolResult,
			}
			if len(mm.Parts) > 0 {
				parts := make([]media.Part, len(mm.Parts))
				copy(parts, mm.Parts)
				for j := range parts {
					if err := resolvePart(&parts[j], ms); err != nil {
						return nil, err
					}
				}
				out[i].Parts = parts
			}
			if len(mm.ToolCalls) > 0 {
				calls := make([]llm.ToolCall, len(mm.ToolCalls))
				for j, tc := range mm.ToolCalls {
					calls[j] = llm.ToolCall{ID: tc.ID, Name: tc.Name, Args: tc.Args}
				}
				out[i].ToolCalls = calls
			}
		}
		return out, nil
	}
	toToolDefs := func(specs []session.ToolSpec) []llm.ToolDef {
		out := make([]llm.ToolDef, len(specs))
		for i, s := range specs {
			out[i] = llm.ToolDef{Name: s.Name, Description: s.Description, Parameters: s.Parameters}
		}
		return out
	}
	optsOf := func(op session.Op) llm.Opts {
		return llm.Opts{
			Temperature: op.Temperature,
			MaxTokens:   op.MaxTokens,
			Tools:       toToolDefs(op.Tools),
			ToolChoice:  op.ToolChoice,
		}
	}
	fail := func(err error) (string, bool) {
		b, _ := json.Marshal(err.Error())
		return string(b), false
	}
	chatOf := func(ctx context.Context, op session.Op) (string, bool) {
		msgs, err := toLLM(op.Messages)
		if err != nil {
			return fail(err)
		}
		text, toolCalls, usage, err := m.Chat(ctx, op.Model, msgs, optsOf(op))
		if err != nil {
			return fail(err)
		}
		b, _ := json.Marshal(map[string]any{"text": text, "usage": usage, "tool_calls": toolCalls})
		return string(b), true
	}
	streamOf := func(ctx context.Context, op session.Op) (string, bool) {
		msgs, err := toLLM(op.Messages)
		if err != nil {
			return fail(err)
		}
		id, err := m.StreamOpen(ctx, op.Model, msgs, optsOf(op))
		if err != nil {
			return fail(err)
		}
		b, _ := json.Marshal(map[string]any{"id": id})
		return string(b), true
	}

	return map[string]session.OpHandler{
		"llm.chat": chatOf,

		"llm.embed": func(ctx context.Context, op session.Op) (string, bool) {
			// Resolve blob handles to inline base64 before the request, the
			// same rule as llm.chat parts — bytes only enter at the wire.
			parts := make([]media.Part, len(op.Inputs))
			copy(parts, op.Inputs)
			for i := range parts {
				if err := resolvePart(&parts[i], ms); err != nil {
					return fail(err)
				}
			}
			vectors, usage, err := m.EmbedParts(ctx, op.Model, parts, llm.EmbedOpts{
				Task:       op.Task,
				Dimensions: op.Dimensions,
				Merged:     op.Merged,
			})
			if err != nil {
				return fail(err)
			}
			b, _ := json.Marshal(map[string]any{"vectors": vectors, "usage": usage})
			return string(b), true
		},

		"llm.rerank": func(ctx context.Context, op session.Op) (string, bool) {
			results, err := m.Rerank(ctx, op.Model, op.Text, op.Documents, op.TopN)
			if err != nil {
				return fail(err)
			}
			b, _ := json.Marshal(map[string]any{"results": results})
			return string(b), true
		},

		"llm.stream.open": streamOf,

		"llm.stream.next": func(ctx context.Context, op session.Op) (string, bool) {
			delta, done, usage, toolCalls, err := m.StreamNext(ctx, op.Stream)
			if err != nil && !done {
				return fail(err)
			}
			b, _ := json.Marshal(map[string]any{
				"delta":      delta,
				"done":       done,
				"usage":      usage,
				"tool_calls": toolCalls,
				"error":      errString(err),
			})
			return string(b), true
		},

		"llm.stream.close": func(ctx context.Context, op session.Op) (string, bool) {
			m.StreamClose(op.Stream)
			return "true", true
		},
	}
}

// resolvePart dereferences a blob-store handle into inline base64 data.
// Already-inline (Data) and URL parts pass through untouched — providers
// decide what they can consume. This is the single point where media bytes
// enter a provider request; Lua never sees them.
func resolvePart(p *media.Part, ms *media.Store) error {
	if p.Handle == "" || p.Data != "" {
		return nil
	}
	if ms == nil {
		return fmt.Errorf("media handle %q but no media store configured", p.Handle)
	}
	if !media.ValidHandle(p.Handle) {
		return fmt.Errorf("malformed media handle %q", p.Handle)
	}
	b, err := ms.ReadAll(p.Handle, maxInlineMedia)
	if err != nil {
		return err
	}
	if p.MIME == "" {
		p.MIME = "application/octet-stream"
	}
	p.Data = base64.StdEncoding.EncodeToString(b)
	return nil
}

// countMediaParts totals the media (non-text) parts across a message list,
// for budget surcharges.
func countMediaParts(ms []session.ChatMessage) int {
	n := 0
	for _, m := range ms {
		for _, p := range m.Parts {
			if p.Type != "text" {
				n++
			}
		}
	}
	return n
}

func errString(err error) any {
	if err == nil {
		return nil
	}
	return err.Error()
}

// MeteredLLMHandlers wraps LLMHandlers with budget reserve/commit/release.
// Before each llm.chat call, it reserves a conservative estimate (the model's
// MaxTokens or a default, plus a flat surcharge per media part — images and
// PDFs cost real input tokens even before any text). After the call, it
// commits the actual reported usage and releases unused reservation. On
// exhaustion, the call returns a structured error instead of reaching the
// provider.
func MeteredLLMHandlers(m *llm.Manager, ms *media.Store, pool *budget.Pool) map[string]session.OpHandler {
	base := LLMHandlers(m, ms)
	chat := base["llm.chat"]
	metered := func(ctx context.Context, op session.Op) (string, bool) {
		// Reserve a conservative estimate before the call.
		estimate := int64(op.MaxTokens)
		if estimate <= 0 {
			estimate = 4096
		}
		// Media surcharge: provider-reported usage arrives after the call, but
		// reserve up front so a media-heavy turn cannot blow past the budget.
		estimate += int64(1600 * countMediaParts(op.Messages))
		lease, err := pool.Reserve(estimate)
		if err != nil {
			metrics.Inc("agentflow_budget_denied")
			b, _ := json.Marshal(map[string]any{
				"ok":     false,
				"error":  "budget_exhausted",
				"detail": err.Error(),
			})
			return string(b), false
		}
		resp, ok := chat(ctx, op)
		if !ok {
			pool.Release(lease)
			return resp, false
		}
		// Extract usage from the response and commit.
		var result map[string]any
		actual := estimate
		if err := json.Unmarshal([]byte(resp), &result); err == nil {
			if usage, ok := result["usage"].(map[string]any); ok {
				in, _ := usage["input"].(float64)
				out, _ := usage["output"].(float64)
				if in+out > 0 {
					actual = int64(in + out)
				}
			}
		}
		_ = pool.Commit(lease, actual)
		metrics.Inc("agentflow_llm_calls")
		metrics.Add("agentflow_llm_tokens", actual)
		return resp, ok
	}
	base["llm.chat"] = metered

	// Embeddings consume input tokens only; reserve a modest per-text
	// estimate (plus a media surcharge — images/audio embed far above text),
	// then commit the reported usage.
	embed := base["llm.embed"]
	meteredEmbed := func(ctx context.Context, op session.Op) (string, bool) {
		n := len(op.Inputs)
		if n < 1 {
			n = 1
		}
		estimate := int64(512 * n)
		for _, p := range op.Inputs {
			if p.Type != "text" {
				estimate += 2048
			}
		}
		lease, err := pool.Reserve(estimate)
		if err != nil {
			b, _ := json.Marshal(map[string]any{
				"ok":     false,
				"error":  "budget_exhausted",
				"detail": err.Error(),
			})
			return string(b), false
		}
		resp, ok := embed(ctx, op)
		if !ok {
			pool.Release(lease)
			return resp, false
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(resp), &result); err == nil {
			if usage, ok := result["usage"].(map[string]any); ok {
				if in, _ := usage["input"].(float64); in > 0 {
					_ = pool.Commit(lease, int64(in))
					return resp, ok
				}
			}
		}
		_ = pool.Commit(lease, estimate)
		return resp, ok
	}
	base["llm.embed"] = meteredEmbed
	return base
}
