// Package caps adapts drivers to session op handlers. Phase 1: llm.*.
package caps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"agentflow/internal/core/accounting"
	"agentflow/internal/core/budget"
	"agentflow/internal/core/chatreq"
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
	return llmHandlers(m, ms, nil, "")
}

func effectiveModel(ctx context.Context, opModel string) string {
	if opModel != "" {
		return opModel
	}
	return session.AgentModelFromCtx(ctx)
}

func llmHandlers(m *llm.Manager, ms media.Store, mediaSvc *media.Service, agent string) map[string]session.OpHandler {
	scopeOf := func(ctx context.Context) string {
		if p := session.ScopePrefixFromCtx(ctx); p != "" {
			return p
		}
		if agent != "" {
			return "agent:" + agent
		}
		return ""
	}
	toLLM := func(ctx context.Context, ms0 []session.ChatMessage) ([]llm.Message, error) {
		out := make([]llm.Message, len(ms0))
		scope := scopeOf(ctx)
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
					if err := resolvePartForScope(ctx, scope, &parts[j], ms, mediaSvc); err != nil {
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
	// optsOf is the one Op-to-Opts conversion, shared by llm.chat, llm.stream
	// and llm.forward: an option missing here reaches no provider, whatever the
	// drivers can serialise.
	optsOf := func(ctx context.Context, op session.Op) llm.Opts {
		return llm.Opts{
			Temperature:       op.Temperature,
			MaxTokens:         op.MaxTokens,
			Thinking:          op.Thinking,
			Tools:             toToolDefs(op.Tools),
			ToolChoice:        op.ToolChoice,
			ParallelToolCalls: op.ParallelToolCalls,
			TopP:              op.TopP,
			Stop:              op.Stop,
			Seed:              op.Seed,
			ResponseFormat:    op.ResponseFormat,
			Passthrough:       forwardedFromCtx(ctx),
		}
	}
	chatOf := func(ctx context.Context, op session.Op) (string, bool) {
		msgs, err := toLLM(ctx, op.Messages)
		if err != nil {
			return failJSON(err)
		}
		reply, err := m.Chat(ctx, effectiveModel(ctx, op.Model), msgs, optsOf(ctx, op))
		if err != nil {
			return failJSON(err)
		}
		replyMedia, err := ingestLLMOutputs(ctx, scopeOf(ctx), reply.Media, ms, mediaSvc)
		if err != nil {
			return failJSON(err)
		}
		payload := map[string]any{
			"text":            reply.Text,
			"thinking":        reply.Thinking,
			"thinking_blocks": reply.ThinkingBlocks,
			"usage":           reply.Usage,
			"tool_calls":      reply.ToolCalls,
			"finish_reason":   finishOf(reply.FinishReason, len(reply.ToolCalls)),
		}
		if len(replyMedia) > 0 {
			payload["media"] = replyMedia
		}
		b, _ := json.Marshal(payload)
		return string(b), true
	}
	streamOf := func(ctx context.Context, op session.Op) (string, bool) {
		msgs, err := toLLM(ctx, op.Messages)
		if err != nil {
			return failJSON(err)
		}
		id, err := m.StreamOpen(ctx, effectiveModel(ctx, op.Model), msgs, optsOf(ctx, op))
		if err != nil {
			return failJSON(err)
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
			scope := scopeOf(ctx)
			for i := range parts {
				if err := resolvePartForScope(ctx, scope, &parts[i], ms, mediaSvc); err != nil {
					return failJSON(err)
				}
			}
			vectors, usage, err := m.EmbedParts(ctx, op.Model, parts, llm.EmbedOpts{
				Task:       op.Task,
				Dimensions: op.Dimensions,
				Merged:     op.Merged,
			})
			if err != nil {
				return failJSON(err)
			}
			b, _ := json.Marshal(map[string]any{"vectors": vectors, "usage": usage})
			return string(b), true
		},

		"llm.rerank": func(ctx context.Context, op session.Op) (string, bool) {
			results, err := m.Rerank(ctx, op.Model, op.Text, op.Documents, op.TopN)
			if err != nil {
				return failJSON(err)
			}
			b, _ := json.Marshal(map[string]any{"results": results})
			return string(b), true
		},

		"llm.stream.open": streamOf,

		"llm.stream.next": func(ctx context.Context, op session.Op) (string, bool) {
			frame, err := m.StreamNext(ctx, op.Stream)
			if err != nil && !frame.Done {
				return failJSON(err)
			}
			var frameMedia []media.Part
			if len(frame.Media) > 0 {
				var ierr error
				frameMedia, ierr = ingestLLMOutputs(ctx, scopeOf(ctx), frame.Media, ms, mediaSvc)
				if ierr != nil {
					return failJSON(ierr)
				}
			}
			payload := map[string]any{
				"delta":           frame.Delta,
				"done":            frame.Done,
				"usage":           frame.Usage,
				"tool_calls":      frame.ToolCalls,
				"thinking":        frame.Thinking,
				"thinking_blocks": frame.ThinkingBlocks,
				"finish_reason":   frame.FinishReason,
				"error":           errString(err),
			}
			if len(frameMedia) > 0 {
				payload["media"] = frameMedia
			}
			b, _ := json.Marshal(payload)
			return string(b), true
		},

		"llm.stream.close": func(ctx context.Context, op session.Op) (string, bool) {
			m.StreamClose(op.Stream)
			return "true", true
		},
	}
}

// resolvePartForScope resolves a handle part via the centralized media.Service
// (enforcing per-scope grants and unified files/media lookup) when configured,
// falling back to resolvePart over a bare media.Store.
func resolvePartForScope(ctx context.Context, scope string, p *media.Part, ms media.Store, svc *media.Service) error {
	if p.Handle == "" || p.Data != "" {
		return nil
	}
	if svc != nil {
		blob, err := svc.MaterializeBytes(ctx, scope, *p, media.TargetSpec{MaxBytes: maxInlineMedia})
		if err != nil {
			return err
		}
		if blob.MIME != "" {
			p.MIME = blob.MIME
		} else if p.MIME == "" {
			p.MIME = "application/octet-stream"
		}
		p.Data = base64.StdEncoding.EncodeToString(blob.Bytes)
		return nil
	}
	return resolvePart(p, ms)
}

// ingestLLMOutputs stores model-generated media (base64/data) into the
// centralized media store so only lightweight handles cross the Lua border.
func ingestLLMOutputs(ctx context.Context, scope string, in []media.Part, ms media.Store, svc *media.Service) ([]media.Part, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]media.Part, 0, len(in))
	for _, p := range in {
		if p.Data == "" {
			out = append(out, p)
			continue
		}
		if svc != nil {
			stored, err := svc.IngestBase64(ctx, p.Data, media.IngestOptions{
				MIME:   p.MIME,
				Name:   p.Name,
				Scope:  scope,
				Origin: "llm",
				Policy: media.Policy{MaxBytes: maxInlineMedia},
			})
			if err != nil {
				return nil, fmt.Errorf("store model media: %w", err)
			}
			if p.Type != "" {
				stored.Type = p.Type
			}
			out = append(out, stored)
			continue
		}
		if ms != nil {
			raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(p.Data))
			if err != nil {
				return nil, fmt.Errorf("decode model media: %w", err)
			}
			mime := p.MIME
			if mime == "" {
				mime = media.SniffMIME(raw, p.Name)
			}
			ref, err := ms.Put(strings.NewReader(string(raw)), mime, media.Policy{MaxBytes: maxInlineMedia})
			if err != nil {
				return nil, fmt.Errorf("store model media: %w", err)
			}
			out = append(out, media.Part{Type: media.Classify(mime), MIME: mime, Handle: ref.Handle, Name: p.Name, Size: ref.Size})
			continue
		}
		out = append(out, p)
	}
	return out, nil
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
	// Requests is the OpenAI-channel request registry. llm.forward and
	// openai.request resolve the current turn's request through it; nil leaves
	// both ops reporting that no forwarded request is available.
	Requests *chatreq.Registry
	// Media is the centralized media service for scope-checked handle
	// materialization and LLM output media ingestion.
	Media *media.Service
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
	lease, err := mt.Quota.Reserve(ctx, session.BillingUUIDFromCtx(ctx), amount)
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
//
// A pool that shares the deployment's ledger refreshes here: the moment before
// a reservation is the moment the figure has to be right, and the read is
// throttled inside the pool.
func (mt Metering) reserve(ctx context.Context, amount int64) (*budget.Lease, error) {
	if mt.Pool == nil {
		return nil, nil
	}
	mt.Pool.Refresh(ctx)
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

// record writes one ledger row. Attribution comes from the context's BILLING
// identity — always the person, never the group membership uuid: a group
// context must not fragment the ledger, because the ledger is the billing
// mechanism (F23). So a channel turn is charged to its user, a delegated turn
// to the tenant that delegated, and engine-fired work lands in the shared
// service bucket rather than on somebody's account.
func (mt Metering) record(ctx context.Context, op session.Op, kind string, u usageCounts, ok bool) {
	if mt.Ledger == nil {
		return
	}
	rec := runtime.UsageRecord{
		UserID:     session.BillingUUIDFromCtx(ctx),
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
	base := llmHandlers(m, ms, mt.Media, mt.Agent)
	chat := base["llm.chat"]
	metered := func(ctx context.Context, op session.Op) (string, bool) {
		op.Model = effectiveModel(ctx, op.Model)
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
		lease, err := mt.reserve(ctx, estimate)
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
		lease, err := mt.reserve(ctx, estimate)
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
		// model and kind are what the terminal frame's ledger row is written
		// with. The next op carries only a stream handle, so without these a
		// streamed call would be recorded against no model at all.
		model string
		kind  string
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
		op.Model = effectiveModel(ctx, op.Model)
		estimate := int64(op.MaxTokens)
		if estimate <= 0 {
			estimate = 4096
		}
		estimate += int64(1600 * countMediaParts(op.Messages))
		ql, qerr := mt.reserveQuota(ctx, estimate)
		if qerr != nil {
			return denyJSON("user_quota_exhausted", qerr), false
		}
		lease, err := mt.reserve(ctx, estimate)
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
			kind := "stream"
			if forwardedFromCtx(ctx) {
				kind = "forward"
			}
			holds[out.ID] = &streamHold{budget: lease, quota: ql, estimate: estimate, model: op.Model, kind: kind}
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
			Error *string `json:"error"`
		}
		if err := json.Unmarshal([]byte(resp), &frame); err != nil || !frame.Done {
			return resp, ok
		}
		// A terminal frame can also be a provider failure part-way through the
		// answer. The call is still recorded — it happened, and whatever usage
		// arrived is real — but not as a success.
		succeeded := frame.Error == nil || *frame.Error == ""
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
		ledgerOp, kind := op, "stream"
		if h != nil {
			ledgerOp.Model, kind = h.model, h.kind
		}
		mt.record(ctx, ledgerOp, kind, u, succeeded)
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

	// llm.forward is the OpenAI channel's pass-through. It is built on the same
	// metered stream path as llm.stream.open/next/close — quota hold, usage
	// ledger, metrics, user keys — and adds only what the channel needs: the
	// request the turn arrived with, and relay of its text deltas when the
	// safety profile cannot veto the answer (D9).
	base["llm.forward"] = func(ctx context.Context, op session.Op) (string, bool) {
		var req *chatreq.Request
		if mt.Requests != nil {
			r, err := currentRequest(ctx, mt.Requests, "llm.forward")
			if err == nil {
				req = r
			} else if len(op.Messages) == 0 {
				return failJSON(err)
			}
		} else if len(op.Messages) == 0 {
			return failJSON(errors.New("llm.forward: no OpenAI request registry is configured on this runtime; pass opts.messages"))
		}
		msgs := op.Messages
		if len(msgs) == 0 {
			msgs = req.Messages
		}
		if len(op.Prepend) > 0 {
			msgs = append(append([]session.ChatMessage{}, op.Prepend...), msgs...)
		}
		if len(op.Append) > 0 {
			// Full slice expression: msgs may still be the registry's own
			// slice, and an append with spare capacity would write into it.
			msgs = append(msgs[:len(msgs):len(msgs)], op.Append...)
		}
		var tools []session.ToolSpec
		if req != nil {
			tools = append(tools, req.Tools...)
		}
		tools = append(tools, op.Tools...)

		model := op.Model
		temperature, topP := op.Temperature, op.TopP
		parallel, stop, seed := op.ParallelToolCalls, op.Stop, op.Seed
		responseFormat, toolChoice := op.ResponseFormat, op.ToolChoice
		maxTokens := op.MaxTokens
		if req != nil {
			if temperature == nil {
				temperature = req.Temperature
			}
			if topP == nil {
				topP = req.TopP
			}
			if parallel == nil {
				parallel = req.ParallelToolCalls
			}
			if len(stop) == 0 {
				stop = req.Stop
			}
			if seed == nil {
				seed = req.Seed
			}
			if responseFormat == nil {
				responseFormat = req.ResponseFormat
			}
			if toolChoice == "" {
				toolChoice = req.ToolChoice
			}
			if maxTokens == 0 {
				maxTokens = req.MaxTokens
			}
		}
		if model == "" {
			model = session.AgentModelFromCtx(ctx)
		}
		if cap := op.MaxTokensCap; cap > 0 && (maxTokens == 0 || maxTokens > cap) {
			maxTokens = cap
		}

		openOp := session.Op{
			Type: "llm.stream.open", Model: model, Messages: msgs, Tools: tools,
			ToolChoice: toolChoice, Temperature: temperature, TopP: topP,
			ParallelToolCalls: parallel, Stop: stop, Seed: seed,
			ResponseFormat: responseFormat, MaxTokens: maxTokens, Thinking: op.Thinking,
		}
		ledgerOp := session.Op{Type: "llm.forward", Model: model}
		// From here on the call is a client's request being forwarded: the
		// drivers drop what they cannot express instead of refusing it, and the
		// ledger row is a "forward" row.
		ctx = withForwarded(ctx)
		resp, ok := base["llm.stream.open"](ctx, openOp)
		if !ok {
			// A refusal (quota, budget) writes no ledger row — the call never
			// happened. A provider failure did happen, and the forward contract
			// says it is recorded like any other attempt.
			if isDenial(resp) {
				return resp, false
			}
			mt.record(ctx, ledgerOp, "forward", usageCounts{}, false)
			return forwardFailure(resp), false
		}
		var handle struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(resp), &handle); err != nil || handle.ID == "" {
			return failJSON(fmt.Errorf("llm.forward: the stream handle is missing from %s", resp))
		}
		closeStream := func() {
			base["llm.stream.close"](ctx, session.Op{Type: "llm.stream.close", Stream: handle.ID})
		}

		var text strings.Builder
		var usage llm.Usage
		var calls []llm.ToolCall
		var thinking, finishReason string
		var blocks []map[string]any
		var outMedia []media.Part
		for {
			frameResp, ok := base["llm.stream.next"](ctx, session.Op{Type: "llm.stream.next", Stream: handle.ID})
			if !ok {
				// A mid-stream transport failure or a cancelled client: the
				// terminal usage never arrived, so the metered next handler
				// wrote nothing. The forward contract records one row for every
				// attempt, and close releases the reservation.
				mt.record(ctx, ledgerOp, "forward", usageCounts{}, false)
				closeStream()
				return forwardFailure(frameResp), false
			}
			var frame struct {
				Delta          string           `json:"delta"`
				Done           bool             `json:"done"`
				Usage          llm.Usage        `json:"usage"`
				ToolCalls      []llm.ToolCall   `json:"tool_calls"`
				Thinking       string           `json:"thinking"`
				ThinkingBlocks []map[string]any `json:"thinking_blocks"`
				Media          []media.Part     `json:"media"`
				FinishReason   string           `json:"finish_reason"`
				Error          *string          `json:"error"`
			}
			if err := json.Unmarshal([]byte(frameResp), &frame); err != nil {
				mt.record(ctx, ledgerOp, "forward", usageCounts{}, false)
				closeStream()
				return failJSON(fmt.Errorf("llm.forward: bad stream frame: %w", err))
			}
			if frame.Delta != "" {
				text.WriteString(frame.Delta)
				// Relay only when the client asked for a stream AND the safety
				// profile cannot veto the answer afterwards (D7/D9). Otherwise
				// the text is returned whole for session.reply.
				if req != nil && req.Stream && session.StreamsLiveFromCtx(ctx) {
					if sink, ok := session.DeltaSinkFromCtx(ctx); ok && sink != nil {
						if err := sink(frame.Delta); err != nil {
							mt.record(ctx, ledgerOp, "forward", usageCounts{}, false)
							closeStream()
							return failJSON(err)
						}
					}
				}
			}
			if frame.Done {
				usage = frame.Usage
				calls = frame.ToolCalls
				thinking = frame.Thinking
				blocks = frame.ThinkingBlocks
				outMedia = frame.Media
				finishReason = frame.FinishReason
				if frame.Error != nil && *frame.Error != "" {
					// The metered next handler already wrote the row for this
					// terminal frame, as a failure; do not write a second.
					closeStream()
					return failJSON(errors.New(upstreamErrorMarker + *frame.Error))
				}
				break
			}
		}
		closeStream() // idempotent; StreamNext already closed on done

		outCalls := make([]session.ToolCallSpec, len(calls))
		for i, c := range calls {
			outCalls[i] = session.ToolCallSpec{ID: c.ID, Name: c.Name, Args: c.Args, Arguments: c.Raw}
		}
		replyMap := map[string]any{
			"text":            text.String(),
			"tool_calls":      outCalls,
			"finish_reason":   finishOf(finishReason, len(outCalls)),
			"usage":           usage,
			"thinking":        thinking,
			"thinking_blocks": blocks,
		}
		if len(outMedia) > 0 {
			replyMap["media"] = outMedia
		}
		b, err := json.Marshal(replyMap)
		if err != nil {
			return failJSON(err)
		}
		return string(b), true
	}

	// openai.request exposes the full request as Lua data, size-capped. The
	// cap is the design: above it the loop should forward the request rather
	// than materialise it.
	base["openai.request"] = func(ctx context.Context, op session.Op) (string, bool) {
		if mt.Requests == nil {
			return failJSON(errors.New("openai.request: this runtime has no OpenAI request registry"))
		}
		req, err := currentRequest(ctx, mt.Requests, "openai.request")
		if err != nil {
			return failJSON(err)
		}
		b, err := json.Marshal(req.LuaView())
		if err != nil {
			return failJSON(err)
		}
		if len(b) > maxForwardRequestJSON {
			return failJSON(fmt.Errorf("openai.request: this request is %d bytes of JSON, over the %d-byte limit; forward it with llm.forward instead of materialising it", len(b), maxForwardRequestJSON))
		}
		return string(b), true
	}
	return base
}

// finishOf is the finish reason a reply reports: the provider's own when it
// gave one, else what the reply's shape implies.
func finishOf(reported string, toolCalls int) string {
	switch {
	case reported != "":
		return reported
	case toolCalls > 0:
		return "tool_calls"
	default:
		return "stop"
	}
}

type forwardedKey struct{}

// withForwarded marks a model call as a client's request being forwarded.
func withForwarded(ctx context.Context) context.Context {
	return context.WithValue(ctx, forwardedKey{}, true)
}

func forwardedFromCtx(ctx context.Context) bool {
	v, _ := ctx.Value(forwardedKey{}).(bool)
	return v
}

// isDenial reports whether an op response is the structured refusal denyJSON
// produces (quota or budget), as opposed to a provider failure whose message
// merely mentions one of those words.
func isDenial(resp string) bool {
	var d struct {
		OK    *bool  `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(resp), &d); err != nil || d.OK == nil || *d.OK {
		return false
	}
	return d.Error == "user_quota_exhausted" || d.Error == "budget_exhausted"
}

// upstreamErrorMarker prefixes a failure of the model call itself. The loop
// sees it in the error it is raised with, and a channel that reports errors by
// kind reads it to tell "the model failed" (502 on the OpenAI endpoint) from a
// failure of the loop's own code.
const upstreamErrorMarker = "upstream_error: "

// forwardFailure marks an op failure response (a JSON string) as an upstream
// failure, unless it is a refusal of the request itself, which the drivers mark
// "invalid_request" and which the client can act on.
func forwardFailure(resp string) string {
	var msg string
	if err := json.Unmarshal([]byte(resp), &msg); err != nil {
		return resp
	}
	if strings.Contains(msg, "invalid_request") || strings.Contains(msg, "has no equivalent") || strings.HasPrefix(msg, "tool_choice") {
		return resp
	}
	b, _ := json.Marshal(upstreamErrorMarker + msg)
	return string(b)
}

// maxForwardRequestJSON is the largest request openai.request materialises.
const maxForwardRequestJSON = 4 << 20

// failJSON is the (response, ok) pair an op handler returns for an error: the
// message crosses the bridge as a JSON string, which the Lua side raises with.
func failJSON(err error) (string, bool) {
	b, _ := json.Marshal(err.Error())
	return string(b), false
}

// currentRequest resolves the request behind the turn being served. A ref this
// process does not know is the regional gap (5.4): a keyed session moved
// instances between turns, and the body stayed where it arrived.
func currentRequest(ctx context.Context, reg *chatreq.Registry, opName string) (*chatreq.Request, error) {
	m := session.TurnMessageFromCtx(ctx)
	if m == nil || m.Payload == nil {
		return nil, fmt.Errorf("%s: this turn has no forwarded request; use it inside an openai-channel turn", opName)
	}
	ref, _ := m.Payload["request_ref"].(string)
	if ref == "" {
		return nil, fmt.Errorf("%s: this turn carries no request_ref; the openai channel sets one on every turn it delivers", opName)
	}
	req, ok := reg.Get(ref)
	if ok && req.ID != m.ReplyTo {
		// The ref is client-visible data in the payload. It must name the
		// request this turn is answering, not merely some request that is
		// registered: a ref passed along from another turn does not qualify.
		return nil, fmt.Errorf("%s: the request_ref on this turn does not belong to it", opName)
	}
	if !ok {
		return nil, fmt.Errorf("%s: the request behind this turn (ref %s) is not in this process — a keyed session that moved instances keeps only the message, not its body; forward with opts.messages, or give the route ephemeral sessions", opName, ref)
	}
	return req, nil
}
