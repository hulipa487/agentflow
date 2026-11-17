package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// ociSpec is the part of an OCI runtime spec a job needs. It is written out
// by hand rather than through the runtime-spec module: a dozen fields do not
// justify a dependency, and every field here is one the spike exercised.
type ociSpec struct {
	Version  string     `json:"ociVersion"`
	Process  ociProcess `json:"process"`
	Root     ociRoot    `json:"root"`
	Hostname string     `json:"hostname"`
	Mounts   []ociMount `json:"mounts"`
	Linux    ociLinux   `json:"linux"`
}

type ociProcess struct {
	Terminal        bool        `json:"terminal"`
	User            ociUser     `json:"user"`
	Args            []string    `json:"args"`
	Env             []string    `json:"env"`
	Cwd             string      `json:"cwd"`
	NoNewPrivileges bool        `json:"noNewPrivileges"`
	Rlimits         []ociRlimit `json:"rlimits,omitempty"`
}

type ociUser struct {
	UID int `json:"uid"`
	GID int `json:"gid"`
}

type ociRlimit struct {
	Type string `json:"type"`
	Hard uint64 `json:"hard"`
	Soft uint64 `json:"soft"`
}

type ociRoot struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

type ociMount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type"`
	Source      string   `json:"source"`
	Options     []string `json:"options,omitempty"`
}

type ociLinux struct {
	Namespaces []ociNamespace `json:"namespaces"`
}

type ociNamespace struct {
	Type string `json:"type"`
}

// validate checks a spec before anything is created for it. Every failure
// here is the caller's mistake, said in the caller's terms.
func (s Spec) validate() error {
	if s.ID == "" || strings.ContainsAny(s.ID, "/\\ \t\n") {
		return fmt.Errorf("sandbox: job id %q is not usable as a name", s.ID)
	}
	if strings.TrimSpace(s.Command) == "" {
		return fmt.Errorf("sandbox: empty command")
	}
	if s.Network != "" && s.Network != "none" {
		return fmt.Errorf("sandbox: network %q is not supported yet: jobs run with no network", s.Network)
	}
	if st, err := os.Stat(s.Rootfs); err != nil || !st.IsDir() {
		return fmt.Errorf("%w: root filesystem %q is not a directory", ErrUnavailable, s.Rootfs)
	}
	if !filepath.IsAbs(s.Workspace) {
		return fmt.Errorf("sandbox: workspace %q must be an absolute path", s.Workspace)
	}
	if st, err := os.Stat(s.Workspace); err != nil || !st.IsDir() {
		return fmt.Errorf("sandbox: workspace %q is not a directory", s.Workspace)
	}
	if mount := s.mount(); !path.IsAbs(mount) || path.Clean(mount) != mount || mount == "/" {
		return fmt.Errorf("sandbox: mount point %q must be a clean absolute path other than /", mount)
	}
	if s.WorkDir != "" {
		if path.IsAbs(s.WorkDir) || !localPath(s.WorkDir) {
			return fmt.Errorf("sandbox: workdir %q must be a path inside the workspace", s.WorkDir)
		}
		if st, err := os.Stat(filepath.Join(s.Workspace, filepath.FromSlash(s.WorkDir))); err != nil || !st.IsDir() {
			return fmt.Errorf("sandbox: workdir %q does not exist in the workspace", s.WorkDir)
		}
	}
	for _, b := range s.Binds {
		if !filepath.IsAbs(b.Source) || !path.IsAbs(b.Dest) || path.Clean(b.Dest) != b.Dest || b.Dest == "/" {
			return fmt.Errorf("sandbox: bind %q -> %q needs two clean absolute paths", b.Source, b.Dest)
		}
	}
	return nil
}

// localPath reports whether a slash path stays inside the directory it is
// relative to.
func localPath(p string) bool {
	clean := path.Clean(p)
	return clean != ".." && !strings.HasPrefix(clean, "../") && !path.IsAbs(clean)
}

func (s Spec) mount() string {
	if s.Mount == "" {
		return DefaultMount
	}
	return s.Mount
}

// oci builds the runtime spec for a job.
func (s Spec) oci() ociSpec {
	mount := s.mount()
	cwd := mount
	if s.WorkDir != "" {
		cwd = path.Join(mount, s.WorkDir)
	}
	shell := s.Shell
	if len(shell) == 0 {
		shell = []string{"/bin/sh", "-c"}
	}
	tmp := fmt.Sprintf("size=%d", s.Limits.tmpBytes())

	// The root is read-only, so the command needs somewhere of its own to
	// write: /tmp, and a home directory for the many tools that keep a cache
	// or a history file there. Both are memory-backed and die with the job.
	home := "/root"
	if st, err := os.Stat(filepath.Join(s.Rootfs, "root")); err != nil || !st.IsDir() {
		home = "/tmp"
	}
	mounts := []ociMount{
		{Destination: "/proc", Type: "proc", Source: "proc"},
		{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", tmp}},
	}
	if home != "/tmp" {
		mounts = append(mounts, ociMount{Destination: home, Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", tmp}})
	}
	mounts = append(mounts, ociMount{Destination: mount, Type: "bind", Source: s.Workspace, Options: []string{"rbind", "rw", "nosuid", "nodev"}})
	for _, b := range s.Binds {
		access := "rw"
		if b.ReadOnly {
			access = "ro"
		}
		mounts = append(mounts, ociMount{Destination: b.Dest, Type: "bind", Source: b.Source, Options: []string{"rbind", access, "nosuid", "nodev"}})
	}

	env := map[string]string{"PATH": defaultPath, "HOME": home}
	for k, v := range s.Env {
		env[k] = v
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	envList := make([]string, 0, len(keys))
	for _, k := range keys {
		envList = append(envList, k+"="+env[k])
	}

	// A core dump of a sandboxed process is of no use to anyone and would land
	// in the workspace. The process limit is the one resource bound that holds
	// without host support.
	rlimits := []ociRlimit{{Type: "RLIMIT_CORE", Hard: 0, Soft: 0}}
	if s.Limits.PIDs > 0 {
		n := uint64(s.Limits.PIDs)
		rlimits = append(rlimits, ociRlimit{Type: "RLIMIT_NPROC", Hard: n, Soft: n})
	}

	return ociSpec{
		Version: "1.0.2",
		Process: ociProcess{
			// uid 0 inside the sandbox is the unprivileged host user outside
			// it, and holds no capabilities: no bounding set is granted.
			User:            ociUser{UID: 0, GID: 0},
			Args:            append(append([]string{}, shell...), s.Command),
			Env:             envList,
			Cwd:             cwd,
			NoNewPrivileges: true,
			Rlimits:         rlimits,
		},
		Root:     ociRoot{Path: s.Rootfs, Readonly: true},
		Hostname: "sandbox",
		Mounts:   mounts,
		Linux: ociLinux{Namespaces: []ociNamespace{
			{Type: "pid"}, {Type: "ipc"}, {Type: "uts"}, {Type: "mount"}, {Type: "network"},
		}},
	}
}

// writeBundle creates the bundle directory for a job.
func writeBundle(dir string, s Spec) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.oci(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600)
}
