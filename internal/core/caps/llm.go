// Package caps adapts drivers to session op handlers. Phase 1: llm.*.
package caps

import (
	"context"
	"encoding/json"

	"agentflow/internal/core/budget"
	"agentflow/internal/core/session"
	"agentflow/internal/drivers/llm"
)

// LLMHandlers exposes the llm driver as session op handlers.
func LLMHandlers(m *llm.Manager) map[string]session.OpHandler {
	toLLM := func(ms []session.ChatMessage) []llm.Message {
		out := make([]llm.Message, len(ms))
		for i, mm := range ms {
			out[i] = llm.Message{
				Role:       mm.Role,
				Content:    mm.Content,
				ToolCallID: mm.ToolCallID,
				ToolResult: mm.ToolResult,
			}
			if len(mm.ToolCalls) > 0 {
				calls := make([]llm.ToolCall, len(mm.ToolCalls))
				for j, tc := range mm.ToolCalls {
					calls[j] = llm.ToolCall{ID: tc.ID, Name: tc.Name, Args: tc.Args}
				}
				out[i].ToolCalls = calls
			}
		}
		return out
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

	return map[string]session.OpHandler{
		"llm.chat": func(ctx context.Context, op session.Op) (string, bool) {
			text, toolCalls, usage, err := m.Chat(ctx, op.Model, toLLM(op.Messages), optsOf(op))
			if err != nil {
				return fail(err)
			}
			b, _ := json.Marshal(map[string]any{"text": text, "usage": usage, "tool_calls": toolCalls})
			return string(b), true
		},

		"llm.embed": func(ctx context.Context, op session.Op) (string, bool) {
			vectors, usage, err := m.Embed(ctx, op.Model, op.Inputs)
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

		"llm.stream.open": func(ctx context.Context, op session.Op) (string, bool) {
			id, err := m.StreamOpen(ctx, op.Model, toLLM(op.Messages), optsOf(op))
			if err != nil {
				return fail(err)
			}
			b, _ := json.Marshal(map[string]any{"id": id})
			return string(b), true
		},

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

func errString(err error) any {
	if err == nil {
		return nil
	}
	return err.Error()
}

// MeteredLLMHandlers wraps LLMHandlers with budget reserve/commit/release.
// Before each llm.chat call, it reserves a conservative estimate (the model's
// MaxTokens or a default). After the call, it commits the actual reported usage
// and releases unused reservation. On exhaustion, the call returns a structured
// error instead of reaching the provider.
func MeteredLLMHandlers(m *llm.Manager, pool *budget.Pool) map[string]session.OpHandler {
	base := LLMHandlers(m)
	chat := base["llm.chat"]
	metered := func(ctx context.Context, op session.Op) (string, bool) {
		// Reserve a conservative estimate before the call.
		estimate := int64(op.MaxTokens)
		if estimate <= 0 {
			estimate = 4096
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
		return resp, ok
	}
	base["llm.chat"] = metered

	// Embeddings consume input tokens only; reserve a modest per-text
	// estimate, then commit the reported usage.
	embed := base["llm.embed"]
	meteredEmbed := func(ctx context.Context, op session.Op) (string, bool) {
		n := len(op.Inputs)
		if n < 1 {
			n = 1
		}
		lease, err := pool.Reserve(int64(512 * n))
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
		_ = pool.Commit(lease, int64(512*n))
		return resp, ok
	}
	base["llm.embed"] = meteredEmbed
	return base
}
