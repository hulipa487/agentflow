package safety

import (
	"context"
	"testing"
)

func TestIngressPassThrough(t *testing.T) {
	d := New(None)
	res := d.Ingress(context.Background(), IngressInput{Message: "hello", Source: "channel"})
	if res.Drop {
		t.Fatal("expected pass-through")
	}
	if res.Text != "hello" {
		t.Fatalf("got %q, want hello", res.Text)
	}
}

func TestEgressPassThrough(t *testing.T) {
	d := New(None)
	res := d.Egress(context.Background(), EgressInput{Text: "hi there", Source: "agent"})
	if res.Drop {
		t.Fatal("expected pass-through")
	}
	if res.Text != "hi there" {
		t.Fatalf("got %q, want 'hi there'", res.Text)
	}
}

func TestSourceAttribution(t *testing.T) {
	f := SourceAttribution{}
	res := f.Ingress(context.Background(), IngressInput{Message: "> quoted text"})
	if res.Reason != "quoted" {
		t.Fatalf("expected quoted, got %q", res.Reason)
	}
	res = f.Ingress(context.Background(), IngressInput{Message: "direct text"})
	if res.Reason != "direct" {
		t.Fatalf("expected direct, got %q", res.Reason)
	}
}

func TestSignalGate(t *testing.T) {
	f := SignalGate{
		ExplicitPhrases: []string{"help me"},
		ImminentPhrases: []string{"right now"},
	}
	res := f.Ingress(context.Background(), IngressInput{Message: "help me please"})
	if res.Reason != "explicit" {
		t.Fatalf("expected explicit, got %q", res.Reason)
	}
	res = f.Ingress(context.Background(), IngressInput{Message: "I need this right now"})
	if res.Reason != "imminent" {
		t.Fatalf("expected imminent, got %q", res.Reason)
	}
	res = f.Ingress(context.Background(), IngressInput{Message: "hello"})
	if res.Reason != "none" {
		t.Fatalf("expected none, got %q", res.Reason)
	}
}

func TestAffectGuardDrops(t *testing.T) {
	f := AffectGuard{
		Patterns:       []string{"nothing matters"},
		EmpathyMarkers: []string{"i hear you"},
	}
	res := f.Egress(context.Background(), EgressInput{Text: "Nothing matters anyway"})
	if !res.Drop {
		t.Fatal("expected drop on affect-guard hit")
	}
	if res.Reason != "affect-guard-hit" {
		t.Fatalf("expected affect-guard-hit reason, got %q", res.Reason)
	}
}

func TestAffectGuardEmpathyPass(t *testing.T) {
	f := AffectGuard{
		Patterns:       []string{"nothing matters"},
		EmpathyMarkers: []string{"i hear you"},
	}
	res := f.Egress(context.Background(), EgressInput{Text: "I hear you, nothing matters is a hard feeling"})
	if res.Drop {
		t.Fatal("expected empathy passage to pass despite pattern hit")
	}
}

func TestDefaultProfileIngress(t *testing.T) {
	d := New(DefaultProfile())
	res := d.Ingress(context.Background(), IngressInput{Message: "help me", Source: "channel"})
	if res.Drop {
		t.Fatal("expected ingress to not drop")
	}
	if res.Text != "help me" {
		t.Fatalf("text changed: %q", res.Text)
	}
}

func TestDefaultProfileEgressDrop(t *testing.T) {
	d := New(DefaultProfile())
	res := d.Egress(context.Background(), EgressInput{Text: "Nothing matters", Source: "agent"})
	if !res.Drop {
		t.Fatal("expected affect-guard to drop nihilistic output")
	}
}

func TestDefaultProfileEgressPass(t *testing.T) {
	d := New(DefaultProfile())
	res := d.Egress(context.Background(), EgressInput{Text: "That's a great question!", Source: "agent"})
	if res.Drop {
		t.Fatal("expected normal output to pass")
	}
}

func TestChainStopsOnDrop(t *testing.T) {
	p := &Profile{
		Name: "test",
		Filters: []Filter{
			AffectGuard{Patterns: []string{"bad"}},
		},
	}
	d := New(p)
	res := d.Egress(context.Background(), EgressInput{Text: "this is bad"})
	if !res.Drop {
		t.Fatal("expected drop")
	}
}

func TestProfileName(t *testing.T) {
	d := New(DefaultProfile())
	if d.Profile() != "default" {
		t.Fatalf("expected default, got %q", d.Profile())
	}
	d2 := New(nil)
	if d2.Profile() != "none" {
		t.Fatalf("expected none, got %q", d2.Profile())
	}
}
