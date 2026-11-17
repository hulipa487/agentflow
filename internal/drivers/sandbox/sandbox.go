// Package sandbox runs one command in one gVisor sandbox and throws the
// sandbox away.
//
// A job is stateless by construction: the sandbox is created for a single
// command, gets a read-only toolchain root and one read-write workspace
// directory, runs under a wall-clock limit, and is destroyed. Whatever must
// outlive the command is in the workspace directory, which the caller owns.
// Nothing here knows about users, projects or the files store: internal/core
// decides what a job may see, this package only runs it.
package sandbox

import (
	"context"
	"errors"
	"time"
)

// ErrUnavailable wraps every reason a job could not be started at all: no
// Linux, no runsc, a root filesystem that is not there. A caller reports it as
// "the shell is unavailable", never as a failed command.
var ErrUnavailable = errors.New("sandbox unavailable")

// DefaultMount is where the workspace appears inside the sandbox.
const DefaultMount = "/workspace"

// Bind is one extra host directory made visible to a job.
type Bind struct {
	Source   string // absolute host path
	Dest     string // absolute path inside the sandbox
	ReadOnly bool
}

// Limits bound what one job may use. A zero field means "no limit of this
// kind", except OutputBytes and TmpBytes, which fall back to safe defaults:
// an unbounded output buffer or tmpfs is a way to exhaust the host.
type Limits struct {
	OutputBytes int64   // per stream (stdout, stderr)
	TmpBytes    int64   // size of each tmpfs (/tmp and the home directory)
	MemoryBytes int64   // whole sandbox, when the host can enforce it
	CPUs        float64 // whole sandbox, when the host can enforce it
	PIDs        int     // processes inside the sandbox
}

// Spec is one job.
type Spec struct {
	// ID names the sandbox while it exists. It must be unique among running
	// jobs and safe as a file name.
	ID string
	// Command is handed to Shell as its last argument.
	Command string
	// Shell is the argv prefix that runs Command; empty means /bin/sh -c.
	Shell []string
	// Env is the complete environment of the command. PATH and HOME are
	// filled in when absent.
	Env map[string]string
	// Rootfs is the read-only root filesystem (a host directory).
	Rootfs string
	// Workspace is the host directory mounted read-write at Mount.
	Workspace string
	// Mount is the workspace's path inside the sandbox; empty means
	// DefaultMount.
	Mount string
	// WorkDir is the command's working directory, relative to Mount. It must
	// exist in Workspace: gVisor would otherwise create it, and a directory
	// nobody asked for would be written back as if the command had made it.
	WorkDir string
	// Binds are extra directories, mounted after the workspace.
	Binds []Bind
	// Timeout is the wall-clock limit; the sandbox is killed when it passes.
	Timeout time.Duration
	Limits  Limits
	// OnOutput, when set, sees every chunk the command writes as it arrives
	// (stream is "stdout" or "stderr"), before the output cap. A background
	// job uses it to keep a tail that can be read while the job runs. It is
	// called from the goroutines that copy the streams and must not block.
	OnOutput func(stream string, p []byte)
	// Network is the network policy. Only "" and "none" are accepted: the
	// field exists so that adding a mode later changes this package and not
	// every caller.
	Network string
}

// Result is what a finished job reports. A non-zero ExitCode is a result, not
// an error.
type Result struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Duration  time.Duration
	TimedOut  bool // killed by the wall-clock limit
	Truncated bool // a stream passed Limits.OutputBytes and was cut
}

// Runner runs jobs.
type Runner interface {
	// Run blocks until the job ends, the timeout passes or ctx is cancelled.
	// An error means the job did not produce a result; it wraps
	// ErrUnavailable when nothing could be started.
	Run(ctx context.Context, spec Spec) (*Result, error)
	// Available reports why jobs cannot run here, or nil.
	Available() error
}

const (
	defaultOutputBytes = 256 << 10
	defaultTmpBytes    = 256 << 20
)

func (l Limits) outputBytes() int64 {
	if l.OutputBytes > 0 {
		return l.OutputBytes
	}
	return defaultOutputBytes
}

func (l Limits) tmpBytes() int64 {
	if l.TmpBytes > 0 {
		return l.TmpBytes
	}
	return defaultTmpBytes
}
