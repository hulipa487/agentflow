package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCappedKeepsTheHeadAndNeverBlocks(t *testing.T) {
	c := &capped{limit: 5}
	for _, chunk := range []string{"abc", "defg", "hij"} {
		if n, err := c.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("Write(%q) = %d, %v; a capped stream must accept everything", chunk, n, err)
		}
	}
	if got := c.String(); got != "abcde" {
		t.Fatalf("kept %q, want the first five bytes", got)
	}
	if !c.wasTruncated() {
		t.Fatal("truncation was not reported")
	}
}

func TestCappedCutsOnACharacterBoundary(t *testing.T) {
	c := &capped{limit: 4}
	_, _ = c.Write([]byte("aé€")) // 1 + 2 + 3 bytes: the limit falls inside €
	if got := c.String(); got != "aé" {
		t.Fatalf("kept %q, want %q", got, "aé")
	}
}

func specFor(t *testing.T) Spec {
	t.Helper()
	root, ws := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "root"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Spec{ID: "job-1", Command: "true", Rootfs: root, Workspace: ws}
}

func TestSpecValidation(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Spec)
		want string
	}{
		{"bad id", func(s *Spec) { s.ID = "a/b" }, "not usable as a name"},
		{"empty command", func(s *Spec) { s.Command = "  " }, "empty command"},
		{"network", func(s *Spec) { s.Network = "host" }, "not supported yet"},
		{"rootfs", func(s *Spec) { s.Rootfs = "/nonexistent/rootfs" }, "root filesystem"},
		{"workspace", func(s *Spec) { s.Workspace = "relative" }, "absolute path"},
		{"mount", func(s *Spec) { s.Mount = "/" }, "mount point"},
		{"workdir escapes", func(s *Spec) { s.WorkDir = "../x" }, "inside the workspace"},
		{"workdir missing", func(s *Spec) { s.WorkDir = "nope" }, "does not exist"},
		{"bind", func(s *Spec) { s.Binds = []Bind{{Source: "rel", Dest: "/x"}} }, "bind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := specFor(t)
			tc.edit(&s)
			err := s.validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validate() = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
	if err := specFor(t).validate(); err != nil {
		t.Fatalf("a plain spec was refused: %v", err)
	}
}

func TestBundleDescribesTheJob(t *testing.T) {
	s := specFor(t)
	if err := os.Mkdir(filepath.Join(s.Workspace, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.Command = "make test"
	s.WorkDir = "app"
	s.Env = map[string]string{"CI": "1", "PATH": "/custom"}
	s.Limits = Limits{PIDs: 64, TmpBytes: 1 << 20}
	s.Binds = []Bind{{Source: "/srv/data", Dest: "/data", ReadOnly: true}}

	dir := filepath.Join(t.TempDir(), "bundle")
	if err := writeBundle(dir, s); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got ociSpec
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if want := []string{"/bin/sh", "-c", "make test"}; strings.Join(got.Process.Args, "|") != strings.Join(want, "|") {
		t.Errorf("args = %q, want %q", got.Process.Args, want)
	}
	if got.Process.Cwd != "/workspace/app" {
		t.Errorf("cwd = %q", got.Process.Cwd)
	}
	if !got.Root.Readonly || got.Root.Path != s.Rootfs {
		t.Errorf("root = %+v, want the rootfs read-only", got.Root)
	}
	if !got.Process.NoNewPrivileges {
		t.Error("noNewPrivileges is off")
	}
	env := strings.Join(got.Process.Env, "|")
	if env != "CI=1|HOME=/root|PATH=/custom" {
		t.Errorf("env = %q: the job's own PATH must win and HOME must be filled in", env)
	}
	mounts := map[string]ociMount{}
	for _, m := range got.Mounts {
		mounts[m.Destination] = m
	}
	if m := mounts["/workspace"]; m.Source != s.Workspace || !contains(m.Options, "rw") {
		t.Errorf("workspace mount = %+v", m)
	}
	if m := mounts["/data"]; m.Source != "/srv/data" || !contains(m.Options, "ro") {
		t.Errorf("extra bind = %+v", m)
	}
	for _, dest := range []string{"/tmp", "/root"} {
		if m := mounts[dest]; m.Type != "tmpfs" || !contains(m.Options, "size=1048576") {
			t.Errorf("%s = %+v, want a size-limited tmpfs", dest, m)
		}
	}
	var nproc uint64
	for _, r := range got.Process.Rlimits {
		if r.Type == "RLIMIT_NPROC" {
			nproc = r.Hard
		}
	}
	if nproc != 64 {
		t.Errorf("RLIMIT_NPROC = %d, want 64", nproc)
	}
	network := false
	for _, ns := range got.Linux.Namespaces {
		network = network || ns.Type == "network"
	}
	if !network {
		t.Error("the job shares the host's network namespace")
	}
}

func TestHomeFallsBackToTmpWithoutARootDirectory(t *testing.T) {
	s := specFor(t)
	if err := os.Remove(filepath.Join(s.Rootfs, "root")); err != nil {
		t.Fatal(err)
	}
	got := s.oci()
	if !contains(got.Process.Env, "HOME=/tmp") {
		t.Errorf("env = %q, want HOME=/tmp", got.Process.Env)
	}
	for _, m := range got.Mounts {
		if m.Destination == "/root" {
			t.Error("mounted a home directory the root filesystem does not have")
		}
	}
}

func TestScopeLimits(t *testing.T) {
	got := strings.Join(scopeLimits(Limits{MemoryBytes: 512 << 20, CPUs: 1.5}), " ")
	if want := "-p MemoryMax=536870912 -p MemorySwapMax=0 -p CPUQuota=150%"; got != want {
		t.Errorf("scopeLimits = %q, want %q", got, want)
	}
	if got := scopeLimits(Limits{}); len(got) != 0 {
		t.Errorf("no limits asked for, got %q", got)
	}
}

func TestLauncherErrorReadsOnlyErrors(t *testing.T) {
	f := filepath.Join(t.TempDir(), "runsc.log")
	lines := `{"msg":"starting","level":"info"}
not json
{"msg":"running container: failed to load /bin/nosh","level":"error"}
{"msg":"later","level":"error"}
`
	if err := os.WriteFile(f, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := launcherError(f); got != "running container: failed to load /bin/nosh" {
		t.Errorf("launcherError = %q", got)
	}
	if got := launcherError(filepath.Join(t.TempDir(), "absent")); got != "" {
		t.Errorf("a missing log reported %q", got)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
