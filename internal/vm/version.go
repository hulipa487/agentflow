package vm

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// PreludeVersion is the version of the Lua-facing API the prelude in this
// package implements, and the whole compatibility contract between this core
// and every loop, support chunk and route handler written against it.
//
// The contract has three parts:
//
//  1. The prelude declares this number as af.version, so Lua code can read it.
//  2. Every Lua file declares the version it targets with a directive in its
//     leading comment block: "-- af-prelude-version: <n>". The declaration is a
//     comment rather than a call on purpose — see CheckChunkVersion.
//  3. A file that declares anything else, or nothing at all, is refused. There
//     is no deprecation window ("essentially no deprecation window — core and
//     proprietary Lua move together"), so a mismatch fails loudly instead of
//     running Lua against an API it was not written for. The cost of that
//     choice is real and is the point: upgrading the core and bumping the
//     version in every Lua file is one atomic deploy, and a mismatch takes out
//     routing for every tenant at once rather than shaving an edge off it.
//
// Those three say how a version is declared and enforced. What the version
// *covers* is broader than the list of ops a chunk may call: it covers
// everything a chunk can observe, including the shape of the values an op hands
// back. The paragraph below is the rule; the message shape is the case that has
// already moved once.
//
// Bump this for any change a chunk could observe: a builder renamed, an option
// renamed on the wire, an op's response shape changed, a global added or
// removed. "A chunk could observe" includes the shape of the values an op hands
// back, not just whether the op exists. The one that moves most is the message
// a loop gets from session.inbox(): its field set, and the three-state
// provenance.user_uuid inside it (absent, represented by a nil *string; known
// empty, by a pointer to ""; or a tenant uuid), are as much a part of this
// contract as the op list is. A loop reading a key that moved — the tenant used
// to arrive as msg.payload.user_uuid — declares this version and passes the
// gate, so the gate cannot catch that class of break on its own; the shape is
// held still by TestInboxWireShape and TestInboxShapeReachesLua in
// internal/core/session instead. Adding a builder is observable only by a chunk
// that uses it, but the cost of bumping needlessly is one deploy and the cost
// of not bumping is a customer's loop silently misbehaving, so: any edit to the
// prelude's surface, or to the shape of what a loop is handed, bumps the
// version. TestPreludeDeclaresItsVersion and the builtins conformance suite
// (internal/builtins TestPreludeContract) are what keep that rule and the
// literal in the prelude from drifting apart.
const PreludeVersion = 1

// ErrPreludeVersion marks a refusal produced by the version gate. A caller can
// tell a version refusal from a read or compile failure with errors.Is, which
// is what lets hot reload keep the running loop on a mismatch — the file on
// disk is wrong, not the version that is already serving traffic.
var ErrPreludeVersion = errors.New("prelude API version")

// versionDirectiveRE matches the declaration a Lua file carries: a comment
// line, "-- af-prelude-version: 3", with the spacing left loose because a
// human writes it. Anchored at both ends so a line that merely mentions the
// directive in prose is not read as one.
var versionDirectiveRE = regexp.MustCompile(`^\s*--\s*af-prelude-version\s*:\s*(\d+)\s*$`)

// directiveLine renders the line a chunk declares PreludeVersion with. It is
// used in error messages, so the operator is told the exact line to write
// rather than sent to a document.
func directiveLine(version int) string {
	return fmt.Sprintf("-- af-prelude-version: %d", version)
}

// declaredVersion reports the prelude version a chunk declares, and whether it
// declared exactly one. A chunk that declares none, or two, reports false;
// CheckChunkVersion says which and why it matters.
func declaredVersion(src string) (int, bool) {
	v := declaredVersions(src)
	if len(v) != 1 {
		return 0, false
	}
	return v[0], true
}

// declaredVersions returns every version declared in the run of blank lines and
// comment lines at the top of the chunk. The scan stops at the first line that
// is neither, so a directive-shaped line deeper in the file — inside a string
// literal, say — is not a declaration.
//
// That is the entire parsing rule, and it is deliberately the boring one: the
// declaration is a comment at the top of the file, so reading it needs no Lua
// parser and no evaluation of the chunk. It matters that the gate can run
// before the chunk does. A chunk that declares its version by *calling*
// something would only be checked once it had already started running, and a
// version mismatch found there is a session that crash-restarts once a second
// instead of the old, still-valid loop kept in place — the exact failure hot
// reload exists to prevent. See CheckChunkVersion for where the gate runs.
func declaredVersions(src string) []int {
	var out []int
	for _, line := range strings.Split(src, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if !strings.HasPrefix(t, "--") {
			break
		}
		m := versionDirectiveRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			// Unreachable: the regexp captures digits only. Skipping rather
			// than panicking keeps a corrupt file a bad chunk, not a crash.
			continue
		}
		out = append(out, n)
	}
	return out
}

// CheckChunkVersion is the hard gate: it refuses a Lua chunk whose declared
// prelude version is missing, stated twice, or different from PreludeVersion.
//
// Every refusal wraps ErrPreludeVersion. The name is what the operator sees —
// a file path for anything read from disk, so the log line says which file to
// edit.
//
// It answers about the source it is handed, not about what the chunk does with
// the API. A chunk that targets the right version and calls a builder that no
// longer exists is still refused, but by the Lua runtime, at whatever turn
// reaches the call; nothing here can see that without running the chunk. The
// version is the contract, and this is a check on the contract.
func CheckChunkVersion(name, src string) error {
	switch v := declaredVersions(src); len(v) {
	case 0:
		return fmt.Errorf("%s: declares no prelude API version; add a line like %s to its leading comment block (this core provides version %d): %w",
			name, directiveLine(PreludeVersion), PreludeVersion, ErrPreludeVersion)
	case 1:
		if v[0] == PreludeVersion {
			return nil
		}
		return fmt.Errorf("%s: targets prelude API version %d, this core provides %d; the core and the Lua that runs on it move together, so this chunk is refused rather than loaded against a different API: %w",
			name, v[0], PreludeVersion, ErrPreludeVersion)
	default:
		return fmt.Errorf("%s: declares a prelude API version %d times (%v); a chunk targets exactly one: %w",
			name, len(v), v, ErrPreludeVersion)
	}
}
