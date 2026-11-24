package vm

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var preludeAfVersionRE = regexp.MustCompile(`(?m)^\s*af\.version\s*=\s*(\d+);?\s*$`)

func directive() string { return fmt.Sprintf("// af-prelude-version: %d\n", PreludeVersion) }

func TestStartRefusesAnUngatedSource(t *testing.T) {
	const name = "in-memory loop of session main|test"
	body := "function loop() { session.inbox(); }\n"

	refused := New(1_000_000)
	defer refused.Close()
	if err := refused.LoadBase(); err != nil {
		t.Fatal(err)
	}
	if status, msg := refused.Start("loop", name, body); status != Failed {
		t.Fatalf("an ungated in-memory source was loaded (status %v, msg %q)", status, msg)
	} else if !strings.Contains(msg, name) || !strings.Contains(msg, "af-prelude-version") {
		t.Fatalf("refusal is not the gate's: %q", msg)
	}

	accepted := New(1_000_000)
	defer accepted.Close()
	if err := accepted.LoadBase(); err != nil {
		t.Fatal(err)
	}
	if status, msg := accepted.Start("loop", name, directive()+body); status != Yielded {
		t.Fatalf("a source declaring this core's version was refused: status %v, msg %q", status, msg)
	}
}

func TestPreludeDeclaresItsVersion(t *testing.T) {
	m := preludeAfVersionRE.FindStringSubmatch(prelude)
	if m == nil {
		t.Fatal("the prelude no longer declares af.version on a line of its own")
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("af.version = %q is not a number: %v", m[1], err)
	}
	if n != PreludeVersion {
		t.Fatalf("the prelude declares af.version = %d but PreludeVersion is %d", n, PreludeVersion)
	}
}

func TestPreludeVersionIsVisibleToScript(t *testing.T) {
	st := New(1_000_000)
	defer st.Close()
	if err := st.LoadBase(); err != nil {
		t.Fatal(err)
	}
	src := fmt.Sprintf(`if (af.version !== %d) throw new Error("af.version is " + String(af.version));`, PreludeVersion)
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
		{"first line", "// af-prelude-version: 2\nfunction loop() {}\n", 2, true},
		{"after a banner and blank lines", "\n// plugin:thing\n// af-prelude-version: 7\n\nfunction loop() {}\n", 7, true},
		{"loose spacing", "//   af-prelude-version:   12  \nfunction loop() {}\n", 12, true},
		{"single line, no trailing newline", "// af-prelude-version: 3", 3, true},
		{"missing", "function loop() {}\n", 0, false},
		{"empty", "", 0, false},
		{"after code", "const x = 1;\n// af-prelude-version: 2\nfunction loop() {}\n", 0, false},
		{"inside a string literal", "const s = \"// af-prelude-version: 2\";\n", 0, false},
		{"mentioned in prose", "// see af-prelude-version: 2 for the rule\n", 0, false},
		{"declared twice", "// af-prelude-version: 1\n// af-prelude-version: 2\n", 0, false},
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
	const name = "loops/react.ts"

	t.Run("the core's own version is accepted", func(t *testing.T) {
		src := fmt.Sprintf("// af-prelude-version: %d\nfunction loop() {}\n", PreludeVersion)
		if err := CheckChunkVersion(name, src); err != nil {
			t.Fatalf("a chunk targeting this core's version was refused: %v", err)
		}
	})

	t.Run("another version is refused", func(t *testing.T) {
		src := fmt.Sprintf("// af-prelude-version: %d\nfunction loop() {}\n", PreludeVersion+1)
		err := CheckChunkVersion(name, src)
		if err == nil {
			t.Fatal("a chunk targeting another prelude version was accepted")
		}
		if !errors.Is(err, ErrPreludeVersion) {
			t.Fatalf("refusal does not wrap ErrPreludeVersion: %v", err)
		}
		for _, want := range []string{name, fmt.Sprint(PreludeVersion + 1), fmt.Sprint(PreludeVersion)} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("refusal does not mention %q: %v", want, err)
			}
		}
	})

	t.Run("silence is refused", func(t *testing.T) {
		err := CheckChunkVersion(name, "function loop() {}\n")
		if err == nil {
			t.Fatal("a chunk that declares no version was accepted")
		}
		if !errors.Is(err, ErrPreludeVersion) {
			t.Fatalf("refusal does not wrap ErrPreludeVersion: %v", err)
		}
		if !strings.Contains(err.Error(), "af-prelude-version") {
			t.Fatalf("refusal does not say which line to add: %v", err)
		}
	})

	t.Run("declaring twice is refused", func(t *testing.T) {
		err := CheckChunkVersion(name, "// af-prelude-version: 1\n// af-prelude-version: 2\n")
		if err == nil {
			t.Fatal("a chunk declaring two versions was accepted")
		}
		if !errors.Is(err, ErrPreludeVersion) {
			t.Fatalf("refusal does not wrap ErrPreludeVersion: %v", err)
		}
	})
}
