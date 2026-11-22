package safety

import (
	"context"
	"strings"
	"sync"
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

const (
	defaultSteadyDirective = "Respond calmly, concisely, and supportively; avoid amplifying distress, urgency, or hopelessness."
	defaultSupportOffer    = "If you are going through a difficult moment, consider reaching out to someone you trust or a local support helpline."
	maxTrackedSessions     = 4096
)

// SteadyDirective injects a core-owned stabilization system directive for the
// turn when earlier ingress classification indicates an active signal on direct
// (non-quoted) traffic. It does not mutate user-facing text directly.
type SteadyDirective struct {
	Directive      string
	TriggerSignals []string
}

func (SteadyDirective) Name() string { return "steady-directive" }

func (d SteadyDirective) Ingress(_ context.Context, in IngressInput) IngressResult {
	if strings.EqualFold(in.Annotations["source-attribution"], "quoted") {
		return IngressResult{Text: in.Message}
	}
	signal := in.Annotations["signal-gate"]
	if signal == "" {
		return IngressResult{Text: in.Message}
	}
	triggers := d.TriggerSignals
	if len(triggers) == 0 {
		triggers = []string{"explicit", "imminent"}
	}
	matched := false
	for _, want := range triggers {
		if strings.EqualFold(signal, want) {
			matched = true
			break
		}
	}
	if !matched {
		return IngressResult{Text: in.Message}
	}
	dir := strings.TrimSpace(d.Directive)
	if dir == "" {
		dir = defaultSteadyDirective
	}
	return IngressResult{
		Text:        in.Message,
		Reason:      "steady-directive-applied",
		Directives:  []string{dir},
		Annotations: map[string]string{"steady-directive": "applied"},
	}
}

func (SteadyDirective) Egress(_ context.Context, in EgressInput) EgressResult {
	return EgressResult{Text: in.Text}
}

// SupportOffer enforces a per-session delivery cap on support resource offers.
// When imminent distress signal is detected on direct (non-quoted) ingress and
// the session has remaining quota, Ingress marks the turn eligible and Egress
// appends a concise support note once per session. It does not autonomously
// contact external services.
type SupportOffer struct {
	PerSessionLimit int
	OfferText       string

	mu      sync.Mutex
	offered map[string]int  // keyed by session
	pending map[string]bool // active turn eligibility keyed by session
}

func NewSupportOffer(limit int) *SupportOffer {
	return &SupportOffer{
		PerSessionLimit: limit,
		OfferText:       defaultSupportOffer,
		offered:         map[string]int{},
		pending:         map[string]bool{},
	}
}

func (s *SupportOffer) Name() string { return "support-offer" }

func (s *SupportOffer) sessionKey(sessionID, source string) string {
	if sessionID != "" {
		return sessionID
	}
	if source != "" {
		return source
	}
	return "default"
}

func (s *SupportOffer) ensureMapsLocked() {
	if s.offered == nil {
		s.offered = map[string]int{}
	}
	if s.pending == nil {
		s.pending = map[string]bool{}
	}
}

func (s *SupportOffer) Ingress(_ context.Context, in IngressInput) IngressResult {
	key := s.sessionKey(in.SessionID, in.Source)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMapsLocked()
	delete(s.pending, key)

	if s.PerSessionLimit <= 0 ||
		strings.EqualFold(in.Annotations["source-attribution"], "quoted") ||
		!strings.EqualFold(in.Annotations["signal-gate"], "imminent") {
		return IngressResult{Text: in.Message}
	}
	if len(s.offered) > maxTrackedSessions {
		clear(s.offered)
	}
	if s.offered[key] >= s.PerSessionLimit {
		return IngressResult{
			Text:        in.Message,
			Reason:      "support-offer-capped",
			Annotations: map[string]string{"support-offer": "capped"},
		}
	}
	if len(s.pending) > maxTrackedSessions {
		clear(s.pending)
	}
	s.pending[key] = true
	return IngressResult{
		Text:        in.Message,
		Reason:      "support-offer-eligible",
		Annotations: map[string]string{"support-offer": "eligible"},
	}
}

func (s *SupportOffer) Egress(_ context.Context, in EgressInput) EgressResult {
	key := s.sessionKey(in.SessionID, in.Source)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMapsLocked()
	wasPending := s.pending[key]
	delete(s.pending, key)

	eligible := wasPending || strings.EqualFold(in.Annotations["support-offer"], "eligible")
	if !eligible || s.PerSessionLimit <= 0 || in.Text == "" || s.offered[key] >= s.PerSessionLimit {
		return EgressResult{Text: in.Text}
	}
	offer := strings.TrimSpace(s.OfferText)
	if offer == "" {
		offer = defaultSupportOffer
	}
	if strings.Contains(strings.ToLower(in.Text), strings.ToLower(offer)) {
		s.offered[key]++
		return EgressResult{
			Text:        in.Text,
			Reason:      "support-offer-appended",
			Annotations: map[string]string{"support-offer": "appended"},
		}
	}
	s.offered[key]++
	return EgressResult{
		Text:        strings.TrimRight(in.Text, "\n") + "\n\n" + offer,
		Reason:      "support-offer-appended",
		Annotations: map[string]string{"support-offer": "appended"},
	}
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

// DefaultProfile returns a baseline profile with all five builtin filters
// configured conservatively. A deployment can select a subset by name in
// profiles.safety and can add model-backed classifier:<name> filters from
// safety.classifiers; the builtin phrase lists themselves stay compiled in.
func DefaultProfile() *Profile {
	return &Profile{
		Name: "default",
		Filters: []Filter{
			SourceAttribution{},
			SignalGate{
				ExplicitPhrases: []string{"i want to", "i need to", "help me"},
				ImminentPhrases: []string{"right now", "immediately", "can't wait"},
			},
			SteadyDirective{
				Directive:      defaultSteadyDirective,
				TriggerSignals: []string{"explicit", "imminent"},
			},
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
