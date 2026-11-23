package safety

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeClassifier struct {
	verdict Verdict
	err     error
	calls   int
}

func (f *fakeClassifier) Classify(_ context.Context, _ ClassifyRequest) (Verdict, error) {
	f.calls++
	return f.verdict, f.err
}

func testFilter(c Classifier) *ClassifierFilter {
	return NewClassifierFilter(ClassifierOptions{
		Name:          "moderation",
		Agent:         "assistant",
		Classifier:    c,
		Ingress:       true,
		Egress:        true,
		Threshold:     0.7,
		Categories:    map[string]string{"safe": "allow", "violence": "block", "harassment": "review"},
		Mode:          "enforce",
		Timeout:       time.Second,
		OnError:       "block",
		OnSaturated:   "block",
		OnBudget:      "block",
		MaxInputChars: 1000,
		Refusal:       "blocked",
		Fallback:      "fallback",
	})
}

func TestClassifierBlocksIngress(t *testing.T) {
	f := testFilter(&fakeClassifier{verdict: Verdict{Label: "violence", Score: 0.95}})
	res := f.Ingress(context.Background(), IngressInput{Message: "hurt them", Source: "channel"})
	if !res.Drop || !res.Skip {
		t.Fatalf("got %+v; want a drop that skips the turn", res)
	}
	if res.Refusal != "blocked" || res.Reason != "classifier:violence" {
		t.Fatalf("got refusal %q reason %q", res.Refusal, res.Reason)
	}
	if res.Meta == nil || res.Meta.Action != "block" {
		t.Fatalf("missing block metadata: %+v", res.Meta)
	}
}

func TestClassifierAllowsBenign(t *testing.T) {
	f := testFilter(&fakeClassifier{verdict: Verdict{Label: "safe", Score: 0.99}})
	res := f.Ingress(context.Background(), IngressInput{Message: "hello", Source: "channel"})
	if res.Drop || res.Text != "hello" {
		t.Fatalf("got %+v; want pass-through", res)
	}
	if res.Meta == nil || res.Meta.Action != "allow" {
		t.Fatalf("missing allow metadata: %+v", res.Meta)
	}
}

func TestClassifierThresholdOverridesLabel(t *testing.T) {
	f := testFilter(&fakeClassifier{verdict: Verdict{Label: "violence", Score: 0.1}})
	res := f.Ingress(context.Background(), IngressInput{Message: "maybe", Source: "channel"})
	if res.Drop {
		t.Fatalf("low-confidence violation must not block: %+v", res)
	}
}

func TestClassifierReviewPassesWithReason(t *testing.T) {
	f := testFilter(&fakeClassifier{verdict: Verdict{Label: "harassment", Score: 0.9}})
	res := f.Ingress(context.Background(), IngressInput{Message: "rude", Source: "channel"})
	if res.Drop || res.Reason != "review:harassment" {
		t.Fatalf("got %+v; want review pass", res)
	}
}

func TestClassifierErrorPolicy(t *testing.T) {
	blocking := testFilter(&fakeClassifier{err: errors.New("provider down")})
	res := blocking.Ingress(context.Background(), IngressInput{Message: "hello", Source: "channel"})
	if !res.Drop || !res.Skip {
		t.Fatalf("on_error block must drop: %+v", res)
	}

	passing := testFilter(&fakeClassifier{err: errors.New("provider down")})
	passing.opts.OnError = "pass"
	res = passing.Ingress(context.Background(), IngressInput{Message: "hello", Source: "channel"})
	if res.Drop || res.Text != "hello" {
		t.Fatalf("on_error pass must preserve the text: %+v", res)
	}
}

func TestClassifierSourceFilterSkips(t *testing.T) {
	fc := &fakeClassifier{verdict: Verdict{Label: "violence", Score: 0.99}}
	f := testFilter(fc)
	f.opts.Sources = []string{"channel"}
	f.sources = map[string]bool{"channel": true}
	res := f.Ingress(context.Background(), IngressInput{Message: "timer", Source: "scheduler"})
	if res.Drop || fc.calls != 0 {
		t.Fatalf("scheduler source must skip the classifier; calls=%d res=%+v", fc.calls, res)
	}
}

func TestClassifierShadowNeverBlocks(t *testing.T) {
	f := testFilter(&fakeClassifier{verdict: Verdict{Label: "violence", Score: 0.99}})
	f.opts.Mode = "shadow"
	res := f.Ingress(context.Background(), IngressInput{Message: "hurt them", Source: "channel"})
	if res.Drop {
		t.Fatalf("shadow mode must not drop: %+v", res)
	}
	if res.Reason != "shadow:block" || res.Meta == nil || res.Meta.Action != "block" {
		t.Fatalf("shadow verdict not recorded: %+v", res)
	}
}

func TestClassifierEgressFallback(t *testing.T) {
	f := testFilter(&fakeClassifier{verdict: Verdict{Label: "violence", Score: 0.99}})
	res := f.Egress(context.Background(), EgressInput{Text: "hurt them", Source: "agent"})
	if !res.Drop || res.Fallback != "fallback" {
		t.Fatalf("got %+v; want egress drop with fallback", res)
	}
}

func TestClassifierEgressErrorPassPreservesText(t *testing.T) {
	f := testFilter(&fakeClassifier{err: errors.New("provider down")})
	f.opts.OnError = "pass"
	res := f.Egress(context.Background(), EgressInput{Text: "hello", Source: "agent"})
	if res.Drop || res.Text != "hello" {
		t.Fatalf("on_error pass must preserve the reply text: %+v", res)
	}
}

func TestClassifierOversizedInputUsesErrorPolicy(t *testing.T) {
	fc := &fakeClassifier{verdict: Verdict{Label: "safe", Score: 0.99}}
	f := testFilter(fc)
	res := f.Ingress(context.Background(), IngressInput{Message: strings.Repeat("x", 1001), Source: "channel"})
	if !res.Drop || fc.calls != 0 {
		t.Fatalf("oversized input must fail closed without a model call; calls=%d res=%+v", fc.calls, res)
	}
}

func TestCategoryActionHelpersPreserveRedirectCase(t *testing.T) {
	if got := NormalizeCategoryAction(" REDIRECT:DeepSeek-Safe "); got != "redirect:DeepSeek-Safe" {
		t.Fatalf("NormalizeCategoryAction = %q, want redirect:DeepSeek-Safe", got)
	}
	if got := NormalizeCategoryAction("  BLOCK "); got != "block" {
		t.Fatalf("NormalizeCategoryAction(BLOCK) = %q, want block", got)
	}
	act, target := ParseCategoryAction("REDIRECT: DeepSeek-Safe ")
	if act != "redirect" || target != "DeepSeek-Safe" {
		t.Fatalf("ParseCategoryAction = (%q, %q), want (redirect, DeepSeek-Safe)", act, target)
	}
	if act, target := ParseCategoryAction("redirect: "); act != "" || target != "" {
		t.Fatalf("empty redirect target must be rejected, got (%q, %q)", act, target)
	}
}

func TestClassifierRedirectIngressShadowAndEgress(t *testing.T) {
	newRedirectFilter := func(mode string, c Classifier) *ClassifierFilter {
		return NewClassifierFilter(ClassifierOptions{
			Name:       "routing",
			Agent:      "assistant",
			Classifier: c,
			Ingress:    true,
			Egress:     true,
			Threshold:  0.7,
			Categories: map[string]string{
				"safe":    "allow",
				"complex": "redirect:Strong-Model",
			},
			Mode:        mode,
			Timeout:     time.Second,
			OnError:     "block",
			OnSaturated: "block",
			OnBudget:    "block",
		})
	}

	enforce := newRedirectFilter("enforce", &fakeClassifier{verdict: Verdict{Label: "complex", Score: 0.92, Model: "guard"}})
	res := enforce.Ingress(context.Background(), IngressInput{Message: "derive navier-stokes", Source: "channel"})
	if res.Drop || res.Skip || res.ModelOverride != "Strong-Model" {
		t.Fatalf("enforce redirect ingress = %+v", res)
	}
	if res.Reason != "redirect:complex->Strong-Model" || res.Meta == nil || res.Meta.Action != "redirect" || res.Meta.RedirectModel != "Strong-Model" {
		t.Fatalf("unexpected enforce redirect meta/reason: reason=%q meta=%+v", res.Reason, res.Meta)
	}

	shadow := newRedirectFilter("shadow", &fakeClassifier{verdict: Verdict{Label: "complex", Score: 0.92, Model: "guard"}})
	sres := shadow.Ingress(context.Background(), IngressInput{Message: "derive navier-stokes", Source: "channel"})
	if sres.Drop || sres.ModelOverride != "" {
		t.Fatalf("shadow mode must not set ModelOverride: %+v", sres)
	}
	if sres.Reason != "shadow:redirect:Strong-Model" || sres.Meta == nil || sres.Meta.Action != "redirect" || sres.Meta.RedirectModel != "Strong-Model" {
		t.Fatalf("unexpected shadow redirect meta/reason: reason=%q meta=%+v", sres.Reason, sres.Meta)
	}

	eg := enforce.Egress(context.Background(), EgressInput{Text: "answer", Source: "agent"})
	if eg.Drop || eg.Reason != "review:complex" || eg.Meta == nil || eg.Meta.Action != "review" {
		t.Fatalf("egress redirect should surface as review, got %+v", eg)
	}
}
