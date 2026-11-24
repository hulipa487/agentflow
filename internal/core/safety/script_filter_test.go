package safety

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestValidateScriptSource(t *testing.T) {
	if err := ValidateScriptSource("   \n "); err == nil {
		t.Fatal("expected blank source error")
	}
	if err := ValidateScriptSource("function ingress(input) { return"); err == nil {
		t.Fatal("expected syntax error")
	}
	if err := ValidateScriptSource("const x: number = 1 + 2;"); err == nil {
		t.Fatal("expected missing ingress/egress hook error")
	}
	if err := ValidateScriptSource("while (true) {}"); err == nil {
		t.Fatal("expected infinite init loop to hit sandbox timeout")
	}
	if err := ValidateScriptSource(`export function ingress(input: { message: string }) { return { text: input.message }; }`); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestScriptFilterIngressEgressAndDispatcher(t *testing.T) {
	sf, err := NewScriptFilter(ScriptFilterOptions{
		Name:     "pii_guard",
		Ingress:  true,
		Egress:   true,
		Refusal:  "blocked by policy",
		Fallback: "redacted",
		Source: `
interface HookInput {
  message?: string;
  text?: string;
  annotations?: Record<string, string>;
}

export function ingress(input: HookInput) {
  if (input.annotations && input.annotations["source-attribution"] === "quoted") {
    return { reason: "quoted-bypass" };
  }
  const lower = (input.message || "").toLowerCase();
  if (lower.includes("secret_cmd")) {
    return { drop: true, reason: "forbidden-cmd" };
  }
  if (lower.includes("tone")) {
    return { directive: "Be brief.", directives: ["Stay factual."], reason: "tone-directive" };
  }
  return { text: "[checked] " + input.message, annotations: { custom: "ok" }, reason: "checked" };
}

export function egress(input: HookInput) {
  const cleaned = (input.text || "").replace(/sk-[A-Za-z0-9]+/g, "[REDACTED]");
  if (cleaned.includes("FORBIDDEN_OUT")) {
    return { drop: true, reason: "egress-blocked" };
  }
  return { text: cleaned, reason: "egress-clean" };
}
`,
	})
	if err != nil {
		t.Fatalf("NewScriptFilter: %v", err)
	}

	disp := New(&Profile{Name: "custom", Filters: []Filter{SourceAttribution{}, sf}})

	// Blocked ingress drops and skips loop with configured refusal.
	dropRes := disp.Ingress(context.Background(), IngressInput{Message: "please run secret_cmd", Source: "channel"})
	if !dropRes.Drop || !dropRes.Skip || dropRes.Refusal != "blocked by policy" || dropRes.Reason != "forbidden-cmd" {
		t.Fatalf("unexpected dropRes = %+v", dropRes)
	}

	// Quoted text sees upstream SourceAttribution annotation.
	quotedRes := disp.Ingress(context.Background(), IngressInput{Message: "> secret_cmd", Source: "channel"})
	if quotedRes.Drop || quotedRes.Reason != "quoted-bypass" {
		t.Fatalf("unexpected quotedRes = %+v", quotedRes)
	}

	// Directives propagate from return object.
	dirRes := disp.Ingress(context.Background(), IngressInput{Message: "watch tone", Source: "channel"})
	if dirRes.Drop || dirRes.Text != "watch tone" || len(dirRes.Directives) != 2 || dirRes.Directives[0] != "Be brief." || dirRes.Directives[1] != "Stay factual." {
		t.Fatalf("unexpected dirRes = %+v", dirRes)
	}

	// Text mutation and custom annotations carry cleanly.
	mutRes := disp.Ingress(context.Background(), IngressInput{Message: "hello", Source: "channel"})
	if mutRes.Text != "[checked] hello" || mutRes.Annotations["custom"] != "ok" || mutRes.Annotations["script:pii_guard"] != "checked" {
		t.Fatalf("unexpected mutRes = %+v", mutRes)
	}

	// Egress redaction and drop fallback.
	egClean := disp.Egress(context.Background(), EgressInput{Text: "key is sk-abc123XYZ", Source: "channel"})
	if egClean.Drop || egClean.Text != "key is [REDACTED]" {
		t.Fatalf("unexpected egClean = %+v", egClean)
	}
	egDrop := disp.Egress(context.Background(), EgressInput{Text: "FORBIDDEN_OUT", Source: "channel"})
	if !egDrop.Drop || egDrop.Fallback != "redacted" || egDrop.Reason != "egress-blocked" {
		t.Fatalf("unexpected egDrop = %+v", egDrop)
	}
}

func TestScriptFilterSandboxAndBudget(t *testing.T) {
	// Sandboxed VM removes host globals; attempting process.env fails cleanly under OnError policy.
	sandboxFilter, err := NewScriptFilter(ScriptFilterOptions{
		Name:    "sandbox_probe",
		Ingress: true,
		OnError: "block",
		Refusal: "error-blocked",
		Source: `
function ingress(_input: unknown) {
  return { text: (process as unknown as Record<string, string>).env };
}
`,
	})
	if err != nil {
		t.Fatalf("NewScriptFilter sandbox: %v", err)
	}
	res := sandboxFilter.Ingress(context.Background(), IngressInput{Message: "hi", Source: "channel"})
	if !res.Drop || !res.Skip || res.Refusal != "error-blocked" || res.Reason != "script-error:sandbox_probe" {
		t.Fatalf("expected sandbox violation block, got %+v", res)
	}

	// Infinite loop trips timeout/budget and respects OnError: pass vs block.
	loopPass, err := NewScriptFilter(ScriptFilterOptions{
		Name:        "spin_pass",
		Ingress:     true,
		InstrBudget: 500,
		Timeout:     100 * time.Millisecond,
		OnError:     "pass",
		Source:      "function ingress(_input) { while (true) {} }",
	})
	if err != nil {
		t.Fatalf("NewScriptFilter spin_pass: %v", err)
	}
	passRes := loopPass.Ingress(context.Background(), IngressInput{Message: "hello", Source: "channel"})
	if passRes.Drop || passRes.Text != "hello" || !strings.HasPrefix(passRes.Reason, "script-error-pass:") {
		t.Fatalf("expected pass on error, got %+v", passRes)
	}
}

func TestScriptFilterDirectionsAndSources(t *testing.T) {
	sf, err := NewScriptFilter(ScriptFilterOptions{
		Name:    "channel_ingress_only",
		Ingress: true,
		Egress:  false,
		Sources: []string{"channel"},
		Source: `
function ingress(input: { message: string }) { return { text: "in:" + input.message }; }
function egress(input: { text: string }) { return { text: "out:" + input.text }; }
`,
	})
	if err != nil {
		t.Fatalf("NewScriptFilter: %v", err)
	}
	if got := sf.Ingress(context.Background(), IngressInput{Message: "m", Source: "scheduler"}); got.Text != "m" {
		t.Fatalf("scheduler source should be skipped, got %+v", got)
	}
	if got := sf.Ingress(context.Background(), IngressInput{Message: "m", Source: "channel"}); got.Text != "in:m" {
		t.Fatalf("channel source should run, got %+v", got)
	}
	if got := sf.Egress(context.Background(), EgressInput{Text: "r", Source: "channel"}); got.Text != "r" {
		t.Fatalf("egress disabled should pass through, got %+v", got)
	}
}

func TestScriptFilterRedirectIngressAndDispatcher(t *testing.T) {
	sf, err := NewScriptFilter(ScriptFilterOptions{
		Name:    "keyword_router",
		Ingress: true,
		Source: `
function ingress(input: { message: string }) {
  const lower = input.message.toLowerCase();
  if (lower.includes("legal")) {
    return { redirect: "Legal-Pro", directive: "Cite statutes." };
  }
  if (lower.includes("code")) {
    return { model: "Code-Max", reason: "code-route" };
  }
  return null;
}
`,
	})
	if err != nil {
		t.Fatalf("NewScriptFilter: %v", err)
	}
	disp := New(&Profile{Name: "route", Filters: []Filter{SourceAttribution{}, sf}})

	resLegal := disp.Ingress(context.Background(), IngressInput{Message: "Need legal contract advice", Source: "channel"})
	if resLegal.Drop || resLegal.ModelOverride != "Legal-Pro" || resLegal.Reason != "redirect:Legal-Pro" {
		t.Fatalf("unexpected resLegal = %+v", resLegal)
	}
	if len(resLegal.Directives) != 1 || resLegal.Directives[0] != "Cite statutes." {
		t.Fatalf("expected directive preserved with redirect, got %+v", resLegal.Directives)
	}
	if resLegal.Meta == nil || resLegal.Meta.Action != "redirect" || resLegal.Meta.RedirectModel != "Legal-Pro" || resLegal.Meta.Classifier != "script:keyword_router" {
		t.Fatalf("unexpected resLegal meta = %+v", resLegal.Meta)
	}

	resCode := disp.Ingress(context.Background(), IngressInput{Message: "Fix this Go code", Source: "channel"})
	if resCode.Drop || resCode.ModelOverride != "Code-Max" || resCode.Reason != "code-route" {
		t.Fatalf("unexpected resCode = %+v", resCode)
	}
	if resCode.Meta == nil || resCode.Meta.RedirectModel != "Code-Max" {
		t.Fatalf("unexpected resCode meta = %+v", resCode.Meta)
	}

	resPlain := disp.Ingress(context.Background(), IngressInput{Message: "hello", Source: "channel"})
	if resPlain.ModelOverride != "" || resPlain.Meta != nil {
		t.Fatalf("plain message should not redirect, got %+v", resPlain)
	}
}
