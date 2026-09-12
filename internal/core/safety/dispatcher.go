// Package safety is the core-owned safety dispatcher. It runs for every
// inbound message before the loop sees it and for every outbound reply before
// the gateway delivers it. Replacing the loop handler can never uninstall it.
//
// The dispatcher delegates to a configured SafetyProfile, which selects
// deterministic builtin filters. The filters may annotate a per-turn context
// or mutate/drop text. Filters never autonomously contact external services.
package safety

import (
	"context"
)

// IngressInput is what the dispatcher receives before the loop processes a turn.
type IngressInput struct {
	Message string // the user/agent/timer text
	Source  string // channel | agent | scheduler | system
}

// IngressResult is the dispatcher's decision. If Drop is true, the message is
// discarded and the loop never sees it. Otherwise Text replaces the original.
type IngressResult struct {
	Text   string
	Drop   bool
	Reason string // auditable reason code, never logged with raw content
}

// EgressInput is what the dispatcher receives before the gateway sends a reply.
type EgressInput struct {
	Text   string
	Source string
}

// EgressResult is the dispatcher's decision. If Drop is true, the reply is
// suppressed (nothing is sent). Otherwise Text replaces the original.
type EgressResult struct {
	Text   string
	Drop   bool
	Reason string
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
func (d *Dispatcher) Ingress(ctx context.Context, in IngressInput) IngressResult {
	if d.profile == None {
		return IngressResult{Text: in.Message}
	}
	text := in.Message
	for _, f := range d.profile.Filters {
		res := f.Ingress(ctx, IngressInput{Message: text, Source: in.Source})
		if res.Drop {
			return res
		}
		text = res.Text
	}
	return IngressResult{Text: text}
}

// Egress runs all egress filters in order. If any filter drops, the chain stops.
func (d *Dispatcher) Egress(ctx context.Context, in EgressInput) EgressResult {
	if d.profile == None {
		return EgressResult{Text: in.Text}
	}
	text := in.Text
	for _, f := range d.profile.Filters {
		res := f.Egress(ctx, EgressInput{Text: text, Source: in.Source})
		if res.Drop {
			return res
		}
		text = res.Text
	}
	return EgressResult{Text: text}
}

// Profile returns the active profile name.
func (d *Dispatcher) Profile() string {
	if d.profile == nil {
		return "none"
	}
	return d.profile.Name
}
