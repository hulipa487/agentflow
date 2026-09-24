package safety

import (
	"context"
	"testing"
)

// TestIngressCarriesTheChainsClassification: only AffectGuard ever sets Drop, so
// for the other four filters a Reason is the only output they produce — and it
// was discarded on the success path, which made the whole default chain
// unobservable. Carrying it changes no text and no decision.
func TestIngressCarriesTheChainsClassification(t *testing.T) {
	d := New(DefaultProfile())
	ctx := context.Background()

	// SourceAttribution classifies this "quoted" and SignalGate matches nothing,
	// so the last non-empty reason is signal-gate's "none".
	res := d.Ingress(ctx, IngressInput{Message: "> quoted text"})
	if res.Drop {
		t.Fatal("the default chain must not drop a quoted message")
	}
	if res.Reason != "none" {
		t.Fatalf("reason = %q; want signal-gate's %q — the classification was dropped", res.Reason, "none")
	}
	if res.Text != "> quoted text" {
		t.Fatalf("text = %q; the chain must not rewrite this message", res.Text)
	}

	res = d.Ingress(ctx, IngressInput{Message: "help me right now"})
	if res.Reason != "imminent" {
		t.Fatalf("reason = %q; want %q", res.Reason, "imminent")
	}
}

// TestEgressCarriesTheReasonToo covers the other direction.
func TestEgressCarriesTheReasonToo(t *testing.T) {
	d := New(DefaultProfile())
	res := d.Egress(context.Background(), EgressInput{Text: "I hear you, that sounds hard"})
	if res.Drop {
		t.Fatal("an empathy passage must pass")
	}
	if res.Reason != "empathy-pass" {
		t.Fatalf("reason = %q; want %q", res.Reason, "empathy-pass")
	}
}

// TestProfileOfSelectsByName: a profiles.safety entry names baseline filters,
// and the selected ones keep the baseline's configuration — not a zero value
// with no phrases.
func TestProfileOfSelectsByName(t *testing.T) {
	p := ProfileOf("gentle", []string{"signal-gate", "affect-guard"})
	if len(p.Filters) != 2 {
		t.Fatalf("got %d filters; want 2", len(p.Filters))
	}
	if p.Filters[0].Name() != "signal-gate" || p.Filters[1].Name() != "affect-guard" {
		t.Fatalf("selection did not follow the list order: %s, %s", p.Filters[0].Name(), p.Filters[1].Name())
	}
	sg, ok := p.Filters[0].(SignalGate)
	if !ok {
		t.Fatalf("signal-gate came back as %T", p.Filters[0])
	}
	if len(sg.ExplicitPhrases) == 0 || len(sg.ImminentPhrases) == 0 {
		t.Fatal("the baseline's phrases were lost in selection")
	}

	// An empty list is an explicit empty chain: the dispatcher runs and decides
	// nothing, which is not the same as safety:none bypassing it.
	empty := ProfileOf("none-but-running", nil)
	if len(empty.Filters) != 0 {
		t.Fatalf("an empty list produced %d filters", len(empty.Filters))
	}

	// An unknown name is skipped rather than panicking. config.validate rejects
	// it at load, so this only guards a hand-built profile.
	if got := ProfileOf("x", []string{"nope"}); len(got.Filters) != 0 {
		t.Fatalf("an unknown filter name produced %d filters", len(got.Filters))
	}
}

// TestFilterNamesMatchesDefaultProfile keeps the validation list and the
// baseline in step — a name added to one and not the other would make a
// legitimate filter un-selectable.
func TestFilterNamesMatchesDefaultProfile(t *testing.T) {
	base := DefaultProfile()
	names := FilterNames()
	if len(names) != len(base.Filters) {
		t.Fatalf("FilterNames has %d entries; the baseline has %d filters", len(names), len(base.Filters))
	}
	for i, f := range base.Filters {
		if names[i] != f.Name() {
			t.Errorf("FilterNames[%d] = %q; baseline has %q", i, names[i], f.Name())
		}
	}
}
