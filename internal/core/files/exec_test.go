package files

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The executable bit is the one permission the store carries. It must survive
// everything a script goes through: a rewrite of its text, a commit, and a
// checkout.
func TestExecutableBitSurvivesRewriteCommitAndCheckout(t *testing.T) {
	m, _ := testManager(t, time.Hour)
	ctx := context.Background()
	const scope, project = "user:u1", "proj"

	e, err := m.PutFile(ctx, scope, project, "run.sh", strings.NewReader("#!/bin/sh\n"), "text/x-sh", true)
	if err != nil || !e.Exec {
		t.Fatalf("PutFile = %+v, %v; want an executable entry", e, err)
	}
	putString(t, m, scope, project, "notes.txt", "plain")

	// A loop rewriting the script's text does not mean to change what it is.
	if e := putString(t, m, scope, project, "run.sh", "#!/bin/sh\necho v2\n"); !e.Exec || e.Revision != 2 {
		t.Fatalf("a rewrite dropped the bit or the revision: %+v", e)
	}
	if e, _ := m.PutHandle(ctx, scope, project, "run.sh", e.Handle, "text/x-sh"); e == nil || !e.Exec {
		t.Fatalf("a put by handle dropped the bit: %+v", e)
	}
	if e, _ := m.Get(ctx, scope, project, "notes.txt"); e.Exec {
		t.Fatal("an ordinary file became executable")
	}

	c, err := m.Commit(ctx, scope, project, "main", "init")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Exec["run.sh"] || c.Exec["notes.txt"] {
		t.Fatalf("commit records exec = %v", c.Exec)
	}
	// The working tree may change afterwards; the snapshot keeps what it saw.
	if _, err := m.PutFile(ctx, scope, project, "run.sh", strings.NewReader("data"), "text/plain", false); err != nil {
		t.Fatal(err)
	}
	man, err := m.Checkout(ctx, scope, project, "main")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range man.Files {
		got[f.Path] = f.Exec
	}
	if !got["run.sh"] || got["notes.txt"] {
		t.Fatalf("checkout manifest exec = %v", got)
	}
	if e, _ := m.Get(ctx, scope, project, "run.sh"); e.Exec {
		t.Fatal("PutFile(exec=false) did not clear the bit")
	}
}
