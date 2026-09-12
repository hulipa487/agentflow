package safety

import (
	"context"
	"strings"
)

// SourceAttribution classifies whether the message is direct user intent vs
// quoted/fictional/third-person content. Conservative bias: ambiguous → direct.
type SourceAttribution struct{}

func (SourceAttribution) Name() string { return "source-attribution" }

func (SourceAttribution) Ingress(_ context.Context, in IngressInput) IngressResult {
	// Detect common quotation markers. If present, the content is likely
	// quoted and should not be treated as direct intent — but we do not drop
	// it; we annotate via a prefix that downstream filters can read.
	if isQuoted(in.Message) {
		return IngressResult{Text: in.Message, Reason: "quoted"}
	}
	return IngressResult{Text: in.Message, Reason: "direct"}
}

func (SourceAttribution) Egress(_ context.Context, in EgressInput) EgressResult {
	return EgressResult{Text: in.Text}
}

func isQuoted(s string) bool {
	return strings.HasPrefix(s, ">") ||
		strings.HasPrefix(s, "\"") ||
		strings.Contains(s, "said:") ||
		strings.Contains(s, "wrote:")
}

// SignalGate assesses signal strength (none|vague|explicit|imminent).
// Phase 4c uses a conservative phrase-based classifier; a model-assisted
// classifier can replace this behind the same interface.
type SignalGate struct {
	ExplicitPhrases []string
	ImminentPhrases []string
}

func (SignalGate) Name() string { return "signal-gate" }

func (s SignalGate) Ingress(_ context.Context, in IngressInput) IngressResult {
	for _, phrase := range s.ImminentPhrases {
		if strings.Contains(strings.ToLower(in.Message), strings.ToLower(phrase)) {
			return IngressResult{Text: in.Message, Reason: "imminent"}
		}
	}
	for _, phrase := range s.ExplicitPhrases {
		if strings.Contains(strings.ToLower(in.Message), strings.ToLower(phrase)) {
			return IngressResult{Text: in.Message, Reason: "explicit"}
		}
	}
	return IngressResult{Text: in.Message, Reason: "none"}
}

func (SignalGate) Egress(_ context.Context, in EgressInput) EgressResult {
	return EgressResult{Text: in.Text}
}

// SteadyDirective injects a stabilization directive at the front of the reply
// system prompt when the signal is non-none. In Phase 4c this is a no-op on
// egress (the directive would be injected at prompt-assembly time, which is
// loop domain). It does not mutate user-facing text.
type SteadyDirective struct{}

func (SteadyDirective) Name() string { return "steady-directive" }

func (SteadyDirective) Ingress(_ context.Context, in IngressInput) IngressResult {
	return IngressResult{Text: in.Message}
}

func (SteadyDirective) Egress(_ context.Context, in EgressInput) EgressResult {
	return EgressResult{Text: in.Text}
}

// SupportOffer enforces a per-session delivery cap on resource offers. It does
// not autonomously contact external services. In Phase 4c it's a stateful
// ingress filter that annotates the turn if an offer has already been made.
type SupportOffer struct {
	PerSessionLimit int
	offered         map[string]int // keyed by session
}

func NewSupportOffer(limit int) *SupportOffer {
	return &SupportOffer{PerSessionLimit: limit, offered: map[string]int{}}
}

func (s *SupportOffer) Name() string { return "support-offer" }

func (s *SupportOffer) Ingress(_ context.Context, in IngressInput) IngressResult {
	// This filter is a placeholder for the full implementation which would
	// track per-session offer counts and annotate the turn context. Phase 4c
	// keeps it stateless-deterministic; the full per-session state requires
	// the session ID which the dispatcher passes via source, not message.
	return IngressResult{Text: in.Message}
}

func (s *SupportOffer) Egress(_ context.Context, in EgressInput) EgressResult {
	return EgressResult{Text: in.Text}
}

// AffectGuard blocks nihilistic/world-weary output in the agent's own voice.
// It pattern-matches against a wordlist and drops on hit. Empathy passages
// acknowledging the user's feelings pass through (framing markers).
type AffectGuard struct {
	Patterns       []string
	EmpathyMarkers []string
}

func (AffectGuard) Name() string { return "affect-guard" }

func (g AffectGuard) Ingress(_ context.Context, in IngressInput) IngressResult {
	return IngressResult{Text: in.Message}
}

func (g AffectGuard) Egress(_ context.Context, in EgressInput) EgressResult {
	lower := strings.ToLower(in.Text)
	for _, marker := range g.EmpathyMarkers {
		if strings.Contains(lower, strings.ToLower(marker)) {
			return EgressResult{Text: in.Text, Reason: "empathy-pass"}
		}
	}
	for _, pat := range g.Patterns {
		if strings.Contains(lower, strings.ToLower(pat)) {
			return EgressResult{Drop: true, Reason: "affect-guard-hit"}
		}
	}
	return EgressResult{Text: in.Text}
}

// DefaultProfile returns a baseline profile with all five filters configured
// conservatively. Deployments can override via YAML.
func DefaultProfile() *Profile {
	return &Profile{
		Name: "default",
		Filters: []Filter{
			SourceAttribution{},
			SignalGate{
				ExplicitPhrases: []string{"i want to", "i need to", "help me"},
				ImminentPhrases: []string{"right now", "immediately", "can't wait"},
			},
			SteadyDirective{},
			NewSupportOffer(1),
			AffectGuard{
				Patterns: []string{
					"nothing matters",
					"what's the point",
					"give up",
				},
				EmpathyMarkers: []string{
					"i hear you",
					"that sounds",
					"you're not alone",
				},
			},
		},
	}
}
