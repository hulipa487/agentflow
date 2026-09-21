package files

import (
	"context"
	"strings"
	"testing"
)

// Projects enumerates the project names in one scope. A project is not a
// registry entity — it exists because something was written under its name — so
// the test is about the scan, the dedupe, and above all about scopes staying
// separate: a caller must never discover another scope's projects.
func TestProjectsEnumeratesOneScope(t *testing.T) {
	m, _ := testManager(t, 0)
	ctx := context.Background()

	put := func(scope, project, path string) {
		t.Helper()
		if _, err := m.Put(ctx, scope, project, path, strings.NewReader("x"), "text/plain"); err != nil {
			t.Fatalf("put %s/%s/%s: %v", scope, project, path, err)
		}
	}
	put("user:u1", "notes", "a.txt")
	put("user:u1", "notes", "b.txt") // same project, second file: one entry
	put("user:u1", "site", "index.html")
	put("user:u2", "private", "secret.txt")
	put("agent:bot", "workspace", "w.txt")

	got, err := m.Projects(ctx, "user:u1")
	if err != nil {
		t.Fatalf("projects: %v", err)
	}
	want := []string{"notes", "site"}
	if len(got) != len(want) {
		t.Fatalf("projects = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("projects = %v, want %v (sorted, deduped)", got, want)
		}
	}

	// Another user's projects are invisible, and the agent scope is its own.
	if got, _ := m.Projects(ctx, "user:u2"); len(got) != 1 || got[0] != "private" {
		t.Fatalf("u2 projects: %v", got)
	}
	if got, _ := m.Projects(ctx, "agent:bot"); len(got) != 1 || got[0] != "workspace" {
		t.Fatalf("agent projects: %v", got)
	}

	// A scope with nothing in it is empty, not an error.
	if got, err := m.Projects(ctx, "user:nobody"); err != nil || len(got) != 0 {
		t.Fatalf("empty scope: %v err=%v", got, err)
	}
	// A scope is required: guessing one would be worse than failing.
	if _, err := m.Projects(ctx, ""); err == nil {
		t.Fatal("an empty scope must be refused")
	}
}

// A commit alone does not create a project (nothing was written), and a project
// whose files were all deleted disappears — enumeration reflects the working
// tree, which is what a person means by "my projects".
func TestProjectsFollowsTheWorkingTree(t *testing.T) {
	m, _ := testManager(t, 0)
	ctx := context.Background()

	if got, _ := m.Projects(ctx, "user:u1"); len(got) != 0 {
		t.Fatalf("a fresh scope has no projects: %v", got)
	}
	if _, err := m.Commit(ctx, "user:u1", "empty", "main", "x"); err == nil {
		t.Fatal("committing an empty project should refuse (nothing to snapshot)")
	}

	if _, err := m.Put(ctx, "user:u1", "proj", "a.txt", strings.NewReader("x"), "text/plain"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if got, _ := m.Projects(ctx, "user:u1"); len(got) != 1 || got[0] != "proj" {
		t.Fatalf("projects after put: %v", got)
	}

	if err := m.Delete(ctx, "user:u1", "proj", "a.txt"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got, _ := m.Projects(ctx, "user:u1"); len(got) != 0 {
		t.Fatalf("an emptied project should disappear from the listing: %v", got)
	}
}
