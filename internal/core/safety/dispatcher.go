// Package safety is the core-owned safety dispatcher. It runs for every
// inbound message before the loop sees it and for every outbound reply before
// the gateway delivers it. Replacing the loop handler can never uninstall it.
//
// The dispatcher delegates to a configured SafetyProfile, which selects
// deterministic builtin filters. The filters may annotate a per-turn context
// or mutate/drop text. Builtin filters never contact external services; a
// classifier filter is different by construction: it calls the model behind the
// injected Classifier interface, and its timeout, cache and failure policy are
// part of ClassifierOptions.
package safety

import (
	"context"
)

// IngressInput is what the dispatcher receives before the loop processes a turn.
type IngressInput struct {
	Message     string            // the user/agent/timer text
	Source      string            // channel | agent | scheduler | system
	SessionID   string            // stable session identifier when known
	Annotations map[string]string // per-chain filter outputs keyed by filter name
}

// SafetyMeta describes a classifier or filter verdict for the trace and audit
// journal. It never carries message text.
type SafetyMeta struct {
	Classifier    string  `json:"classifier,omitempty"`
	Label         string  `json:"label,omitempty"`
	Action        string  `json:"action,omitempty"`
	Score         float64 `json:"score,omitempty"`
	Model         string  `json:"model,omitempty"`
	RedirectModel string  `json:"redirect_model,omitempty"`
	LatencyMS     int64   `json:"latency_ms,omitempty"`
	Cached        bool    `json:"cached,omitempty"`
}

// IngressResult is the dispatcher's decision. If Drop is true, the message is
// discarded and the loop never sees it. Skip additionally ends the turn
// without handing the loop an empty message; Refusal, when set, is a
// core-authored reply delivered instead. Otherwise Text replaces the original.
type IngressResult struct {
	Text          string
	Drop          bool
	Skip          bool              // drop the turn rather than passing an empty message
	Refusal       string            // optional core-authored reply for a dropped turn
	Reason        string            // auditable reason code, never logged with raw content
	Meta          *SafetyMeta
	Annotations   map[string]string // per-turn filter annotations propagated down the chain
	Directives    []string          // core-authored system directives prepended to turn LLM calls
	ModelOverride string            // core-enforced model override for this turn
}

// EgressInput is what the dispatcher receives before the gateway sends a reply.
type EgressInput struct {
	Text        string
	Source      string
	SessionID   string
	Annotations map[string]string
}

// EgressResult is the dispatcher's decision. If Drop is true, the reply is
// suppressed (nothing is sent). Fallback, when set, is a core-authored reply
// delivered instead of the blocked text. Otherwise Text replaces the original.
type EgressResult struct {
	Text        string
	Drop        bool
	Fallback    string
	Reason      string
	Meta        *SafetyMeta
	Annotations map[string]string
}

// Filter is a single safety filter. Filters are chained in profile order.
type Filter interface {
	Ingress(ctx context.Context, in IngressInput) IngressResult
	Egress(ctx context.Context, in EgressInput) EgressResult
	Name() string
}

// Profile is a resolved safety profile: an ordered chain of filters.
type Profile struct {
	Name    string
	Filters []Filter
}

// None is the explicit opt-out profile. It passes everything through.
var None = &Profile{Name: "none"}

// Dispatcher runs the ingress and egress chains.
type Dispatcher struct {
	profile *Profile
}

// New creates a dispatcher bound to a profile. A nil profile means safety:none.
func New(profile *Profile) *Dispatcher {
	if profile == nil {
		return &Dispatcher{profile: None}
	}
	return &Dispatcher{profile: profile}
}

// Ingress runs all ingress filters in order. If any filter drops, the chain
// stops and the drop is returned immediately.
//
// The last non-empty Reason is carried forward on the success path. It used to
// be discarded there, which made most of the default chain unobservable: only
// AffectGuard ever sets Drop, so a classification like "quoted" or "imminent"
// from the filters that only advise never reached the caller or the journal.
// Carrying it changes no text and no decision — it only stops the chain's
// verdicts from being thrown away.
func (d *Dispatcher) Ingress(ctx context.Context, in IngressInput) IngressResult {
	if d.profile == None {
		return IngressResult{Text: in.Message}
	}
	text := in.Message
	reason := ""
	modelOverride := ""
	var meta *SafetyMeta
	annotations := copyAnnotations(in.Annotations)
	var directives []string
	for _, f := range d.profile.Filters {
		res := f.Ingress(ctx, IngressInput{
			Message:     text,
			Source:      in.Source,
			SessionID:   in.SessionID,
			Annotations: copyAnnotations(annotations),
		})
		if res.Drop || res.Skip {
			if len(res.Annotations) == 0 && len(annotations) > 0 {
				res.Annotations = annotations
			}
			return res
		}
		text = res.Text
		if res.Reason != "" {
			reason = res.Reason
			annotations[f.Name()] = res.Reason
		}
		for k, v := range res.Annotations {
			annotations[k] = v
		}
		if len(res.Directives) > 0 {
			directives = append(directives, res.Directives...)
		}
		if res.ModelOverride != "" {
			modelOverride = res.ModelOverride
		}
		if res.Meta != nil {
			m := *res.Meta
			meta = &m
		}
	}
	if reason == "" && modelOverride != "" {
		reason = "redirect:" + modelOverride
	}
	if modelOverride != "" {
		if meta == nil {
			meta = &SafetyMeta{Action: "redirect", RedirectModel: modelOverride}
		} else if meta.RedirectModel == "" {
			meta.RedirectModel = modelOverride
		}
	}
	return IngressResult{
		Text:          text,
		Reason:        reason,
		Meta:          meta,
		Annotations:   annotations,
		Directives:    directives,
		ModelOverride: modelOverride,
	}
}

// Egress runs all egress filters in order. If any filter drops, the chain stops.
// The last non-empty Reason is carried forward, as in Ingress.
func (d *Dispatcher) Egress(ctx context.Context, in EgressInput) EgressResult {
	if d.profile == None {
		return EgressResult{Text: in.Text}
	}
	text := in.Text
	reason := ""
	var meta *SafetyMeta
	annotations := copyAnnotations(in.Annotations)
	for _, f := range d.profile.Filters {
		res := f.Egress(ctx, EgressInput{
			Text:        text,
			Source:      in.Source,
			SessionID:   in.SessionID,
			Annotations: copyAnnotations(annotations),
		})
		if res.Drop {
			if len(res.Annotations) == 0 && len(annotations) > 0 {
				res.Annotations = annotations
			}
			return res
		}
		text = res.Text
		if res.Reason != "" {
			reason = res.Reason
			annotations[f.Name()] = res.Reason
		}
		for k, v := range res.Annotations {
			annotations[k] = v
		}
		if res.Meta != nil {
			meta = res.Meta
		}
	}
	return EgressResult{Text: text, Reason: reason, Meta: meta, Annotations: annotations}
}

func copyAnnotations(src map[string]string) map[string]string {
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// Profile returns the active profile name.
func (d *Dispatcher) Profile() string {
	if d.profile == nil {
		return "none"
	}
	return d.profile.Name
}

// CanDropEgress reports whether an outbound reply can be vetoed after it was
// produced. It is what decides whether a stream may be relayed live: a
// channel that streams must not have sent the first half of an answer before
// the filter sees the second half. Any filter in a non-empty profile can, in
// principle, drop — the conservative answer is the correct one here.
func (d *Dispatcher) CanDropEgress() bool {
	return d.profile != nil && d.profile != None && len(d.profile.Filters) > 0
}
