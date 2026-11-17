package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agentflow/internal/config"
	"agentflow/internal/drivers/sandbox"
)

// doctorSandbox reports whether shell jobs can run on this host, and what
// each declared environment needs before its first job.
//
// A sandbox that cannot run is a warning, not a failure: the deployment still
// boots and every other feature works, and a config written for a Linux fleet
// is routinely checked on a laptop that is not one. The message says exactly
// what a shell call would say.
func doctorSandbox(cfg *config.Config, add func(status, check, detail string)) {
	inUse := sandboxInUse(cfg)
	if !inUse && len(cfg.Environments) == 0 {
		return
	}

	// Probed in a scratch directory: the real data directory may belong to a
	// running engine, and a probe there would clear that engine's job state.
	scratch, err := os.MkdirTemp("", "agentflow-doctor-sandbox-")
	if err != nil {
		add("WARN", "sandbox", "could not create a scratch directory to probe in: "+err.Error())
		return
	}
	defer os.RemoveAll(scratch)
	runner := sandbox.NewRunsc(cfg.Runtime.Sandbox.Runsc, scratch, nil)
	if err := runner.Available(); err != nil {
		detail := strings.TrimPrefix(err.Error(), sandbox.ErrUnavailable.Error()+": ")
		if inUse {
			add("WARN", "sandbox", "shell jobs cannot run on this host: "+detail)
		} else {
			add("SKIP", "sandbox", "not usable on this host ("+detail+"); no agent has a sandbox shell profile")
		}
	} else {
		mode := "rootless"
		if os.Geteuid() == 0 {
			mode = "as root"
		}
		add("PASS", "sandbox", fmt.Sprintf("%s, %s", runner.Version(), mode))
		if enforced, note := runner.Limiting(); enforced {
			add("PASS", "sandbox limits", note)
		} else {
			add("WARN", "sandbox limits", note+". The process count, tmpfs sizes, time limit and output cap still apply")
		}
	}

	names := make([]string, 0, len(cfg.Environments))
	for name := range cfg.Environments {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		e := cfg.Environments[name]
		check := "environment " + name
		if e.Rootfs != "" {
			if st, err := os.Stat(e.Rootfs); err != nil || !st.IsDir() {
				add("WARN", check, fmt.Sprintf("root filesystem %s is not a directory on this host", e.Rootfs))
			} else if _, err := os.Lstat(filepath.Join(e.Rootfs, "bin", "sh")); err != nil {
				add("WARN", check, fmt.Sprintf("root filesystem %s has no /bin/sh; commands run through it", e.Rootfs))
			} else {
				add("PASS", check, "root filesystem "+e.Rootfs)
			}
			continue
		}
		ref, err := sandbox.ParseImageRef(e.Image)
		if err != nil {
			add("WARN", check, err.Error())
			continue
		}
		cached := filepath.Join(cfg.SandboxDataDir(), "images", strings.TrimPrefix(ref.Digest, "sha256:"), "env.json")
		if _, err := os.Stat(cached); err == nil {
			add("PASS", check, fmt.Sprintf("%s/%s, already unpacked on this host", ref.Registry, ref.Repo))
		} else {
			add("PASS", check, fmt.Sprintf("%s/%s, pulled on its first use (the first job in it waits for the download)", ref.Registry, ref.Repo))
		}
	}
}
