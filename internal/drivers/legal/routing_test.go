package legal

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// shapedEngine stands in for a real engine: it accepts only the path shape it
// understands and rejects everything else the way the real engines do.
type shapedEngine struct {
	name    string
	accepts func(string) bool
}

func (e *shapedEngine) Search(context.Context, Request) (*SearchResult, error) {
	return &SearchResult{}, nil
}

func (e *shapedEngine) Fetch(_ context.Context, fr FetchRequest) (*Judgment, error) {
	if !e.accepts(fr.Path) {
		// Wrapped exactly as the real engines wrap it: "this is not my kind of
		// path" is what lets Set.Fetch try the others, and a bare error would
		// be indistinguishable from a fetch that failed.
		return nil, fmt.Errorf("legal %s: path %q is not mine: %w", e.name, fr.Path, ErrPathNotForEngine)
	}
	return &Judgment{Text: "body from " + e.name, TextLen: 12}, nil
}

// hkliiShape mirrors hklii's identity(): 5 segments with "cases" in second
// place. A looser predicate would accept paths the real engine rejects and hide
// the routing these tests are about.
func hkliiShape(p string) bool {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	return len(parts) == 5 && parts[1] == "cases"
}

// npcShape mirrors npc's check: an opaque id with no separators.
func npcShape(p string) bool { return !strings.Contains(p, "/") }

// bothEngines builds the deployment the bug needed: two engines configured,
// hklii first, so it is the default.
func bothEngines() *Set {
	return &Set{
		Engines: map[string]Engine{
			"hklii": &shapedEngine{name: "hklii", accepts: hkliiShape},
			"npc":   &shapedEngine{name: "npc", accepts: npcShape},
		},
		Default: "hklii",
	}
}

// TestSetFetchReachesTheEngineThatOwnsThePath is the regression test for a
// legal_search that works followed by a legal_read that cannot.
//
// A hit's path is only meaningful to the engine that produced it: hklii emits
// /{lang}/cases/{abbr}/{year}/{num}, npc emits an opaque document id. With both
// configured and hklii as the default, reading an npc hit sent the id to hklii,
// which rejected it with "not a case judgment path" — an error naming the wrong
// problem, since the path is fine and merely went to the wrong backend.
func TestSetFetchReachesTheEngineThatOwnsThePath(t *testing.T) {
	set := bothEngines()
	ctx := context.Background()

	// An hklii path with no engine named goes to the default, as before.
	used, j, err := set.Fetch(ctx, "", FetchRequest{Path: "/en/cases/hkcfi/2020/1808"})
	if err != nil || used != "hklii" {
		t.Fatalf("hklii path: used=%q err=%v", used, err)
	}
	if j.Text != "body from hklii" {
		t.Fatalf("text = %q", j.Text)
	}

	// An npc id with no engine named must still be read: the default cannot
	// handle it, so it falls through to the engine that can.
	used, j, err = set.Fetch(ctx, "", FetchRequest{Path: "ff8081819ff54a6401a032af29bf2887"})
	if err != nil {
		t.Fatalf("an npc hit must be readable without naming the engine: %v", err)
	}
	if used != "npc" {
		t.Fatalf("used = %q; want npc", used)
	}
	if j.Text != "body from npc" {
		t.Fatalf("text = %q", j.Text)
	}

	// Naming the engine explicitly is never second-guessed.
	if used, _, err = set.Fetch(ctx, "hklii", FetchRequest{Path: "/en/cases/hkcfi/2020/1808"}); err != nil || used != "hklii" {
		t.Fatalf("explicit engine: used=%q err=%v", used, err)
	}
}

// TestSetFetchDoesNotMaskARealFailure: the fall-through is only for "not my
// kind of path". A transport or upstream error on the engine that does own the
// path is the answer, and hunting for another engine would replace a real
// failure with a misleading one — or with a bogus success.
func TestSetFetchDoesNotMaskARealFailure(t *testing.T) {
	set := bothEngines()
	set.Engines["npc"] = &failingEngine{}

	_, _, err := set.Fetch(context.Background(), "", FetchRequest{Path: "ff8081819ff54a6401a032af29bf2887"})
	if err == nil {
		t.Fatal("a failing fetch must not be reported as success")
	}
	if !strings.Contains(err.Error(), "upstream is down") {
		t.Fatalf("the real failure must surface, got %v", err)
	}
}

// failingEngine owns every path and always fails, standing in for an engine
// whose backend is unreachable.
type failingEngine struct{}

func (f *failingEngine) Search(context.Context, Request) (*SearchResult, error) {
	return nil, fmt.Errorf("upstream is down")
}

func (f *failingEngine) Fetch(context.Context, FetchRequest) (*Judgment, error) {
	return nil, fmt.Errorf("upstream is down")
}

// TestSetFetchReportsWhenNobodyOwnsThePath: falling through the engines must
// not invent a success, and the failure keeps naming a reason.
func TestSetFetchReportsWhenNobodyOwnsThePath(t *testing.T) {
	set := bothEngines()

	// Satisfies neither engine: hklii wants 5 segments under /cases, npc wants
	// no separator at all.
	_, _, err := set.Fetch(context.Background(), "", FetchRequest{Path: "/nope/nope"})
	if err == nil {
		t.Fatal("a path no engine accepts must fail")
	}
	if !strings.Contains(err.Error(), "hklii") {
		t.Fatalf("the error must name the engine whose refusal is reported: %v", err)
	}
}

// TestSetFetchSingleEngineUnchanged: with one engine there is nothing to fall
// through to, so behaviour is exactly what it was.
func TestSetFetchSingleEngineUnchanged(t *testing.T) {
	set := &Set{Engines: map[string]Engine{"hklii": &shapedEngine{name: "hklii", accepts: hkliiShape}}, Default: "hklii"}

	if _, _, err := set.Fetch(context.Background(), "", FetchRequest{Path: "/en/cases/hkcfi/2020/1808"}); err != nil {
		t.Fatalf("a matching path must still work: %v", err)
	}
	_, _, err := set.Fetch(context.Background(), "", FetchRequest{Path: "ff8081819ff54a6401a032af29bf2887"})
	if err == nil {
		t.Fatal("an unreadable path must still fail when there is nowhere to fall through to")
	}
}
