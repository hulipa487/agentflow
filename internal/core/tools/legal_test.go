package tools

import (
	"context"
	"strings"
	"testing"

	"agentflow/internal/config"
	"agentflow/internal/drivers/legal"
)

// fakeLegalEngine satisfies legal.Engine. These tests are about the tool's
// description and result shape, so the engine only has to answer well-formed.
type fakeLegalEngine struct {
	judgment *legal.Judgment
}

func (f *fakeLegalEngine) Search(ctx context.Context, req legal.Request) (*legal.SearchResult, error) {
	return &legal.SearchResult{Query: req.Query, Total: 1, Results: []legal.Case{{Title: "t", Path: "p"}}}, nil
}

func (f *fakeLegalEngine) Fetch(ctx context.Context, fr legal.FetchRequest) (*legal.Judgment, error) {
	if f.judgment != nil {
		return f.judgment, nil
	}
	return &legal.Judgment{Title: "t", Text: "body", TextLen: 4}, nil
}

// legalSetFor builds a Set with the named engines configured.
func legalSetFor(engines ...string) *legal.Set {
	s := &legal.Set{Engines: map[string]legal.Engine{}}
	for i, n := range engines {
		s.Engines[n] = &fakeLegalEngine{}
		if i == 0 {
			s.Default = n
		}
	}
	return s
}

func legalSpec(t *testing.T, set *legal.Set, name string) ToolSpec {
	t.Helper()
	r := NewRegistry()
	RegisterLegalBuiltins(r, set)
	spec, ok := r.tools[name]
	if !ok {
		t.Fatalf("%s must be registered", name)
	}
	return spec
}

func legalProps(spec ToolSpec) map[string]any {
	p, _ := spec.Parameters["properties"].(map[string]any)
	return p
}

// TestLegalReadDescriptionIsEngineAware is the point of the rewrite. The
// description used to say "Read one legal judgment's full text by its path
// (from legal_search) or abbr/year/num" for every deployment — which is wrong
// for npc three times over: an npc hit is legislation, not a judgment, it has
// no abbr/year/num, and its Fetch reads none of them.
func TestLegalReadDescriptionIsEngineAware(t *testing.T) {
	hk := legalSpec(t, legalSetFor("hklii"), "builtin:legal_read")
	if !strings.Contains(hk.Description, "case judgment") {
		t.Fatalf("an hklii deployment reads case judgments: %s", hk.Description)
	}
	if strings.Contains(hk.Description, "npc") {
		t.Fatalf("npc must not be mentioned when it is not configured: %s", hk.Description)
	}
	// The caveat that legal_search advertises legislation this tool cannot read.
	if !strings.Contains(hk.Description, "Ordinances") {
		t.Fatalf("the unreadable-legislation caveat must be stated: %s", hk.Description)
	}
	for _, p := range []string{"abbr", "year", "num", "lang"} {
		if _, has := legalProps(hk)[p]; !has {
			t.Fatalf("hklii honours %q, so it must be offered", p)
		}
	}

	np := legalSpec(t, legalSetFor("npc"), "builtin:legal_read")
	if !strings.Contains(np.Description, "opaque document id") {
		t.Fatalf("npc's path is an opaque id and must be described as one: %s", np.Description)
	}
	// npc's Fetch reads only Path, so offering these would invite a model to
	// pass an argument the engine silently discards.
	for _, p := range []string{"abbr", "year", "num", "lang"} {
		if _, has := legalProps(np)[p]; has {
			t.Fatalf("npc ignores %q, so it must not be advertised", p)
		}
	}
	for _, p := range []string{"path", "start", "max_chars"} {
		if _, has := legalProps(np)[p]; !has {
			t.Fatalf("npc honours %q, so it must be offered", p)
		}
	}

	both := legalSpec(t, legalSetFor("hklii", "npc"), "builtin:legal_read")
	for _, want := range []string{"hklii", "npc", "abbr", "lang"} {
		if !strings.Contains(both.Description, want) {
			t.Fatalf("with both engines configured the description must cover %q: %s", want, both.Description)
		}
	}
	if _, has := legalProps(both)["lang"]; !has {
		t.Fatal("lang is meaningful for one of the two engines and must still be offered")
	}
}

// TestLegalSearchDescriptionIsEngineAware: the old text named HKLII and
// promised "citations, court, date, and parties" whatever was configured — and
// npc has none of the citations or parties, while its `court` is the enacting
// organ.
func TestLegalSearchDescriptionIsEngineAware(t *testing.T) {
	np := legalSpec(t, legalSetFor("npc"), "builtin:legal_search")
	if strings.Contains(np.Description, "HKLII") {
		t.Fatalf("an npc-only deployment must not be described in hklii's terms: %s", np.Description)
	}
	if !strings.Contains(np.Description, "enacting organ") {
		t.Fatalf("npc's `court` is the enacting organ and must be said so: %s", np.Description)
	}
	if !strings.Contains(np.Description, "no citations and no parties") {
		t.Fatalf("the absent fields must be called out: %s", np.Description)
	}
	// disable_fuzzy means something different on each engine.
	dz, _ := legalProps(np)["disable_fuzzy"].(map[string]any)
	if desc, _ := dz["description"].(string); !strings.Contains(desc, "title only") {
		t.Fatalf("npc's disable_fuzzy matches the title, not a keyword: %s", desc)
	}

	hk := legalSpec(t, legalSetFor("hklii"), "builtin:legal_search")
	if !strings.Contains(hk.Description, "neutral and parallel citation") {
		t.Fatalf("hklii's citations must be described: %s", hk.Description)
	}
	if strings.Contains(hk.Description, "npc") {
		t.Fatalf("npc must not be mentioned when it is not configured: %s", hk.Description)
	}

	both := legalSpec(t, legalSetFor("hklii", "npc"), "builtin:legal_search")
	dz, _ = legalProps(both)["disable_fuzzy"].(map[string]any)
	desc, _ := dz["description"].(string)
	if !strings.Contains(desc, "hklii") || !strings.Contains(desc, "npc") {
		t.Fatalf("with both engines the difference must be spelled out: %s", desc)
	}
}

// TestLegalToolsUnconfigured: no engine leaves both honest-unavailable, and the
// description says so rather than enumerating parameters nothing will honour.
func TestLegalToolsUnconfigured(t *testing.T) {
	empty := &legal.Set{}
	for _, name := range []string{"builtin:legal_search", "builtin:legal_read"} {
		spec := legalSpec(t, empty, name)
		if !strings.Contains(spec.Description, "honest unavailable") {
			t.Fatalf("%s: %s", name, spec.Description)
		}
		if _, has := legalProps(spec)["engine"]; has {
			t.Fatalf("%s must not offer an engine enum when none is configured", name)
		}
	}
}

// TestLegalReadOmitsFieldsTheEngineDoesNotFill: npc returns no court, date,
// parallel citation or translation flag. Emitting them as empty strings reads as
// "unknown" rather than "this engine does not have it", which is the wrong
// conclusion to invite from a result the model is reasoning over.
func TestLegalReadOmitsFieldsTheEngineDoesNotFill(t *testing.T) {
	set := legalSetFor("npc")
	set.Engines["npc"] = &fakeLegalEngine{judgment: &legal.Judgment{
		// What npc.Fetch actually returns: a title from the first line, the
		// bbbs id in Neutral, and nothing else.
		Title:   "中华人民共和国刑法",
		Neutral: "ff8081819ff54a6401a032af29bf2887",
		Text:    "第一条 …",
		TextLen: 42,
	}}

	r := NewRegistry()
	RegisterLegalBuiltins(r, set)
	as := r.Expose([]string{"builtin:legal_read"}, config.ToolsPolicy{}, false)
	res, err := as.Invoke(context.Background(), "builtin:legal_read",
		map[string]any{"path": "ff8081819ff54a6401a032af29bf2887"})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result type %T", res)
	}
	for _, absent := range []string{"court", "date", "parallel_citation", "has_translation", "truncated"} {
		if _, has := m[absent]; has {
			t.Fatalf("npc does not fill %q, so it must be absent rather than empty: %v", absent, m[absent])
		}
	}
	for _, present := range []string{"title", "neutral", "text", "text_len"} {
		if _, has := m[present]; !has {
			t.Fatalf("%q must be present: %v", present, m)
		}
	}
	if m["neutral"] != "ff8081819ff54a6401a032af29bf2887" {
		t.Fatalf("npc's neutral carries the document id: %v", m["neutral"])
	}
}
