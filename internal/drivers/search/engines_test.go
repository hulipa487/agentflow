package search

import (
	"slices"
	"strings"
	"testing"

	"agentflow/internal/config"
)

// TestEngineListsAgree: engineNames, NewEngine's switch and keyedEngines are
// three answers to "which engines exist", kept by hand in one file. They drifted
// silently before — an engine the validator accepted that NewEngine could not
// build, or one missing from keyedEngines that skipped the warning about an
// unresolvable credential. Each assertion here is one direction of that drift.
func TestEngineListsAgree(t *testing.T) {
	for _, name := range engineNames {
		// Every advertised engine must be buildable. An empty config may fail
		// for its own reasons (a missing base_url, say); what must not happen is
		// "unsupported engine".
		if _, err := NewEngine(name, config.SearchEngine{}); err != nil &&
			strings.Contains(err.Error(), "unsupported engine") {
			t.Errorf("engineNames advertises %q but NewEngine cannot build it", name)
		}
	}

	// And keyedEngines may only name engines that exist, or a typo there would
	// quietly disable the honest-degradation path for a real one.
	for name := range keyedEngines {
		if !slices.Contains(engineNames, name) {
			t.Errorf("keyedEngines names %q, which is not an engine this build has", name)
		}
	}

	// A name NewEngine builds but engineNames omits would be invisible to the
	// checks above, so at least one keyless engine has to be in the list for the
	// keyless case to be meaningful.
	if len(engineNames) <= len(keyedEngines) {
		t.Errorf("engineNames has %d entries and keyedEngines %d: no keyless engine is listed",
			len(engineNames), len(keyedEngines))
	}
}
