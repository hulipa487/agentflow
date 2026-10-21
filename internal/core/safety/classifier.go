package safety

import (
	"context"
	"errors"
	"strings"
	"time"

	"agentflow/internal/core/metrics"
)

// Classifier is the model-backed half of a classifier filter. Implementations
// live outside this package so safety stays free of provider imports; the
// dispatcher only sees the synchronous Filter interface.
type Classifier interface {
	Classify(ctx context.Context, req ClassifyRequest) (Verdict, error)
}

// ClassifyRequest is one text handed to a classifier.
type ClassifyRequest struct {
	Direction string // ingress | egress
	Text      string
	Source    string // channel | agent | scheduler | system
	Agent     string
}

// Verdict is a classifier's raw answer. It deliberately carries no action:
// the policy in ClassifierFilter decides what a label and score mean.
type Verdict struct {
	Label        string
	Score        float64
	Reason       string
	Model        string
	InputTokens  int
	OutputTokens int
	Latency      time.Duration
	Cached       bool
}

// Classifier errors the filter maps to its own policy knobs.
var (
	ErrClassifierSaturated   = errors.New("classifier saturated")
	ErrClassifierBudget      = errors.New("classifier budget exhausted")
	ErrClassifierUnavailable = errors.New("classifier circuit open")
)

// ErrClassifierInputTooLarge is returned before a provider call when the text
// exceeds the configured cap. It is an error, not a silent truncation: a
// payload after the cutoff must not become invisible.
var ErrClassifierInputTooLarge = errors.New("classifier input exceeds max_input_chars")

// ClassifierOptions is the deployment policy for one classifier filter.
type ClassifierOptions struct {
	Name          string
	Agent         string
	Classifier    Classifier
	Ingress       bool
	Egress        bool
	Sources       []string
	Threshold     float64
	Categories    map[string]string // label -> allow | review | block
	Mode          string            // enforce | shadow
	Timeout       time.Duration
	OnError       string // block | pass
	OnSaturated   string // block | pass
	OnBudget      string // block | pass
	MaxInputChars int
	Refusal       string
	Fallback      string
}

// ClassifierFilter runs a classifier on ingress and/or egress and applies the
// deployment's label policy. It is the only safety filter that can block on
// model output, and it is core-owned: a loop cannot uninstall it.
type ClassifierFilter struct {
	opts    ClassifierOptions
	sources map[string]bool
}

// NewClassifierFilter builds a filter from already-validated options.
func NewClassifierFilter(opts ClassifierOptions) *ClassifierFilter {
	if opts.Mode == "" {
		opts.Mode = "enforce"
	}
	if opts.OnError == "" {
		opts.OnError = "block"
	}
	if opts.OnSaturated == "" {
		opts.OnSaturated = opts.OnError
	}
	if opts.OnBudget == "" {
		opts.OnBudget = opts.OnError
	}
	src := map[string]bool{}
	for _, s := range opts.Sources {
		src[strings.ToLower(strings.TrimSpace(s))] = true
	}
	if len(src) == 0 {
		src["channel"] = true
		src["agent"] = true
	}
	return &ClassifierFilter{opts: opts, sources: src}
}

func (f *ClassifierFilter) Name() string { return "classifier:" + f.opts.Name }

// Ingress classifies an inbound message. A blocking verdict drops the turn
// (Skip) so the loop never sees the message; a review/allow verdict passes the
// text through with metadata for the trace and journal.
func (f *ClassifierFilter) Ingress(ctx context.Context, in IngressInput) IngressResult {
	if !f.opts.Ingress || !f.sources[strings.ToLower(in.Source)] {
		return IngressResult{Text: in.Message}
	}
	v, err := f.classify(ctx, "ingress", in.Message, in.Source)
	if err != nil {
		return f.errorResult(err, true, in.Message)
	}
	action := f.action(v)
	meta := f.meta(v, action)
	switch action {
	case "block":
		if f.opts.Mode == "shadow" {
			metrics.Inc("agentflow_moderation_shadow_blocks")
			return IngressResult{Text: in.Message, Reason: "shadow:block", Meta: meta}
		}
		metrics.Inc("agentflow_moderation_blocks")
		return IngressResult{Drop: true, Skip: true, Refusal: f.opts.Refusal, Reason: "classifier:" + v.Label, Meta: meta}
	case "review":
		metrics.Inc("agentflow_moderation_reviews")
		return IngressResult{Text: in.Message, Reason: "review:" + v.Label, Meta: meta}
	default:
		return IngressResult{Text: in.Message, Reason: "allow:" + v.Label, Meta: meta}
	}
}

// Egress classifies an outbound reply. A blocking verdict suppresses delivery
// and optionally supplies a core-authored fallback.
func (f *ClassifierFilter) Egress(ctx context.Context, in EgressInput) EgressResult {
	if !f.opts.Egress {
		return EgressResult{Text: in.Text}
	}
	v, err := f.classify(ctx, "egress", in.Text, in.Source)
	if err != nil {
		return f.errorResultEgress(err, in.Text)
	}
	action := f.action(v)
	meta := f.meta(v, action)
	switch action {
	case "block":
		if f.opts.Mode == "shadow" {
			metrics.Inc("agentflow_moderation_shadow_blocks")
			return EgressResult{Text: in.Text, Reason: "shadow:block", Meta: meta}
		}
		metrics.Inc("agentflow_moderation_blocks")
		return EgressResult{Drop: true, Fallback: f.opts.Fallback, Reason: "classifier:" + v.Label, Meta: meta}
	case "review":
		metrics.Inc("agentflow_moderation_reviews")
		return EgressResult{Text: in.Text, Reason: "review:" + v.Label, Meta: meta}
	default:
		return EgressResult{Text: in.Text, Reason: "allow:" + v.Label, Meta: meta}
	}
}

func (f *ClassifierFilter) classify(ctx context.Context, direction, text, source string) (Verdict, error) {
	if f.opts.Classifier == nil {
		return Verdict{}, errors.New("classifier is not configured")
	}
	if f.opts.MaxInputChars > 0 && len(text) > f.opts.MaxInputChars {
		return Verdict{}, ErrClassifierInputTooLarge
	}
	req := ClassifyRequest{
		Direction: direction,
		Text:      text,
		Source:    source,
		Agent:     f.opts.Agent,
	}
	if f.opts.Timeout <= 0 {
		return f.opts.Classifier.Classify(ctx, req)
	}
	callCtx, cancel := context.WithTimeout(ctx, f.opts.Timeout)
	defer cancel()
	return f.opts.Classifier.Classify(callCtx, req)
}

// action maps a verdict to the policy action. A score below the threshold is
// always allow, whatever the label; labels without a policy action are allow.
func (f *ClassifierFilter) action(v Verdict) string {
	if v.Score < f.opts.Threshold {
		return "allow"
	}
	switch strings.ToLower(f.opts.Categories[v.Label]) {
	case "block":
		return "block"
	case "review":
		return "review"
	default:
		return "allow"
	}
}

func (f *ClassifierFilter) meta(v Verdict, action string) *SafetyMeta {
	return &SafetyMeta{
		Classifier: f.opts.Name,
		Label:      v.Label,
		Action:     action,
		Score:      v.Score,
		Model:      v.Model,
		LatencyMS:  v.Latency.Milliseconds(),
		Cached:     v.Cached,
	}
}

// errorResult applies the error policy on ingress. A block drops the turn and
// skips it; pass preserves availability with a reason the trace can show.
func (f *ClassifierFilter) errorResult(err error, ingress bool, text string) IngressResult {
	action := f.errorAction(err)
	meta := &SafetyMeta{Classifier: f.opts.Name, Action: action, Model: "", Label: "error"}
	reason := f.errorReason(err)
	if action == "block" {
		return IngressResult{Drop: true, Skip: ingress, Refusal: f.opts.Refusal, Reason: reason, Meta: meta}
	}
	return IngressResult{Text: text, Reason: reason + ":pass", Meta: meta}
}

func (f *ClassifierFilter) errorResultEgress(err error, text string) EgressResult {
	action := f.errorAction(err)
	meta := &SafetyMeta{Classifier: f.opts.Name, Action: action, Label: "error"}
	reason := f.errorReason(err)
	if action == "block" {
		return EgressResult{Drop: true, Fallback: f.opts.Fallback, Reason: reason, Meta: meta}
	}
	return EgressResult{Text: text, Reason: reason + ":pass", Meta: meta}
}

// errorReason records the failure once, on whichever policy path it takes.
func (f *ClassifierFilter) errorReason(err error) string {
	metrics.Inc("agentflow_moderation_errors")
	if errors.Is(err, context.DeadlineExceeded) {
		metrics.Inc("agentflow_moderation_timeouts")
		return "classifier-timeout"
	}
	if errors.Is(err, ErrClassifierSaturated) {
		metrics.Inc("agentflow_moderation_saturated")
		return "classifier-saturated"
	}
	return "classifier-error"
}

func (f *ClassifierFilter) errorAction(err error) string {
	switch {
	case errors.Is(err, ErrClassifierSaturated):
		return f.opts.OnSaturated
	case errors.Is(err, ErrClassifierBudget):
		return f.opts.OnBudget
	default:
		return f.opts.OnError
	}
}
