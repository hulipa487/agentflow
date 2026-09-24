package metrics

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryCounterHasAnIncrementSite: a counter that is registered and never
// incremented renders as a permanent zero, which an operator reads as "this
// never happens" rather than "this is not measured". Nine shipped that way — the
// dashboard's entire Traffic section among them, plus three gauges — and nothing
// caught it, because a counter's name and its call sites are connected by a
// string and nothing else. TestCuratedCountersExist cannot see it either: it
// checks that a curated row names a registered counter, and a dead counter is
// registered.
//
// So this connects them by scanning the source: every counter in the default set
// must appear inside a metrics.Inc/metrics.Add call outside this package and
// outside tests. A gauge whose value is Set rather than Inc'd would need a third
// pattern here — there is none today.
func TestEveryCounterHasAnIncrementSite(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	counters := DefaultCounters()
	seen := map[string]bool{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "third_party", "build", "data", "metrics":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(b)
		for name := range counters {
			if seen[name] {
				continue
			}
			if strings.Contains(src, `metrics.Inc("`+name+`")`) ||
				strings.Contains(src, `metrics.Add("`+name+`"`) {
				seen[name] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for name := range counters {
		if !seen[name] {
			t.Errorf("counter %q is registered but never incremented outside this package — it can only ever render as zero", name)
		}
	}
}
