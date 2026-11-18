package workspace

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"agentflow/internal/core/files"
	"agentflow/internal/core/media"
	"agentflow/internal/core/runtime"
)

const (
	scope   = "user:u1"
	project = "site"
)

type fixture struct {
	t  *testing.T
	fm *files.Manager
	m  *Manager
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	blobs, err := media.Open(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	rt, err := runtime.OpenSQLite(filepath.Join(t.TempDir(), "rt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close() })
	fm := files.New(blobs, rt, time.Hour, 1<<20, nil2())
	return &fixture{t: t, fm: fm, m: New(filepath.Join(t.TempDir(), "ws"), fm, nil)}
}

func (f *fixture) put(path, content string) *files.Entry {
	f.t.Helper()
	e, err := f.fm.Put(context.Background(), scope, project, path, strings.NewReader(content), "text/plain")
	if err != nil {
		f.t.Fatal(err)
	}
	return e
}

func (f *fixture) prepare(ignore ...string) *Workspace {
	f.t.Helper()
	w, err := f.m.Prepare(context.Background(), scope, project, NewIgnore(ignore))
	if err != nil {
		f.t.Fatal(err)
	}
	return w
}

func (f *fixture) collect(w *Workspace, lim Limits) *Changes {
	f.t.Helper()
	ch, err := w.Collect(context.Background(), lim)
	if err != nil {
		f.t.Fatal(err)
	}
	return ch
}

// stored reads a path's content from the files store, "" when absent.
func (f *fixture) stored(path string) string {
	f.t.Helper()
	e, err := f.fm.Get(context.Background(), scope, project, path)
	if err != nil {
		return ""
	}
	b, err := f.fm.ReadBlob(context.Background(), e.Handle, 1<<20)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func (f *fixture) tree() []string {
	f.t.Helper()
	entries, err := f.fm.List(context.Background(), scope, project, "")
	if err != nil {
		f.t.Fatal(err)
	}
	out := []string{}
	for _, e := range entries {
		out = append(out, e.Path)
	}
	return out
}

func write(t *testing.T, w *Workspace, rel, content string) {
	t.Helper()
	full := filepath.Join(w.Dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func onDisk(t *testing.T, w *Workspace) []string {
	t.Helper()
	out := []string{}
	err := filepath.Walk(w.Dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || p == w.Dir {
			return err
		}
		rel, _ := filepath.Rel(w.Dir, p)
		if info.IsDir() {
			rel += "/"
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func same(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestPrepareMirrorsTheTree(t *testing.T) {
	f := newFixture(t)
	f.put("README.md", "# site")
	f.put("src/app/main.py", "print(1)")
	w := f.prepare()

	same(t, "directory", onDisk(t, w), []string{"README.md", "src/", "src/app/", "src/app/main.py"})
	b, _ := os.ReadFile(filepath.Join(w.Dir, "src/app/main.py"))
	if string(b) != "print(1)" {
		t.Errorf("content = %q", b)
	}
	if !w.Has("src/app") || !w.Has("") || w.Has("nope") || w.Has("../x") || w.Has("README.md") {
		t.Error("Has does not tell directories in the workspace from everything else")
	}
}

func TestPrepareRewritesOnlyWhatDiffers(t *testing.T) {
	f := newFixture(t)
	f.put("keep.txt", "same")
	f.put("change.txt", "v1")
	f.put("gone.txt", "bye")
	w := f.prepare()
	keep := filepath.Join(w.Dir, "keep.txt")
	before, _ := os.Stat(keep)

	f.put("change.txt", "v2")
	f.put("new.txt", "hi")
	if err := f.fm.Delete(context.Background(), scope, project, "gone.txt"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	w = f.prepare()

	same(t, "directory", onDisk(t, w), []string{"change.txt", "keep.txt", "new.txt"})
	if b, _ := os.ReadFile(filepath.Join(w.Dir, "change.txt")); string(b) != "v2" {
		t.Errorf("change.txt = %q", b)
	}
	after, _ := os.Stat(keep)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("an unchanged file was rewritten")
	}
}

func TestPrepareRepairsADirectoryThatDrifted(t *testing.T) {
	f := newFixture(t)
	f.put("a.txt", "stored")
	f.put("dir/b.txt", "stored")
	w := f.prepare()
	// Between jobs: one file edited by hand, one replaced by a directory, a
	// stray file, and a link where a directory belongs.
	write(t, w, "a.txt", "tampered!")
	write(t, w, "stray.txt", "x")
	if err := os.RemoveAll(filepath.Join(w.Dir, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(w.Dir, "dir")); err != nil {
		t.Fatal(err)
	}
	w = f.prepare()

	same(t, "directory", onDisk(t, w), []string{"a.txt", "dir/", "dir/b.txt"})
	if b, _ := os.ReadFile(filepath.Join(w.Dir, "a.txt")); string(b) != "stored" {
		t.Errorf("a.txt = %q, want the stored content back", b)
	}
	if _, err := os.Stat("/etc/b.txt"); err == nil {
		t.Fatal("a file was written through a link out of the workspace")
	}
}

func TestCollectWritesChangesBack(t *testing.T) {
	f := newFixture(t)
	f.put("edit.txt", "v1")
	f.put("remove.txt", "bye")
	f.put("same.txt", "same")
	f.put("touched.txt", "same")
	w := f.prepare()

	write(t, w, "edit.txt", "v2 longer")
	write(t, w, "out/report.xml", "<ok/>")
	write(t, w, "touched.txt", "same") // rewritten with identical content
	if err := os.Remove(filepath.Join(w.Dir, "remove.txt")); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(w.Dir, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	touched, _ := f.fm.Get(context.Background(), scope, project, "touched.txt")

	ch := f.collect(w, Limits{})
	same(t, "changed", ch.Changed, []string{"edit.txt", "out/report.xml", "run.sh"})
	same(t, "deleted", ch.Deleted, []string{"remove.txt"})
	same(t, "conflicts", ch.Conflicts, nil)
	if len(ch.Skipped) != 0 {
		t.Errorf("skipped = %v", ch.Skipped)
	}
	same(t, "tree", f.tree(), []string{"edit.txt", "out/report.xml", "run.sh", "same.txt", "touched.txt"})
	if got := f.stored("edit.txt"); got != "v2 longer" {
		t.Errorf("edit.txt in the store = %q", got)
	}
	// A file created executable by a job: only a filesystem with mode bits
	// can say so. TestCollectKeepsAStoredExecutableBit covers every platform.
	if e, _ := f.fm.Get(context.Background(), scope, project, "run.sh"); modeBits && (e == nil || !e.Exec) {
		t.Errorf("run.sh lost its executable bit: %+v", e)
	}
	if e, _ := f.fm.Get(context.Background(), scope, project, "touched.txt"); e.Revision != touched.Revision {
		t.Error("a file rewritten with the same content got a new revision")
	}

	// A second job with no changes writes nothing.
	w = f.prepare()
	if st, _ := os.Stat(filepath.Join(w.Dir, "run.sh")); modeBits && st.Mode().Perm()&0o111 == 0 {
		t.Error("run.sh came back without its executable bit")
	}
	ch = f.collect(w, Limits{})
	if len(ch.Changed)+len(ch.Deleted)+len(ch.Conflicts)+len(ch.Skipped) != 0 {
		t.Errorf("an idle job reported changes: %+v", ch)
	}
}

// A script the store knows is executable stays executable when a job rewrites
// it, on every platform: where the filesystem has mode bits the rewrite keeps
// the file's mode, and where it has none the stored bit stands.
func TestCollectKeepsAStoredExecutableBit(t *testing.T) {
	f := newFixture(t)
	if _, err := f.fm.PutFile(context.Background(), scope, project, "build.sh", strings.NewReader("#!/bin/sh\n"), "text/x-sh", true); err != nil {
		t.Fatal(err)
	}
	f.put("notes.txt", "plain")
	w := f.prepare()
	write(t, w, "build.sh", "#!/bin/sh\necho v2\n")
	write(t, w, "notes.txt", "plain, edited")

	ch := f.collect(w, Limits{})
	same(t, "changed", ch.Changed, []string{"build.sh", "notes.txt"})
	if e, _ := f.fm.Get(context.Background(), scope, project, "build.sh"); e == nil || !e.Exec {
		t.Fatalf("build.sh lost its executable bit: %+v", e)
	}
	if e, _ := f.fm.Get(context.Background(), scope, project, "notes.txt"); e == nil || e.Exec {
		t.Fatalf("notes.txt became executable: %+v", e)
	}
	// And a job that does not touch it does not report it.
	ch = f.collect(f.prepare(), Limits{})
	if len(ch.Changed) != 0 {
		t.Fatalf("an untouched executable was reported changed: %v", ch.Changed)
	}
}

func TestCollectNeverOverwritesANewerFile(t *testing.T) {
	f := newFixture(t)
	f.put("both.txt", "base")
	f.put("deleted-by-job.txt", "base")
	f.put("deleted-by-user.txt", "base")
	w := f.prepare()

	// The job's edits...
	write(t, w, "both.txt", "from the job")
	write(t, w, "created.txt", "from the job")
	write(t, w, "deleted-by-user.txt", "from the job")
	if err := os.Remove(filepath.Join(w.Dir, "deleted-by-job.txt")); err != nil {
		t.Fatal(err)
	}
	// ...and what the user did while it ran.
	f.put("both.txt", "from the user")
	f.put("created.txt", "from the user")
	f.put("deleted-by-job.txt", "from the user")
	if err := f.fm.Delete(context.Background(), scope, project, "deleted-by-user.txt"); err != nil {
		t.Fatal(err)
	}

	ch := f.collect(w, Limits{})
	same(t, "conflicts", ch.Conflicts, []string{"both.txt", "created.txt", "deleted-by-job.txt", "deleted-by-user.txt"})
	same(t, "changed", ch.Changed, nil)
	same(t, "deleted", ch.Deleted, nil)
	for _, p := range []string{"both.txt", "created.txt", "deleted-by-job.txt"} {
		if got := f.stored(p); got != "from the user" {
			t.Errorf("%s in the store = %q, want the user's version", p, got)
		}
	}
	if got := f.stored("deleted-by-user.txt"); got != "" {
		t.Errorf("a file the user deleted came back: %q", got)
	}

	// The next job starts from the store's versions, not the losing ones.
	w = f.prepare()
	same(t, "directory", onDisk(t, w), []string{"both.txt", "created.txt", "deleted-by-job.txt"})
	if b, _ := os.ReadFile(filepath.Join(w.Dir, "both.txt")); string(b) != "from the user" {
		t.Errorf("both.txt = %q", b)
	}
}

func TestCollectSkipsWhatCannotBeStoredAndRemovesIt(t *testing.T) {
	f := newFixture(t)
	f.put("keep.txt", "k")
	w := f.prepare()
	write(t, w, "huge.bin", strings.Repeat("x", 2<<20)) // over the 1 MiB per-file limit
	want := map[string]string{"huge.bin": "per-file limit", "link": "not a regular file"}
	if goruntime.GOOS != "windows" {
		// A name Windows will not create in the first place.
		write(t, w, "a|b.txt", "pipe")
		want["a|b.txt"] = "does not allow"
	}
	write(t, w, "ok.txt", "fine")
	if err := os.Symlink("/etc/passwd", filepath.Join(w.Dir, "link")); err != nil {
		t.Fatal(err)
	}

	ch := f.collect(w, Limits{})
	same(t, "changed", ch.Changed, []string{"ok.txt"})
	reasons := map[string]string{}
	for _, s := range ch.Skipped {
		reasons[s.Path] = s.Reason
	}
	for path, want := range want {
		if !strings.Contains(reasons[path], want) {
			t.Errorf("skip reason for %s = %q, want it to mention %q", path, reasons[path], want)
		}
	}
	same(t, "directory", onDisk(t, w), []string{"keep.txt", "ok.txt"})
	same(t, "tree", f.tree(), []string{"keep.txt", "ok.txt"})
}

func TestCollectHonoursTheJobLimits(t *testing.T) {
	f := newFixture(t)
	w := f.prepare()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		write(t, w, name, "12345")
	}
	ch := f.collect(w, Limits{MaxFiles: 2})
	same(t, "changed", ch.Changed, []string{"a.txt", "b.txt"})
	if len(ch.Skipped) != 1 || ch.Skipped[0].Path != "c.txt" || !strings.Contains(ch.Skipped[0].Reason, "limit") {
		t.Errorf("skipped = %+v", ch.Skipped)
	}

	w = f.prepare()
	write(t, w, "d.txt", "12345")
	write(t, w, "e.txt", "12345")
	ch = f.collect(w, Limits{MaxBytes: 8})
	same(t, "changed", ch.Changed, []string{"d.txt"})
	same(t, "tree", f.tree(), []string{"a.txt", "b.txt", "d.txt"})
}

func TestIgnoredPathsAreNeitherStoredNorCleaned(t *testing.T) {
	f := newFixture(t)
	f.put("src/main.js", "1")
	ignore := []string{"node_modules/", "*.log", "build/out"}
	w := f.prepare(ignore...)
	write(t, w, "node_modules/left-pad/index.js", "dep")
	write(t, w, "src/node_modules/x.js", "dep")
	write(t, w, "debug.log", "noise")
	write(t, w, "build/out/app.js", "built")
	write(t, w, "build/keep.js", "kept")

	ch := f.collect(w, Limits{})
	same(t, "changed", ch.Changed, []string{"build/keep.js"})
	same(t, "tree", f.tree(), []string{"build/keep.js", "src/main.js"})

	// The cache survives to the next job on this instance.
	w = f.prepare(ignore...)
	for _, p := range []string{"node_modules/left-pad/index.js", "src/node_modules/x.js", "debug.log", "build/out/app.js"} {
		if _, err := os.Stat(filepath.Join(w.Dir, p)); err != nil {
			t.Errorf("ignored path %s was cleaned up", p)
		}
	}
}

func TestIgnoreMatching(t *testing.T) {
	ig := NewIgnore([]string{"node_modules/", "*.pyc", "build/out", "dist/", " ", "[bad"})
	cases := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"node_modules", true, true},
		{"node_modules", false, false}, // a file with a directory-only pattern's name
		{"a/node_modules/b.js", false, true},
		{"a/b.pyc", false, true},
		{"build/out", true, true},
		{"build/out/x", false, true},
		{"x/build/out", true, false}, // anchored at the root
		{"dist/app.js", false, true},
		{"src/main.py", false, false},
	}
	for _, c := range cases {
		if got := ig.Match(c.path, c.isDir); got != c.want {
			t.Errorf("Match(%q, dir=%v) = %v, want %v", c.path, c.isDir, got, c.want)
		}
	}
	if NewIgnore(nil).Match("anything", false) {
		t.Error("an empty ignore list matched")
	}
}

func TestAFileShadowedByADirectoryIsLeftAlone(t *testing.T) {
	f := newFixture(t)
	f.put("a", "file")
	f.put("a/b", "nested")
	w := f.prepare()
	same(t, "directory", onDisk(t, w), []string{"a/", "a/b"})
	ch := f.collect(w, Limits{})
	if len(ch.Deleted) != 0 {
		t.Errorf("a file that could not be laid out was deleted from the store: %v", ch.Deleted)
	}
	same(t, "tree", f.tree(), []string{"a", "a/b"})
}

func TestWorkspacesAreSeparatePerScopeAndProject(t *testing.T) {
	keys := map[string]bool{}
	for _, k := range [][2]string{{"user:a", "p"}, {"user:b", "p"}, {"user:a", "q"}, {"user:a|p", ""}, {"user:a", "|p"}} {
		keys[Key(k[0], k[1])] = true
	}
	if len(keys) != 5 {
		t.Errorf("only %d distinct keys for 5 scope/project pairs", len(keys))
	}
}

// A job controls every name in the directory, including the one Prepare uses
// for its temporary file. A link planted there must not redirect the write.
func TestPrepareDoesNotWriteThroughPlantedLinks(t *testing.T) {
	f := newFixture(t)
	f.put("a.txt", "v1")
	f.put("deep/b.txt", "v1")
	w := f.prepare()

	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(w.Dir, "a.txt.af-tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(w.Dir, "deep")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(w.Dir, "deep")); err != nil {
		t.Fatal(err)
	}
	f.put("a.txt", "v2")
	f.put("deep/b.txt", "v2")
	w = f.prepare()

	if b, _ := os.ReadFile(victim); string(b) != "untouched" {
		t.Fatalf("a planted link redirected a write: the file outside now holds %q", b)
	}
	if left, _ := os.ReadDir(outside); len(left) != 1 {
		t.Fatalf("files were created outside the workspace: %v", left)
	}
	for _, p := range []string{"a.txt", "deep/b.txt"} {
		if b, _ := os.ReadFile(filepath.Join(w.Dir, p)); string(b) != "v2" {
			t.Errorf("%s = %q, want v2", p, b)
		}
	}
	same(t, "directory", onDisk(t, w), []string{"a.txt", "deep/", "deep/b.txt"})
}
