package shellops

import (
	"context"
	"strings"
	"testing"
	"time"

	"agentflow/internal/core/session"
)

func TestHandleIsItsOptions(t *testing.T) {
	in := Options{Project: "site", Environment: "node", WorkDir: "app", Mount: "/work", Env: map[string]string{"A": "1"}}
	id, err := in.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !IsSandboxHandle(id) || IsSandboxHandle("ssh-1a2b3c4d") {
		t.Fatalf("IsSandboxHandle misjudged %q or an SSH id", id)
	}
	out, err := DecodeHandle(id)
	if err != nil {
		t.Fatal(err)
	}
	if out.Project != in.Project || out.Environment != in.Environment || out.WorkDir != in.WorkDir || out.Mount != in.Mount || out.Env["A"] != "1" {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
	for _, bad := range []string{"sandbox:***", "sandbox:" + "bm90IGpzb24", "ssh-1", ""} {
		if _, err := DecodeHandle(bad); err == nil {
			t.Errorf("DecodeHandle(%q) was accepted", bad)
		}
	}
	big := Options{Env: map[string]string{"X": strings.Repeat("x", maxHandle)}}
	if _, err := big.Encode(); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("an oversized handle gave %v", err)
	}
}

func TestWorkspacePath(t *testing.T) {
	cases := []struct {
		opts Options
		in   string
		want string // "" = refused
	}{
		{Options{}, "src/a.py", "src/a.py"},
		{Options{}, "./src/../a.py", "a.py"},
		{Options{}, "/workspace/src/a.py", "src/a.py"},
		{Options{Mount: "/work"}, "/work/a.py", "a.py"},
		{Options{Mount: "/work"}, "/workspace/a.py", ""},
		{Options{WorkDir: "app"}, "main.js", "app/main.js"},
		{Options{WorkDir: "app"}, "/workspace/main.js", "main.js"},
		{Options{}, "/etc/passwd", ""},
		{Options{}, "/workspaces/x", ""}, // a prefix of the name is not the mount
		{Options{}, "../up", ""},
		{Options{}, "/workspace", ""},
		{Options{}, "  ", ""},
	}
	for _, c := range cases {
		got, err := WorkspacePath(c.opts, c.in)
		if (c.want == "") != (err != nil) || got != c.want {
			t.Errorf("WorkspacePath(%+v, %q) = %q, %v; want %q", c.opts, c.in, got, err, c.want)
		}
	}
}

func TestWorkspaceDir(t *testing.T) {
	cases := []struct{ mount, in, want string }{
		{"", "", ""},
		{"", "app", "app"},
		{"", "/workspace", ""},
		{"", "/workspace/app/src", "app/src"},
		{"/work", "/work", ""},
		{"/work", "/work/app", "app"},
		{"", ".", ""},
	}
	for _, c := range cases {
		if got, err := WorkspaceDir(c.mount, c.in); err != nil || got != c.want {
			t.Errorf("WorkspaceDir(%q, %q) = %q, %v; want %q", c.mount, c.in, got, err, c.want)
		}
	}
	for _, bad := range []struct{ mount, in string }{{"", "/etc"}, {"", "../x"}, {"/work", "/workspace"}} {
		if _, err := WorkspaceDir(bad.mount, bad.in); err == nil {
			t.Errorf("WorkspaceDir(%q, %q) was accepted", bad.mount, bad.in)
		}
	}
}

func TestProfileAndScopeComeFromTheContext(t *testing.T) {
	if _, _, _, ok := Profile(context.Background()); ok {
		t.Fatal("a context with no shell profile reported one")
	}
	ctx := session.WithShell(context.Background(), map[string]any{
		"environment": "python", "project": "site", "workdir": "app", "network": "none",
		"timeout": 90 * time.Second, "memory_bytes": int64(512 << 20), "cpu_limit": 1.5,
		"env": map[string]string{"CI": "1"}, "ignore": []string{"dist/"},
	})
	provider, p, _, ok := Profile(ctx)
	if !ok || provider != Sandbox {
		t.Fatalf("provider = %q, ok = %v; an unnamed provider is the sandbox", provider, ok)
	}
	if p.Environment != "python" || p.Project != "site" || p.WorkDir != "app" || p.Timeout != 90*time.Second ||
		p.MemoryBytes != 512<<20 || p.CPUs != 1.5 || p.Env["CI"] != "1" || len(p.Ignore) != 1 {
		t.Errorf("profile = %+v", p)
	}
	if _, ok := SSHProfile(context.Background()); ok {
		t.Fatal("a context with no SSH profile reported one")
	}
	sshCtx := session.WithSSH(ctx, map[string]any{
		"host": "box:22", "user": "dev", "password": "pw", "key_file": "/id",
		"workdir": "/srv/work", "env": map[string]string{"FOO": "bar"},
	})
	opts, ok := SSHProfile(sshCtx)
	if !ok || opts.Host != "box:22" || opts.User != "dev" || opts.Password != "pw" ||
		opts.KeyFile != "/id" || opts.WorkDir != "/srv/work" || opts.Env["FOO"] != "bar" {
		t.Errorf("SSHProfile = ok:%v %+v", ok, opts)
	}

	if got := Scope(context.Background(), "coder"); got != "agent:coder" {
		t.Errorf("scope without a user = %q", got)
	}
	if got := Scope(session.WithPersonalIdentity(context.Background(), "u1"), "coder"); got != "user:u1" {
		t.Errorf("scope with a user = %q", got)
	}
	if AgentOf("coder|local:user:x") != "coder" {
		t.Error("AgentOf did not split the session key")
	}
}
