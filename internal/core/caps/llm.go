// Package caps adapts drivers to session op handlers. Phase 1: llm.*.
package caps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"agentflow/internal/core/accounting"
	"agentflow/internal/core/budget"
	"agentflow/internal/core/media"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/runtime"
	"agentflow/internal/core/session"
	"agentflow/internal/core/tools"
	"agentflow/internal/drivers/llm"
)

// maxInlineMedia bounds a single resolved media part handed to a provider
// (decoded bytes). Larger blobs fail the call with an explicit error instead
// of silently OOM-ing a worker.
const maxInlineMedia = 20 << 20 // 20 MiB

// LLMHandlers exposes the llm driver as session op handlers. ms is the blob
// store used to resolve part handles to inline base64 just before the
// provider request; nil disables handle resolution (handle parts error).
func LLMHandlers(m *llm.Manager, ms media.Store) map[string]session.OpHandler {
	toLLM := func(ms0 []session.ChatMessage) ([]llm.Message, error) {
		out := make([]llm.Message, len(ms0))
		for i, mm := range ms0 {
			out[i] = llm.Message{
				Role:           mm.Role,
				Content:        mm.Content,
				ToolCallID:     mm.ToolCallID,
				ToolResult:     mm.ToolResult,
				ThinkingBlocks: mm.ThinkingBlocks,
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
			// Schemas arrive from Lua: an empty Go `required: []` crossed the
			// bridge as an empty table and decodes back as an object, which is
			// invalid JSON Schema (strict providers 400 on it). Normalize so
			// no schema can emit "required": {} regardless of origin.
			out[i] = llm.ToolDef{Name: s.Name, Description: s.Description, Parameters: tools.NormalizeSchema(s.Parameters)}
		}
		return out
	}
	optsOf := func(op session.Op) llm.Opts {
		return llm.Opts{
			Temperature: op.Temperature,
			MaxTokens:   op.MaxTokens,
			Thinking:    op.Thinking,
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
		reply, err := m.Chat(ctx, op.Model, msgs, optsOf(op))
		if err != nil {
			return fail(err)
		}
		b, _ := json.Marshal(map[string]any{
			"text":            reply.Text,
			"thinking":        reply.Thinking,
			"thinking_blocks": reply.ThinkingBlocks,
			"usage":           reply.Usage,
			"tool_calls":      reply.ToolCalls,
		})
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
			frame, err := m.StreamNext(ctx, op.Stream)
			if err != nil && !frame.Done {
				return fail(err)
			}
			b, _ := json.Marshal(map[string]any{
				"delta":           frame.Delta,
				"done":            frame.Done,
				"usage":           frame.Usage,
				"tool_calls":      frame.ToolCalls,
				"thinking":        frame.Thinking,
				"thinking_blocks": frame.ThinkingBlocks,
				"error":           errString(err),
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
func resolvePart(p *media.Part, ms media.Store) error {
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

// UsageRecorder receives one record per metered LLM call, for per-user
// accounting. A nil recorder disables accounting (tests, minimal setups).
type UsageRecorder interface {
	RecordUsage(rec runtime.UsageRecord, withEvent bool) error
}

// Metering carries what the metered handlers need beyond the driver: the
// agent's budget pool (nil = no budget configured, so calls are accounted but
// not limited), the per-user quota, the attribution for the ledger, and where
// to complain when a ledger write fails.
type Metering struct {
	Pool   *budget.Pool
	Quota  *accounting.Quota
	Agent  string
	Ledger UsageRecorder // nil = no accounting
	Events bool          // also write the per-call detail row
	Log    *slog.Logger
}

// denyJSON is the structured refusal a loop receives when a limit bites, so it
// can tell the user which wall they hit rather than reporting a provider error.
func denyJSON(kind string, err error) string {
	b, _ := json.Marshal(map[string]any{"ok": false, "error": kind, "detail": err.Error()})
	return string(b)
}

// reserveQuota checks the user's daily quota before a call. It returns
// (lease, nil) to proceed, (nil, nil) when there is no quota to enforce (no
// quota configured, or a context with no user), and (nil, err) when the call
// must be refused. An infrastructure failure fails open — counted and logged,
// never silent — because an unreadable ledger must not stop the runtime.
func (mt Metering) reserveQuota(ctx context.Context, amount int64) (*accounting.Lease, error) {
	if mt.Quota == nil {
		return nil, nil
	}
	lease, err := mt.Quota.Reserve(ctx, session.UserUUIDFromCtx(ctx), amount)
	if err == nil {
		return lease, nil
	}
	if errors.Is(err, accounting.ErrQuotaExhausted) {
		metrics.Inc("agentflow_user_quota_denied")
		return nil, err
	}
	metrics.Inc("agentflow_user_quota_unavailable")
	if mt.Log != nil {
		mt.Log.Warn("quota check failed; allowing the call", "err", err)
	}
	return nil, nil
}

func (mt Metering) releaseQuota(l *accounting.Lease) {
	if l != nil {
		l.Release()
	}
}

// reserve takes a budget lease when the deployment configured one. A nil pool
// means the call is accounted but unlimited.
func (mt Metering) reserve(amount int64) (*budget.Lease, error) {
	if mt.Pool == nil {
		return nil, nil
	}
	return mt.Pool.Reserve(amount)
}

func (mt Metering) release(l *budget.Lease) {
	if mt.Pool != nil && l != nil {
		mt.Pool.Release(l)
	}
}

func (mt Metering) commit(l *budget.Lease, actual int64) {
	if mt.Pool != nil && l != nil {
		_ = mt.Pool.Commit(l, actual)
	}
}

// usageCounts mirrors the token fields of a metered reply.
type usageCounts struct {
	Input, Output, Cached, CacheWrite, Reasoning int
}

// replyUsage extracts provider-reported usage from a handler response.
func replyUsage(resp string) (usageCounts, bool) {
	var result struct {
		Usage struct {
			Input      int `json:"input"`
			Output     int `json:"output"`
			Cached     int `json:"cached"`
			CacheWrite int `json:"cache_write"`
			Reasoning  int `json:"reasoning"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		return usageCounts{}, false
	}
	u := usageCounts{
		Input:      result.Usage.Input,
		Output:     result.Usage.Output,
		Cached:     result.Usage.Cached,
		CacheWrite: result.Usage.CacheWrite,
		Reasoning:  result.Usage.Reasoning,
	}
	return u, u.Input+u.Output > 0
}

// record writes one ledger row. Attribution comes from the context's user
// stamp, so a channel turn is charged to its user and engine-fired work lands
// in the shared service bucket rather than on somebody's account.
func (mt Metering) record(ctx context.Context, op session.Op, kind string, u usageCounts, ok bool) {
	if mt.Ledger == nil {
		return
	}
	rec := runtime.UsageRecord{
		UserID:     session.UserUUIDFromCtx(ctx),
		Agent:      mt.Agent,
		Model:      op.Model,
		Kind:       kind,
		Input:      u.Input,
		Output:     u.Output,
		Cached:     u.Cached,
		CacheWrite: u.CacheWrite,
		Reasoning:  u.Reasoning,
		OK:         ok,
	}
	if err := mt.Ledger.RecordUsage(rec, mt.Events); err != nil {
		metrics.Inc("agentflow_usage_record_failed")
		if mt.Log != nil {
			mt.Log.Warn("usage record failed", "err", err, "user_id", rec.UserID, "kind", kind)
		}
	}
}

// MeteredLLMHandlers wraps LLMHandlers with budget reserve/commit/release and
// per-call accounting. Before each llm.chat call, it reserves a conservative
// estimate (the model's MaxTokens or a default, plus a flat surcharge per media
// part — images and PDFs cost real input tokens even before any text). After
// the call, it commits the actual reported usage and releases unused
// reservation. On exhaustion, the call returns a structured error instead of
// reaching the provider.
//
// Every metered call — success or failure — reaches the ledger, so "how many
// invocations" is answerable per user as well as "how many tokens".
func MeteredLLMHandlers(m *llm.Manager, ms media.Store, mt Metering) map[string]session.OpHandler {
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
		// The user's daily quota and the agent's budget both bound the call;
		// whichever is hit first refuses it.
		ql, qerr := mt.reserveQuota(ctx, estimate)
		if qerr != nil {
			return denyJSON("user_quota_exhausted", qerr), false
		}
		lease, err := mt.reserve(estimate)
		if err != nil {
			mt.releaseQuota(ql)
			metrics.Inc("agentflow_budget_denied")
			return denyJSON("budget_exhausted", err), false
		}
		resp, ok := chat(ctx, op)
		if !ok {
			mt.release(lease)
			mt.releaseQuota(ql)
			// The provider failed without reporting usage: the ledger records
			// the attempt (and its tokens stay zero).
			mt.record(ctx, op, "chat", usageCounts{}, false)
			return resp, false
		}
		u, haveUsage := replyUsage(resp)
		actual := estimate
		if haveUsage {
			actual = int64(u.Input + u.Output)
		}
		mt.commit(lease, actual)
		metrics.Inc("agentflow_llm_calls")
		metrics.Add("agentflow_llm_tokens", actual)
		// Ledger first, then release the quota hold: the durable record has to
		// land before the reservation drops, or a concurrent call could read a
		// stale (smaller) spend and over-admit.
		mt.record(ctx, op, "chat", u, true)
		mt.releaseQuota(ql)
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
		ql, qerr := mt.reserveQuota(ctx, estimate)
		if qerr != nil {
			return denyJSON("user_quota_exhausted", qerr), false
		}
		lease, err := mt.reserve(estimate)
		if err != nil {
			mt.releaseQuota(ql)
			metrics.Inc("agentflow_budget_denied")
			return denyJSON("budget_exhausted", err), false
		}
		resp, ok := embed(ctx, op)
		if !ok {
			mt.release(lease)
			mt.releaseQuota(ql)
			mt.record(ctx, op, "embed", usageCounts{}, false)
			return resp, false
		}
		u, haveUsage := replyUsage(resp)
		if haveUsage {
			mt.commit(lease, int64(u.Input))
		} else {
			mt.commit(lease, estimate)
		}
		metrics.Inc("agentflow_llm_calls")
		mt.record(ctx, op, "embed", u, true)
		mt.releaseQuota(ql)
		return resp, ok
	}
	base["llm.embed"] = meteredEmbed

	// Streaming holds a budget and quota reservation from open to its terminal
	// frame (or to close, if the consumer walks away), so a streaming loop is
	// limited like a buffered one instead of slipping past the gate. Usage is
	// only complete on the terminal frame, so the ledger records there.
	type streamHold struct {
		budget   *budget.Lease
		quota    *accounting.Lease
		estimate int64
	}
	var (
		holdMu sync.Mutex
		holds  = map[string]*streamHold{}
	)
	takeHold := func(id string) *streamHold {
		holdMu.Lock()
		defer holdMu.Unlock()
		h := holds[id]
		delete(holds, id)
		return h
	}

	open := base["llm.stream.open"]
	base["llm.stream.open"] = func(ctx context.Context, op session.Op) (string, bool) {
		estimate := int64(op.MaxTokens)
		if estimate <= 0 {
			estimate = 4096
		}
		estimate += int64(1600 * countMediaParts(op.Messages))
		ql, qerr := mt.reserveQuota(ctx, estimate)
		if qerr != nil {
			return denyJSON("user_quota_exhausted", qerr), false
		}
		lease, err := mt.reserve(estimate)
		if err != nil {
			mt.releaseQuota(ql)
			metrics.Inc("agentflow_budget_denied")
			return denyJSON("budget_exhausted", err), false
		}
		resp, ok := open(ctx, op)
		if !ok {
			mt.release(lease)
			mt.releaseQuota(ql)
			return resp, false
		}
		// Key the hold by the handle the caller was given.
		var out struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(resp), &out); err == nil && out.ID != "" {
			holdMu.Lock()
			holds[out.ID] = &streamHold{budget: lease, quota: ql, estimate: estimate}
			holdMu.Unlock()
			return resp, true
		}
		// No handle to key a hold on: release rather than leak the reservation.
		mt.release(lease)
		mt.releaseQuota(ql)
		return resp, true
	}

	next := base["llm.stream.next"]
	base["llm.stream.next"] = func(ctx context.Context, op session.Op) (string, bool) {
		resp, ok := next(ctx, op)
		if !ok {
			return resp, ok
		}
		var frame struct {
			Done  bool `json:"done"`
			Usage struct {
				Input      int `json:"input"`
				Output     int `json:"output"`
				Cached     int `json:"cached"`
				CacheWrite int `json:"cache_write"`
				Reasoning  int `json:"reasoning"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(resp), &frame); err != nil || !frame.Done {
			return resp, ok
		}
		u := usageCounts{
			Input:      frame.Usage.Input,
			Output:     frame.Usage.Output,
			Cached:     frame.Usage.Cached,
			CacheWrite: frame.Usage.CacheWrite,
			Reasoning:  frame.Usage.Reasoning,
		}
		actual := int64(u.Input + u.Output)
		h := takeHold(op.Stream)
		if h != nil {
			if actual <= 0 {
				actual = h.estimate // provider reported nothing: charge the reserve
			}
			mt.commit(h.budget, actual)
		}
		metrics.Inc("agentflow_llm_calls")
		metrics.Add("agentflow_llm_tokens", actual)
		mt.record(ctx, op, "stream", u, true)
		if h != nil {
			mt.releaseQuota(h.quota)
		}
		return resp, ok
	}

	closeStream := base["llm.stream.close"]
	base["llm.stream.close"] = func(ctx context.Context, op session.Op) (string, bool) {
		// An abandoned stream still gives its reservation back — otherwise the
		// user's quota would stay held until the process restarted.
		if h := takeHold(op.Stream); h != nil {
			mt.release(h.budget)
			mt.releaseQuota(h.quota)
		}
		return closeStream(ctx, op)
	}

	// Rerank reports no token usage; the invocation is what can be accounted.
	rerank := base["llm.rerank"]
	base["llm.rerank"] = func(ctx context.Context, op session.Op) (string, bool) {
		resp, ok := rerank(ctx, op)
		mt.record(ctx, op, "rerank", usageCounts{}, ok)
		return resp, ok
	}
	return base
}
