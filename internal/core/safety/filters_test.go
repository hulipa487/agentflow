package safety

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func TestSteadyDirective(t *testing.T) {
	ctx := context.Background()
	f := SteadyDirective{}

	for _, tc := range []struct {
		name      string
		in        IngressInput
		wantDir   bool
		wantReas  string
		wantAnnot string
	}{
		{
			name:     "direct explicit triggers directive",
			in:       IngressInput{Message: "help me", Annotations: map[string]string{"source-attribution": "direct", "signal-gate": "explicit"}},
			wantDir:  true,
			wantReas: "steady-directive-applied",
		},
		{
			name:     "direct imminent triggers directive",
			in:       IngressInput{Message: "need help right now", Annotations: map[string]string{"source-attribution": "direct", "signal-gate": "imminent"}},
			wantDir:  true,
			wantReas: "steady-directive-applied",
		},
		{
			name:     "quoted content does not trigger directive",
			in:       IngressInput{Message: `> "help me right now"`, Annotations: map[string]string{"source-attribution": "quoted", "signal-gate": "imminent"}},
			wantDir:  false,
			wantReas: "",
		},
		{
			name:     "no signal does not trigger directive",
			in:       IngressInput{Message: "what is Go?", Annotations: map[string]string{"source-attribution": "direct", "signal-gate": "none"}},
			wantDir:  false,
			wantReas: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := f.Ingress(ctx, tc.in)
			if tc.wantDir && len(res.Directives) == 0 {
				t.Fatalf("expected directive, got none (%+v)", res)
			}
			if !tc.wantDir && len(res.Directives) > 0 {
				t.Fatalf("unexpected directive %v", res.Directives)
			}
			if res.Reason != tc.wantReas {
				t.Fatalf("reason = %q, want %q", res.Reason, tc.wantReas)
			}
			if res.Text != tc.in.Message {
				t.Fatalf("message mutated: %q", res.Text)
			}
		})
	}
}

func TestSupportOfferPerSessionCap(t *testing.T) {
	ctx := context.Background()
	s := NewSupportOffer(1)

	imminent := IngressInput{
		Message:   "help me right now",
		SessionID: "sess-a",
		Annotations: map[string]string{
			"source-attribution": "direct",
			"signal-gate":        "imminent",
		},
	}

	// Turn 1 in sess-a: eligible on ingress, appends on egress.
	in1 := s.Ingress(ctx, imminent)
	if in1.Reason != "support-offer-eligible" {
		t.Fatalf("turn 1 ingress reason = %q, want support-offer-eligible", in1.Reason)
	}
	eg1 := s.Egress(ctx, EgressInput{Text: "Here is what I can do.", SessionID: "sess-a"})
	if eg1.Reason != "support-offer-appended" || !strings.Contains(eg1.Text, defaultSupportOffer) {
		t.Fatalf("turn 1 egress = %+v, want appended support offer", eg1)
	}

	// Turn 2 in sess-a: capped at PerSessionLimit = 1.
	in2 := s.Ingress(ctx, imminent)
	if in2.Reason != "support-offer-capped" {
		t.Fatalf("turn 2 ingress reason = %q, want support-offer-capped", in2.Reason)
	}
	eg2 := s.Egress(ctx, EgressInput{Text: "Second reply.", SessionID: "sess-a"})
	if eg2.Text != "Second reply." || eg2.Reason != "" {
		t.Fatalf("turn 2 egress should not append again, got %+v", eg2)
	}

	// Independent session sess-b still has its own quota.
	other := imminent
	other.SessionID = "sess-b"
	inB := s.Ingress(ctx, other)
	if inB.Reason != "support-offer-eligible" {
		t.Fatalf("sess-b ingress reason = %q, want support-offer-eligible", inB.Reason)
	}
	egB := s.Egress(ctx, EgressInput{Text: "Reply for B.", SessionID: "sess-b"})
	if !strings.Contains(egB.Text, defaultSupportOffer) {
		t.Fatalf("sess-b egress did not append support offer: %q", egB.Text)
	}
}

func TestSupportOfferQuotedSkippedAndConcurrentSafe(t *testing.T) {
	ctx := context.Background()
	s := NewSupportOffer(2)

	quoted := s.Ingress(ctx, IngressInput{
		Message:   `> right now`,
		SessionID: "sess-q",
		Annotations: map[string]string{
			"source-attribution": "quoted",
			"signal-gate":        "imminent",
		},
	})
	if quoted.Reason != "" {
		t.Fatalf("expected quoted imminent message not to trigger support offer, got %q", quoted.Reason)
	}
	eg := s.Egress(ctx, EgressInput{Text: "Quoted analysis.", SessionID: "sess-q"})
	if eg.Text != "Quoted analysis." {
		t.Fatalf("unexpected mutation on quoted turn: %q", eg.Text)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			sid := "conc-" + string(rune('a'+id))
			_ = s.Ingress(ctx, IngressInput{
				Message:   "right now",
				SessionID: sid,
				Annotations: map[string]string{
					"source-attribution": "direct",
					"signal-gate":        "imminent",
				},
			})
			_ = s.Egress(ctx, EgressInput{Text: "reply", SessionID: sid})
		}(i)
	}
	wg.Wait()
}

func TestDefaultProfileChainDirectivesAndSupportOffer(t *testing.T) {
	ctx := context.Background()
	d := New(DefaultProfile())

	inRes := d.Ingress(ctx, IngressInput{
		Message:   "I need help right now",
		Source:    "channel",
		SessionID: "sess-chain",
	})
	if inRes.Drop || inRes.Skip {
		t.Fatalf("expected non-dropping ingress, got %+v", inRes)
	}
	if len(inRes.Directives) != 1 || !strings.Contains(inRes.Directives[0], "calmly") {
		t.Fatalf("expected SteadyDirective output, got %v", inRes.Directives)
	}
	if inRes.Reason != "support-offer-eligible" {
		t.Fatalf("expected support-offer-eligible last reason, got %q", inRes.Reason)
	}

	egRes := d.Egress(ctx, EgressInput{Text: "Take a slow breath with me.", Source: "agent", SessionID: "sess-chain"})
	if egRes.Drop || !strings.Contains(egRes.Text, defaultSupportOffer) {
		t.Fatalf("expected egress support offer appended, got %+v", egRes)
	}
}
