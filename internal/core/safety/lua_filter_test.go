package safety

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestValidateLuaSource(t *testing.T) {
	if err := ValidateLuaSource("   \n "); err == nil {
		t.Fatal("expected blank source error")
	}
	if err := ValidateLuaSource("function ingress(input) return"); err == nil {
		t.Fatal("expected syntax error")
	}
	if err := ValidateLuaSource("local x = 1 + 2"); err == nil {
		t.Fatal("expected missing ingress/egress hook error")
	}
	if err := ValidateLuaSource("while true do end"); err == nil {
		t.Fatal("expected infinite init loop to hit instruction budget")
	}
	if err := ValidateLuaSource(`function ingress(input) return { text = input.message } end`); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestLuaFilterIngressEgressAndDispatcher(t *testing.T) {
	lf, err := NewLuaFilter(LuaFilterOptions{
		Name:    "pii_guard",
		Ingress: true,
		Egress:  true,
		Refusal: "blocked by policy",
		Fallback: "redacted",
		Source: `
function ingress(input)
  if input.annotations and input.annotations["source-attribution"] == "quoted" then
    return { reason = "quoted-bypass" }
  end
  if string.find(string.lower(input.message), "secret_cmd", 1, true) then
    return { drop = true, reason = "forbidden-cmd" }
  end
  if string.find(string.lower(input.message), "tone", 1, true) then
    return { directive = "Be brief.", directives = { "Stay factual." }, reason = "tone-directive" }
  end
  return { text = "[checked] " .. input.message, annotations = { ["custom"] = "ok" }, reason = "checked" }
end

function egress(input)
  local cleaned = string.gsub(input.text, "sk%-[A-Za-z0-9]+", "[REDACTED]")
  if string.find(cleaned, "FORBIDDEN_OUT", 1, true) then
    return { drop = true, reason = "egress-blocked" }
  end
  return { text = cleaned, reason = "egress-clean" }
end
`,
	})
	if err != nil {
		t.Fatalf("NewLuaFilter: %v", err)
	}

	disp := New(&Profile{Name: "custom", Filters: []Filter{SourceAttribution{}, lf}})

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

	// Directives propagate from Lua return table.
	dirRes := disp.Ingress(context.Background(), IngressInput{Message: "watch tone", Source: "channel"})
	if dirRes.Drop || dirRes.Text != "watch tone" || len(dirRes.Directives) != 2 || dirRes.Directives[0] != "Be brief." || dirRes.Directives[1] != "Stay factual." {
		t.Fatalf("unexpected dirRes = %+v", dirRes)
	}

	// Text mutation and custom annotations carry cleanly.
	mutRes := disp.Ingress(context.Background(), IngressInput{Message: "hello", Source: "channel"})
	if mutRes.Text != "[checked] hello" || mutRes.Annotations["custom"] != "ok" || mutRes.Annotations["lua:pii_guard"] != "checked" {
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

func TestLuaFilterSandboxAndBudget(t *testing.T) {
	// Sandboxed VM removes os/io/require; attempting os.getenv fails cleanly under OnError policy.
	sandboxFilter, err := NewLuaFilter(LuaFilterOptions{
		Name:    "sandbox_probe",
		Ingress: true,
		OnError: "block",
		Refusal: "error-blocked",
		Source: `
function ingress(input)
  return { text = os.getenv("PATH") }
end
`,
	})
	if err != nil {
		t.Fatalf("NewLuaFilter sandbox: %v", err)
	}
	res := sandboxFilter.Ingress(context.Background(), IngressInput{Message: "hi", Source: "channel"})
	if !res.Drop || !res.Skip || res.Refusal != "error-blocked" || res.Reason != "lua-error:sandbox_probe" {
		t.Fatalf("expected sandbox violation block, got %+v", res)
	}

	// Infinite loop trips instruction budget and respects OnError: pass vs block.
	loopPass, err := NewLuaFilter(LuaFilterOptions{
		Name:        "spin_pass",
		Ingress:     true,
		InstrBudget: 500,
		Timeout:     100 * time.Millisecond,
		OnError:     "pass",
		Source:      "function ingress(input) while true do end end",
	})
	if err != nil {
		t.Fatalf("NewLuaFilter spin_pass: %v", err)
	}
	passRes := loopPass.Ingress(context.Background(), IngressInput{Message: "hello", Source: "channel"})
	if passRes.Drop || passRes.Text != "hello" || !strings.HasPrefix(passRes.Reason, "lua-error-pass:") {
		t.Fatalf("expected pass on error, got %+v", passRes)
	}
}

func TestLuaFilterDirectionsAndSources(t *testing.T) {
	lf, err := NewLuaFilter(LuaFilterOptions{
		Name:    "channel_ingress_only",
		Ingress: true,
		Egress:  false,
		Sources: []string{"channel"},
		Source: `
function ingress(input) return { text = "in:" .. input.message } end
function egress(input) return { text = "out:" .. input.text } end
`,
	})
	if err != nil {
		t.Fatalf("NewLuaFilter: %v", err)
	}
	if got := lf.Ingress(context.Background(), IngressInput{Message: "m", Source: "scheduler"}); got.Text != "m" {
		t.Fatalf("scheduler source should be skipped, got %+v", got)
	}
	if got := lf.Ingress(context.Background(), IngressInput{Message: "m", Source: "channel"}); got.Text != "in:m" {
		t.Fatalf("channel source should run, got %+v", got)
	}
	if got := lf.Egress(context.Background(), EgressInput{Text: "r", Source: "channel"}); got.Text != "r" {
		t.Fatalf("egress disabled should pass through, got %+v", got)
	}
}
