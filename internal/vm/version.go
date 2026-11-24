package vm

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// PreludeVersion is the version of the TypeScript/JS-facing API the prelude
// in this package implements, and the whole compatibility contract between
// this core and every loop, support chunk and route handler written against it.
//
// The contract has three parts:
//
//  1. The prelude declares this number as af.version, so script code can read it.
//  2. Every loop/route/support script file declares the version it targets with
//     a directive in its leading comment block: "// af-prelude-version: <n>".
//     The declaration is a comment rather than a call on purpose — see CheckChunkVersion.
//  3. A file that declares anything else, or nothing at all, is refused. There
//     is no deprecation window, so a mismatch fails loudly instead of running
//     scripts against an API they were not written for.
//
// Bump this for any change a chunk could observe: a builder renamed, an option
// renamed on the wire, an op's response shape changed, or a global added or
// removed.
const PreludeVersion = 2

// ErrPreludeVersion marks a refusal produced by the version gate. A caller can
// tell a version refusal from a read or compile failure with errors.Is, which
// is what lets hot reload keep the running loop on a mismatch — the file on
// disk is wrong, not the version that is already serving traffic.
var ErrPreludeVersion = errors.New("prelude API version")

// versionDirectiveRE matches the declaration a script file carries: a comment
// line, "// af-prelude-version: 2", with spacing left loose because a human
// writes it. Anchored at both ends so prose mentioning the directive is ignored.
var versionDirectiveRE = regexp.MustCompile(`^\s*//\s*af-prelude-version\s*:\s*(\d+)\s*$`)

// directiveLine renders the line a chunk declares PreludeVersion with.
func directiveLine(version int) string {
	return fmt.Sprintf("// af-prelude-version: %d", version)
}

// declaredVersion reports the prelude version a chunk declares, and whether it
// declared exactly one.
func declaredVersion(src string) (int, bool) {
	v := declaredVersions(src)
	if len(v) != 1 {
		return 0, false
	}
	return v[0], true
}

// declaredVersions returns every version declared in the run of blank lines
// and `//` comment lines at the top of the chunk. The scan stops at the first
// line that is neither, so a directive-shaped line deeper in the file is not
// treated as a declaration.
func declaredVersions(src string) []int {
	var out []int
	for _, line := range strings.Split(src, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if !strings.HasPrefix(t, "//") {
			break
		}
		m := versionDirectiveRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

// CheckChunkVersion is the hard gate: it refuses a script chunk whose declared
// prelude version is missing, stated twice, or different from PreludeVersion.
// Every refusal wraps ErrPreludeVersion and names the chunk source.
func CheckChunkVersion(name, src string) error {
	switch v := declaredVersions(src); len(v) {
	case 0:
		return fmt.Errorf("%s: declares no prelude API version; add a line like %s to its leading comment block (this core provides version %d): %w",
			name, directiveLine(PreludeVersion), PreludeVersion, ErrPreludeVersion)
	case 1:
		if v[0] == PreludeVersion {
			return nil
		}
		return fmt.Errorf("%s: targets prelude API version %d, this core provides %d; the core and the scripts that run on it move together, so this chunk is refused rather than loaded against a different API: %w",
			name, v[0], PreludeVersion, ErrPreludeVersion)
	default:
		return fmt.Errorf("%s: declares a prelude API version %d times (%v); a chunk targets exactly one: %w",
			name, len(v), v, ErrPreludeVersion)
	}
}
