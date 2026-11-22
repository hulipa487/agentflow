// Package telemetry exports runtime execution traces to OpenTelemetry (OTLP/HTTP
// JSON) and Langfuse-compatible collectors without CGO dependencies.
package telemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentflow/internal/core/session"
)

const (
	defaultBatchSize     = 64
	defaultFlushInterval = 5 * time.Second
	defaultHTTPTimeout   = 10 * time.Second
	defaultServiceName   = "agentflow"
	maxQueuedSpans       = 4096
	maxActiveTraces      = 2048
)

// Options configures an OTLP/HTTP JSON or Langfuse trace exporter.
type Options struct {
	Provider        string // "otlp" (default) | "langfuse"
	Endpoint        string
	ServiceName     string
	Headers         map[string]string
	PublicKey       string
	SecretKey       string
	SampleRate      float64 // (0, 1]; <= 0 disables sampling only when explicit; config defaults nil to 1.0
	IncludePayloads bool
	BatchSize       int
	FlushInterval   time.Duration
	Timeout         time.Duration
	HTTPClient      *http.Client
}

// Exporter converts session.TraceEvent records into OTLP spans and sends them
// asynchronously to an OTLP/HTTP JSON endpoint (including Langfuse's OTLP
// ingestion endpoint).
type Exporter struct {
	endpoint        string
	provider        string
	serviceName     string
	headers         map[string]string
	sampleRate      float64
	includePayloads bool
	batchSize       int
	flushInterval   time.Duration
	client          *http.Client
	log             *slog.Logger

	mu      sync.Mutex
	traces  map[string]*activeTrace
	queue   []otlpSpan
	dropped int64
	closed  bool

	notifyCh chan struct{}
	flushCh  chan chan error
	stopCh   chan struct{}
	doneCh   chan struct{}
}

type activeTrace struct {
	traceID    string
	rootSpanID string
	sampled    bool
	agent      string
	sessionID  string
	thread     string
	turn       string
	start      time.Time
	lastSeen   time.Time
	input      string
	lastReply  string
	tokensIn   int
	tokensOut  int
	tokensReas int
	hasErr     bool
	errMsg     string
	ops        []pendingOp
}

type pendingOp struct {
	spanID string
	op     string
	tool   string
	model  string
	msgs   int
	input  string
	start  time.Time
}

// New validates opts, builds the Exporter, and starts its background flush loop.
func New(opts Options, log *slog.Logger) (*Exporter, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	provider := strings.ToLower(strings.TrimSpace(opts.Provider))
	if provider == "" {
		provider = "otlp"
	}
	if provider != "otlp" && provider != "langfuse" {
		return nil, fmt.Errorf("telemetry: unsupported tracing provider %q (want otlp or langfuse)", opts.Provider)
	}
	endpoint, err := resolveEndpoint(provider, opts.Endpoint)
	if err != nil {
		return nil, err
	}

	headers := make(map[string]string, len(opts.Headers)+2)
	hasAuth := false
	for k, v := range opts.Headers {
		kTrim := strings.TrimSpace(k)
		if kTrim == "" {
			continue
		}
		headers[kTrim] = v
		if strings.EqualFold(kTrim, "authorization") && strings.TrimSpace(v) != "" {
			hasAuth = true
		}
	}
	if provider == "langfuse" && !hasAuth {
		if opts.PublicKey == "" || opts.SecretKey == "" {
			return nil, fmt.Errorf("telemetry: langfuse requires public_key and secret_key (or an Authorization header)")
		}
		tok := base64.StdEncoding.EncodeToString([]byte(opts.PublicKey + ":" + opts.SecretKey))
		headers["Authorization"] = "Basic " + tok
	}

	svc := strings.TrimSpace(opts.ServiceName)
	if svc == "" {
		svc = defaultServiceName
	}
	batch := opts.BatchSize
	if batch <= 0 {
		batch = defaultBatchSize
	}
	flushInterval := opts.FlushInterval
	if flushInterval <= 0 {
		flushInterval = defaultFlushInterval
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	sample := opts.SampleRate
	if sample > 1 {
		sample = 1
	}

	e := &Exporter{
		endpoint:        endpoint,
		provider:        provider,
		serviceName:     svc,
		headers:         headers,
		sampleRate:      sample,
		includePayloads: opts.IncludePayloads,
		batchSize:       batch,
		flushInterval:   flushInterval,
		client:          client,
		log:             log.With("component", "telemetry"),
		traces:          map[string]*activeTrace{},
		notifyCh:        make(chan struct{}, 1),
		flushCh:         make(chan chan error),
		stopCh:          make(chan struct{}),
		doneCh:          make(chan struct{}),
	}
	go e.worker()
	return e, nil
}

func resolveEndpoint(provider, raw string) (string, error) {
	ep := strings.TrimRight(strings.TrimSpace(raw), "/")
	if provider == "langfuse" && ep == "" {
		ep = "https://cloud.langfuse.com"
	}
	if ep == "" {
		return "", fmt.Errorf("telemetry: endpoint is required for provider %q", provider)
	}
	u, err := url.Parse(ep)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("telemetry: invalid http(s) endpoint %q", raw)
	}
	if strings.HasSuffix(ep, "/v1/traces") {
		return ep, nil
	}
	if provider == "langfuse" {
		if strings.HasSuffix(ep, "/api/public/otel") {
			return ep + "/v1/traces", nil
		}
		return ep + "/api/public/otel/v1/traces", nil
	}
	return ep + "/v1/traces", nil
}

// Trace implements session.Tracer without blocking the actor goroutine.
func (e *Exporter) Trace(ev session.TraceEvent) {
	if e == nil {
		return
	}
	if ev.Ts.IsZero() {
		ev.Ts = time.Now()
	}

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.handleEventLocked(ev)
	shouldWake := len(e.queue) >= e.batchSize
	e.mu.Unlock()

	if shouldWake {
		select {
		case e.notifyCh <- struct{}{}:
		default:
		}
	}
}

// Flush synchronously sends all currently queued spans.
func (e *Exporter) Flush(ctx context.Context) error {
	if e == nil {
		return nil
	}
	ch := make(chan error, 1)
	select {
	case e.flushCh <- ch:
	case <-e.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown finalizes active turns, flushes buffered spans, and stops the worker.
func (e *Exporter) Shutdown(ctx context.Context) error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		<-e.doneCh
		return nil
	}
	e.closed = true
	now := time.Now()
	for key, tr := range e.traces {
		if tr.sampled {
			e.enqueueTurnRootLocked(tr, now)
		}
		delete(e.traces, key)
	}
	e.mu.Unlock()

	close(e.stopCh)
	select {
	case <-e.doneCh:
	case <-ctx.Done():
		return ctx.Err()
	}
	return e.drainRemaining(ctx)
}

func (e *Exporter) handleEventLocked(ev session.TraceEvent) {
	key := ev.SessionID + "|" + ev.Turn
	switch ev.Kind {
	case "turn.start":
		tr := e.traces[key]
		if tr == nil {
			tr = e.newActiveTrace(ev)
			e.putActiveTraceLocked(key, tr)
		} else {
			tr.start = ev.Ts
			tr.lastSeen = ev.Ts
			if ev.Thread != "" {
				tr.thread = ev.Thread
			}
		}
		if e.includePayloads && ev.Text != "" {
			tr.input = ev.Text
		}

	case "op.start":
		tr := e.ensureActiveTraceLocked(key, ev)
		tr.lastSeen = ev.Ts
		if !tr.sampled {
			return
		}
		op := pendingOp{
			spanID: newSpanID(),
			op:     ev.Op,
			tool:   ev.Tool,
			model:  ev.Model,
			msgs:   ev.Messages,
			start:  ev.Ts,
		}
		if e.includePayloads {
			op.input = ev.Text
		}
		tr.ops = append(tr.ops, op)

	case "op.end":
		tr := e.ensureActiveTraceLocked(key, ev)
		tr.lastSeen = ev.Ts
		if !tr.sampled {
			return
		}
		start := ev.Ts
		if ev.DurationMS > 0 {
			start = ev.Ts.Add(-time.Duration(ev.DurationMS) * time.Millisecond)
		}
		spanID := newSpanID()
		var input string
		if idx := matchPendingOp(tr.ops, ev); idx >= 0 {
			pop := tr.ops[idx]
			tr.ops = append(tr.ops[:idx], tr.ops[idx+1:]...)
			spanID = pop.spanID
			if !pop.start.IsZero() && !pop.start.After(ev.Ts) {
				start = pop.start
			}
			input = pop.input
		}
		tr.tokensIn += ev.TokensIn
		tr.tokensOut += ev.TokensOut
		tr.tokensReas += ev.TokensReasoning

		attrs := e.baseAttrs(ev)
		attrs = appendKVString(attrs, "agentflow.op", ev.Op)
		if ev.Model != "" {
			attrs = appendKVString(attrs, "gen_ai.request.model", ev.Model)
			if e.provider == "langfuse" {
				attrs = appendKVString(attrs, "langfuse.observation.model.name", ev.Model)
			}
		}
		if ev.Tool != "" {
			attrs = appendKVString(attrs, "gen_ai.tool.name", ev.Tool)
		}
		if ev.TokensIn > 0 {
			attrs = appendKVInt(attrs, "gen_ai.usage.input_tokens", int64(ev.TokensIn))
		}
		if ev.TokensOut > 0 {
			attrs = appendKVInt(attrs, "gen_ai.usage.output_tokens", int64(ev.TokensOut))
		}
		if ev.TokensReasoning > 0 {
			attrs = appendKVInt(attrs, "gen_ai.usage.reasoning_tokens", int64(ev.TokensReasoning))
		}
		if ev.ToolCalls > 0 {
			attrs = appendKVInt(attrs, "agentflow.tool_calls", int64(ev.ToolCalls))
		}
		if ev.Messages > 0 {
			attrs = appendKVInt(attrs, "agentflow.messages", int64(ev.Messages))
		}
		opName, obsType, kind := classifyOp(ev.Op)
		if opName != "" {
			attrs = appendKVString(attrs, "gen_ai.operation.name", opName)
		}
		if e.provider == "langfuse" && obsType != "" {
			attrs = appendKVString(attrs, "langfuse.observation.type", obsType)
		}
		if e.includePayloads {
			if input != "" {
				attrs = appendKVString(attrs, "langfuse.observation.input", input)
			}
			if ev.Text != "" {
				attrs = appendKVString(attrs, "langfuse.observation.output", ev.Text)
			}
		}
		status := otlpStatus{Code: 1}
		if !ev.OK {
			status = otlpStatus{Code: 2, Message: ev.Err}
			tr.hasErr = true
			if tr.errMsg == "" {
				tr.errMsg = ev.Err
			}
		}
		e.enqueueSpanLocked(otlpSpan{
			TraceID:           tr.traceID,
			SpanID:            spanID,
			ParentSpanID:      tr.rootSpanID,
			Name:              spanNameForOp(ev),
			Kind:              kind,
			StartTimeUnixNano: formatNano(start),
			EndTimeUnixNano:   formatNano(ev.Ts),
			Attributes:        attrs,
			Status:            status,
		})

	case "reply":
		tr := e.ensureActiveTraceLocked(key, ev)
		tr.lastSeen = ev.Ts
		if e.includePayloads && ev.Text != "" {
			if tr.lastReply == "" {
				tr.lastReply = ev.Text
			} else {
				tr.lastReply += "\n\n" + ev.Text
			}
		}
		if !tr.sampled {
			return
		}
		attrs := e.baseAttrs(ev)
		if e.provider == "langfuse" {
			attrs = appendKVString(attrs, "langfuse.observation.type", "event")
		}
		if e.includePayloads && ev.Text != "" {
			attrs = appendKVString(attrs, "langfuse.observation.output", ev.Text)
		}
		status := otlpStatus{Code: 1}
		if !ev.OK {
			status = otlpStatus{Code: 2, Message: ev.Err}
		}
		e.enqueueSpanLocked(otlpSpan{
			TraceID:           tr.traceID,
			SpanID:            newSpanID(),
			ParentSpanID:      tr.rootSpanID,
			Name:              "agent.reply",
			Kind:              1,
			StartTimeUnixNano: formatNano(ev.Ts),
			EndTimeUnixNano:   formatNano(ev.Ts),
			Attributes:        attrs,
			Status:            status,
		})

	case "safety":
		tr := e.ensureActiveTraceLocked(key, ev)
		tr.lastSeen = ev.Ts
		if !tr.sampled {
			return
		}
		attrs := e.baseAttrs(ev)
		if ev.Op != "" {
			attrs = appendKVString(attrs, "agentflow.safety.stage", ev.Op)
		}
		if ev.Err != "" {
			attrs = appendKVString(attrs, "agentflow.safety.reason", ev.Err)
		}
		if ev.Safety != nil {
			if ev.Safety.Classifier != "" {
				attrs = appendKVString(attrs, "agentflow.safety.classifier", ev.Safety.Classifier)
			}
			if ev.Safety.Label != "" {
				attrs = appendKVString(attrs, "agentflow.safety.label", ev.Safety.Label)
			}
			if ev.Safety.Action != "" {
				attrs = appendKVString(attrs, "agentflow.safety.action", ev.Safety.Action)
			}
			if ev.Safety.Model != "" {
				attrs = appendKVString(attrs, "gen_ai.request.model", ev.Safety.Model)
			}
			if ev.Safety.Score > 0 {
				attrs = append(attrs, otlpKeyValue{Key: "agentflow.safety.score", Value: otlpValue{DoubleValue: &ev.Safety.Score}})
			}
			attrs = appendKVBool(attrs, "agentflow.safety.cached", ev.Safety.Cached)
		}
		if e.provider == "langfuse" {
			attrs = appendKVString(attrs, "langfuse.observation.type", "guardrail")
		}
		start := ev.Ts
		if ev.Safety != nil && ev.Safety.LatencyMS > 0 {
			start = ev.Ts.Add(-time.Duration(ev.Safety.LatencyMS) * time.Millisecond)
		}
		status := otlpStatus{Code: 1}
		if !ev.OK {
			status = otlpStatus{Code: 2, Message: ev.Err}
		}
		name := "safety"
		if ev.Op != "" {
			name = "safety." + ev.Op
		}
		e.enqueueSpanLocked(otlpSpan{
			TraceID:           tr.traceID,
			SpanID:            newSpanID(),
			ParentSpanID:      tr.rootSpanID,
			Name:              name,
			Kind:              1,
			StartTimeUnixNano: formatNano(start),
			EndTimeUnixNano:   formatNano(ev.Ts),
			Attributes:        attrs,
			Status:            status,
		})

	case "error", "reload":
		tr, hasActive := e.traces[key]
		if !hasActive {
			if !e.shouldSample(ev) {
				return
			}
			attrs := e.baseAttrs(ev)
			status := otlpStatus{Code: 1}
			if ev.Kind == "error" {
				status = otlpStatus{Code: 2, Message: ev.Err}
			}
			e.enqueueSpanLocked(otlpSpan{
				TraceID:           newTraceID(),
				SpanID:            newSpanID(),
				Name:              "agent." + ev.Kind,
				Kind:              1,
				StartTimeUnixNano: formatNano(ev.Ts),
				EndTimeUnixNano:   formatNano(ev.Ts),
				Attributes:        attrs,
				Status:            status,
			})
			return
		}
		tr.lastSeen = ev.Ts
		if ev.Kind == "error" {
			tr.hasErr = true
			if tr.errMsg == "" {
				tr.errMsg = ev.Err
			}
		}
		if !tr.sampled {
			return
		}
		attrs := e.baseAttrs(ev)
		status := otlpStatus{Code: 1}
		if ev.Kind == "error" {
			status = otlpStatus{Code: 2, Message: ev.Err}
		}
		e.enqueueSpanLocked(otlpSpan{
			TraceID:           tr.traceID,
			SpanID:            newSpanID(),
			ParentSpanID:      tr.rootSpanID,
			Name:              "agent." + ev.Kind,
			Kind:              1,
			StartTimeUnixNano: formatNano(ev.Ts),
			EndTimeUnixNano:   formatNano(ev.Ts),
			Attributes:        attrs,
			Status:            status,
		})

	case "turn.end":
		tr, ok := e.traces[key]
		if !ok {
			return
		}
		delete(e.traces, key)
		if tr.sampled {
			e.enqueueTurnRootLocked(tr, ev.Ts)
		}
	}
}

func (e *Exporter) enqueueTurnRootLocked(tr *activeTrace, end time.Time) {
	if end.Before(tr.start) {
		end = tr.start
	}
	attrs := e.baseAttrs(session.TraceEvent{
		Agent:     tr.agent,
		SessionID: tr.sessionID,
		Thread:    tr.thread,
		Turn:      tr.turn,
	})
	if e.provider == "langfuse" {
		if tr.sessionID != "" {
			attrs = appendKVString(attrs, "langfuse.session.id", tr.sessionID)
		}
		attrs = appendKVString(attrs, "langfuse.trace.name", "agent.turn:"+tr.agent)
		attrs = appendKVString(attrs, "langfuse.observation.type", "agent")
		if e.includePayloads {
			if tr.input != "" {
				attrs = appendKVString(attrs, "langfuse.trace.input", tr.input)
				attrs = appendKVString(attrs, "langfuse.observation.input", tr.input)
			}
			if tr.lastReply != "" {
				attrs = appendKVString(attrs, "langfuse.trace.output", tr.lastReply)
				attrs = appendKVString(attrs, "langfuse.observation.output", tr.lastReply)
			}
		}
	} else if e.includePayloads {
		if tr.input != "" {
			attrs = appendKVString(attrs, "gen_ai.prompt", tr.input)
		}
		if tr.lastReply != "" {
			attrs = appendKVString(attrs, "gen_ai.completion", tr.lastReply)
		}
	}
	if tr.tokensIn > 0 {
		attrs = appendKVInt(attrs, "gen_ai.usage.input_tokens", int64(tr.tokensIn))
	}
	if tr.tokensOut > 0 {
		attrs = appendKVInt(attrs, "gen_ai.usage.output_tokens", int64(tr.tokensOut))
	}
	if tr.tokensReas > 0 {
		attrs = appendKVInt(attrs, "gen_ai.usage.reasoning_tokens", int64(tr.tokensReas))
	}
	status := otlpStatus{Code: 1}
	if tr.hasErr {
		status = otlpStatus{Code: 2, Message: tr.errMsg}
	}
	e.enqueueSpanLocked(otlpSpan{
		TraceID:           tr.traceID,
		SpanID:            tr.rootSpanID,
		Name:              "agent.turn:" + tr.agent,
		Kind:              1,
		StartTimeUnixNano: formatNano(tr.start),
		EndTimeUnixNano:   formatNano(end),
		Attributes:        attrs,
		Status:            status,
	})
}

func (e *Exporter) ensureActiveTraceLocked(key string, ev session.TraceEvent) *activeTrace {
	if tr, ok := e.traces[key]; ok {
		return tr
	}
	tr := e.newActiveTrace(ev)
	e.putActiveTraceLocked(key, tr)
	return tr
}

func (e *Exporter) newActiveTrace(ev session.TraceEvent) *activeTrace {
	return &activeTrace{
		traceID:    newTraceID(),
		rootSpanID: newSpanID(),
		sampled:    e.shouldSample(ev),
		agent:      ev.Agent,
		sessionID:  ev.SessionID,
		thread:     ev.Thread,
		turn:       ev.Turn,
		start:      ev.Ts,
		lastSeen:   ev.Ts,
	}
}

func (e *Exporter) putActiveTraceLocked(key string, tr *activeTrace) {
	if len(e.traces) >= maxActiveTraces {
		var oldestKey string
		var oldest time.Time
		for k, v := range e.traces {
			if oldestKey == "" || v.lastSeen.Before(oldest) {
				oldestKey, oldest = k, v.lastSeen
			}
		}
		if oldestKey != "" {
			if old := e.traces[oldestKey]; old != nil && old.sampled {
				e.enqueueTurnRootLocked(old, old.lastSeen)
			}
			delete(e.traces, oldestKey)
		}
	}
	e.traces[key] = tr
}

func (e *Exporter) shouldSample(ev session.TraceEvent) bool {
	if e.sampleRate <= 0 {
		return false
	}
	if e.sampleRate >= 1 {
		return true
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(ev.SessionID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(ev.Turn))
	if ev.Turn == "" {
		var buf [8]byte
		_, _ = rand.Read(buf[:])
		_, _ = h.Write(buf[:])
	}
	frac := float64(h.Sum64()%10000) / 10000.0
	return frac < e.sampleRate
}

func (e *Exporter) baseAttrs(ev session.TraceEvent) []otlpKeyValue {
	var attrs []otlpKeyValue
	if ev.Agent != "" {
		attrs = appendKVString(attrs, "agentflow.agent", ev.Agent)
	}
	if ev.SessionID != "" {
		attrs = appendKVString(attrs, "agentflow.session_id", ev.SessionID)
	}
	if ev.Thread != "" {
		attrs = appendKVString(attrs, "agentflow.thread", ev.Thread)
	}
	if ev.Turn != "" {
		attrs = appendKVString(attrs, "agentflow.turn", ev.Turn)
	}
	return attrs
}

func (e *Exporter) enqueueSpanLocked(span otlpSpan) {
	if len(e.queue) >= maxQueuedSpans {
		drop := len(e.queue) - maxQueuedSpans + 1
		e.queue = append(e.queue[:0], e.queue[drop:]...)
		e.dropped += int64(drop)
	}
	e.queue = append(e.queue, span)
}

func (e *Exporter) worker() {
	defer close(e.doneCh)
	ticker := time.NewTicker(e.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-e.notifyCh:
			_ = e.drainOnce(context.Background())
		case <-ticker.C:
			_ = e.drainOnce(context.Background())
		case ch := <-e.flushCh:
			ch <- e.drainRemaining(context.Background())
		case <-e.stopCh:
			return
		}
	}
}

func (e *Exporter) drainRemaining(ctx context.Context) error {
	for {
		e.mu.Lock()
		n := len(e.queue)
		e.mu.Unlock()
		if n == 0 {
			return nil
		}
		if err := e.drainOnce(ctx); err != nil {
			return err
		}
	}
}

func (e *Exporter) drainOnce(ctx context.Context) error {
	e.mu.Lock()
	if len(e.queue) == 0 {
		e.mu.Unlock()
		return nil
	}
	n := len(e.queue)
	if n > e.batchSize {
		n = e.batchSize
	}
	batch := append([]otlpSpan(nil), e.queue[:n]...)
	e.queue = append(e.queue[:0], e.queue[n:]...)
	e.mu.Unlock()

	return e.sendBatch(ctx, batch)
}

func (e *Exporter) sendBatch(ctx context.Context, spans []otlpSpan) error {
	payload := otlpExportRequest{
		ResourceSpans: []otlpResourceSpans{{
			Resource: otlpResource{
				Attributes: []otlpKeyValue{
					{Key: "service.name", Value: otlpValue{StringValue: strPtr(e.serviceName)}},
				},
			},
			ScopeSpans: []otlpScopeSpans{{
				Scope: otlpScope{Name: "agentflow", Version: "1"},
				Spans: spans,
			}},
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.headers {
		req.Header.Set(k, v)
	}
	res, err := e.client.Do(req)
	if err != nil {
		e.log.Warn("telemetry export failed", "err", err, "provider", e.provider)
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		err := fmt.Errorf("telemetry HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(msg)))
		e.log.Warn("telemetry collector rejected spans", "err", err, "provider", e.provider)
		return err
	}
	return nil
}

func matchPendingOp(ops []pendingOp, ev session.TraceEvent) int {
	for i, op := range ops {
		if op.op == ev.Op && op.tool == ev.Tool && op.model == ev.Model {
			return i
		}
	}
	for i, op := range ops {
		if op.op == ev.Op {
			return i
		}
	}
	return -1
}

func classifyOp(op string) (genAIOp, langfuseType string, spanKind int) {
	switch {
	case strings.HasPrefix(op, "llm."):
		return "chat", "generation", 3
	case op == "tools.run":
		return "execute_tool", "tool", 1
	case strings.HasPrefix(op, "http."):
		return "", "span", 3
	case strings.HasPrefix(op, "store."), strings.HasPrefix(op, "files."):
		return "", "retriever", 1
	default:
		return "", "span", 1
	}
}

func spanNameForOp(ev session.TraceEvent) string {
	switch {
	case ev.Tool != "":
		return ev.Op + ":" + ev.Tool
	case ev.Model != "":
		return ev.Op + ":" + ev.Model
	case ev.Op != "":
		return ev.Op
	default:
		return ev.Kind
	}
}

func formatNano(t time.Time) string {
	return strconv.FormatInt(t.UnixNano(), 10)
}

func newTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		binary.LittleEndian.PutUint64(b[:8], uint64(time.Now().UnixNano()))
		binary.LittleEndian.PutUint64(b[8:], 1)
	}
	return hex.EncodeToString(b[:])
}

func newSpanID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		binary.LittleEndian.PutUint64(b[:], uint64(time.Now().UnixNano()))
	}
	return hex.EncodeToString(b[:])
}

type otlpExportRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes,omitempty"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	Status            otlpStatus     `json:"status,omitempty"`
}

type otlpKeyValue struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
}

type otlpStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}

func strPtr(s string) *string { return &s }

func appendKVString(attrs []otlpKeyValue, key, val string) []otlpKeyValue {
	return append(attrs, otlpKeyValue{Key: key, Value: otlpValue{StringValue: &val}})
}

func appendKVInt(attrs []otlpKeyValue, key string, val int64) []otlpKeyValue {
	s := strconv.FormatInt(val, 10)
	return append(attrs, otlpKeyValue{Key: key, Value: otlpValue{IntValue: &s}})
}

func appendKVBool(attrs []otlpKeyValue, key string, val bool) []otlpKeyValue {
	return append(attrs, otlpKeyValue{Key: key, Value: otlpValue{BoolValue: &val}})
}
