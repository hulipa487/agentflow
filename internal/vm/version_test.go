package vm

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The prelude declares af.version, and PreludeVersion is the Go side of the same
// number. Two spellings of one fact, so this test is the tie between them: a
// bump that edits one and not the other fails here instead of shipping a core
// that gates every chunk against a version the prelude does not admit to.
var preludeAfVersionRE = regexp.MustCompile(`(?m)^af\.version\s*=\s*(\d+)\s*$`)

func TestPreludeDeclaresItsVersion(t *testing.T) {
	m := preludeAfVersionRE.FindStringSubmatch(prelude)
	if m == nil {
		t.Fatal("the prelude no longer declares af.version on a line of its own; PreludeVersion has nothing to be tied to")
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("af.version = %q is not a number: %v", m[1], err)
	}
	if n != PreludeVersion {
		t.Fatalf("the prelude declares af.version = %d but PreludeVersion is %d; they are one number and must be bumped together", n, PreludeVersion)
	}
}

// TestPreludeVersionIsVisibleToLua is the end of the wire the contract promises:
// a loaded prelude answers af.version with the number this core gates against.
// Reading the constant out of the Go source would not have caught the prelude
// assigning it under a different name, or not at all.
func TestPreludeVersionIsVisibleToLua(t *testing.T) {
	st := New(1_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	src := fmt.Sprintf("assert(af.version == %d, 'af.version is ' .. tostring(af.version))", PreludeVersion)
	if err := st.Eval("@test", src); err != nil {
		t.Fatalf("af.version in the loaded prelude is not %d: %v", PreludeVersion, err)
	}
}

func TestDeclaredVersion(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
		ok   bool
	}{
		{"first line", "-- af-prelude-version: 1\nfunction loop() end\n", 1, true},
		{"after a banner and blank lines", "\n-- plugin:thing\n-- af-prelude-version: 7\n\nreturn 1\n", 7, true},
		{"loose spacing", "--   af-prelude-version:   12  \nreturn 1\n", 12, true},
		{"after a luau mode directive", "--!strict\n-- af-prelude-version: 2\nreturn 1\n", 2, true},
		{"single line, no trailing newline", "-- af-prelude-version: 3", 3, true},
		{"missing", "function loop() end\n", 0, false},
		{"empty", "", 0, false},
		// The scan stops at the first line that is neither blank nor a comment,
		// so a directive-shaped line further down is not a declaration. That is
		// the whole parsing rule: the declaration is a comment at the top, and
		// reading it that way needs no Lua parser and no running chunk.
		{"after code", "local x = 1\n-- af-prelude-version: 1\nreturn x\n", 0, false},
		{"inside a string literal", "local s = \"-- af-prelude-version: 1\"\nreturn s\n", 0, false},
		{"mentioned in prose", "-- see af-prelude-version: 1 for the rule\nreturn 1\n", 0, false},
		{"declared twice", "-- af-prelude-version: 1\n-- af-prelude-version: 2\nreturn 1\n", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := declaredVersion(c.src)
			if ok != c.ok || got != c.want {
				t.Fatalf("declaredVersion = %d, %v; want %d, %v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestCheckChunkVersion(t *testing.T) {
	const name = "loops/react.lua"

	t.Run("the core's own version is accepted", func(t *testing.T) {
		src := fmt.Sprintf("-- af-prelude-version: %d\nfunction loop() end\n", PreludeVersion)
		if err := CheckChunkVersion(name, src); err != nil {
			t.Fatalf("a chunk targeting this core's version was refused: %v", err)
		}
	})

	t.Run("another version is refused", func(t *testing.T) {
		src := fmt.Sprintf("-- af-prelude-version: %d\nfunction loop() end\n", PreludeVersion+1)
		err := CheckChunkVersion(name, src)
		if err == nil {
			t.Fatal("a chunk targeting another prelude version was accepted")
		}
		if !errors.Is(err, ErrPreludeVersion) {
			t.Fatalf("refusal does not wrap ErrPreludeVersion: %v", err)
		}
		// Loud enough to act on without opening the source: which file, which
		// version it wants, which version it got.
		for _, want := range []string{name, fmt.Sprint(PreludeVersion + 1), fmt.Sprint(PreludeVersion)} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("refusal does not mention %q: %v", want, err)
			}
		}
	})

	t.Run("silence is refused, not assumed compatible", func(t *testing.T) {
		err := CheckChunkVersion(name, "function loop() end\n")
		if err == nil {
			t.Fatal("a chunk that declares no version was accepted; the gate would then only catch chunks that opted in")
		}
		if !errors.Is(err, ErrPreludeVersion) {
			t.Fatalf("refusal does not wrap ErrPreludeVersion: %v", err)
		}
		// The one message a loop author is most likely to meet has to say what
		// to write, not merely that something is wrong.
		if !strings.Contains(err.Error(), "af-prelude-version") {
			t.Fatalf("refusal does not say which line to add: %v", err)
		}
	})

	t.Run("declaring twice is refused", func(t *testing.T) {
		err := CheckChunkVersion(name, "-- af-prelude-version: 1\n-- af-prelude-version: 2\nreturn 1\n")
		if err == nil {
			t.Fatal("a chunk declaring two versions was accepted")
		}
		if !errors.Is(err, ErrPreludeVersion) {
			t.Fatalf("refusal does not wrap ErrPreludeVersion: %v", err)
		}
	})
}
