# Master plan: one-off gVisor jobs for the shell

Status: phases 0 to 4 implemented on 2026-11-17, uncommitted, on top of `main`
at `7ce6a6f` (agentflow) and `efc9190` (agentflow-studio). Phase 5 is not
started. Section 9 records what was built and where it differs from the plan
below; where they disagree, section 9 and the code are right.

Sections 1 to 4 are the design and its reasons. Section 5 is the work, in
order. Section 6 is the contracts (Lua, config, results). Section 7 records the
owner's answers.

---

## 1. Goal

Replace the persistent Docker container behind `shell.*` with **stateless
gVisor jobs**. A command is queued, runs once in a fresh `runsc` sandbox with a
time limit, and the sandbox is destroyed. What persists is the user's project
in the files store, never the sandbox.

### The design in one paragraph

A job is `{command, environment, project, time limit}`. The engine queues it,
assembles a workspace directory from the project's working tree, starts one
`runsc` sandbox with a read-only toolchain rootfs and the workspace mounted
read-write, enforces a wall-clock limit and output caps, then scans the
workspace and writes changed and deleted files back to the working tree. Any
instance can run any job, because everything a job needs is rebuilt from the
shared store. It resembles a CI runner, except the checkout is the user's live
working tree and the results flow back into it.

### Non-goals

- No long-lived sandbox, no process state between commands, no files that
  persist outside the workspace.
- No networking inside the sandbox in this plan (section 5, phase 5 keeps the
  door open).
- No Docker dependency. `runsc` is called directly.
- No replacement for SSH. SSH stays as the explicit "a real remote machine"
  provider, with its handle semantics.

---

## 2. Ground rules for this repo

- `PreludeVersion` stays 1. Additive Lua API changes do not bump it. Existing
  loops that call `shell.spawn` / `shell.exec` must keep working (section 6.1).
- The working tree has CRLF noise. Commit LF content only, file by file; never
  `git add -A`; never include `third_party/luau`.
- Docs live in `agentflow-studio/docs` and are shared by both Studio apps. A
  Lua API or config change must land in `desktop/internal/lua/api.go` and
  `web/src/lua/api.ts` together (a web test fails when they differ), and in
  `docs/loops-reference.md`, `docs/configuration.md` and `docs/luau/af.d.luau`
  (synced from `api/luau/af.d.luau` with `make docs-sync`).
- gVisor is Linux-only (x86_64 and ARM64, kernel 5.6+). The binary still
  builds and runs on Windows and macOS; there the sandbox reports itself
  unavailable with one clear sentence, the way `builtin:browser` does with no
  account.
- Tests that need `runsc` are integration tests that skip when it is absent,
  like `internal/drivers/browser/integration_test.go`. Everything else is
  tested against a fake runner.

---

## 3. How things work today (read these first)

| File | What it does |
|---|---|
| `internal/drivers/shell/manager.go` | Owns handles per session. Registry, adoption after failover, reclaim sweep. |
| `internal/drivers/shell/docker.go` | `docker run -d … sleep infinity`, then `docker exec` per command. |
| `internal/drivers/shell/ssh.go` | One SSH connection per handle. Cannot be adopted by another instance. |
| `internal/drivers/shell/registry.go` | The durable handle record. |
| `internal/core/caps/shell.go` | The `shell.*` ops. Spawn materialises a checkout by writing files one at a time through the provider. |
| `internal/core/caps/shellstore.go` | Handle records in the runtime row table. |
| `internal/core/tools/builtins.go` | `builtin:shell.exec` / `shell.write` / `shell.destroy`, `builtin:fs.read` / `fs.write`, bound to the agent's shell profile. |
| `internal/core/files/files.go` | The files store: content-addressed blobs, a working tree per scope and project (`Entry` has a per-path `Revision`), commits, refs, scratch. |
| `internal/config/config.go` | `profiles.shell.<name>` (`ShellProfile`), referenced by `agents.<name>.shell`. |
| `internal/vm/prelude.go` | `shell.spawn` / `exec` / `write` / `destroy`. |

Things to know before changing them:

- The handle registry, adoption and the reclaim sweep exist because a Docker
  container outlives the instance that made it. With one-off jobs nothing
  outlives a job, so all of it goes. SSH never supported adoption.
- The supervisor's idle janitor asks `Manager.Owned` before unloading a
  session. With jobs, a session is busy only while a job of its own runs.
- The word "checkout" in the spawn error is matched by a deployment's
  cold-start retry (`TestShellSpawnCheckoutFailureNamesCheckout`). Keep that
  test's contract when the code moves.
- The files store has no file mode. An executable script loses its bit on the
  way through. Phase 2 adds it.

---

## 4. Decisions already made

| # | Decision |
|---|---|
| D1 | `runsc` is invoked directly with a generated OCI bundle. No Docker, no containerd. |
| D2 | "The user's environment" is the project workspace, synced with the files store. Toolchain and packages are **declarative**: a named environment, never something a command installs and expects to find later. |
| D3 | Write-back is automatic: after a job, changed and deleted files go into the project's uncommitted working tree. The loop still decides when to commit. |
| D4 | Jobs are synchronous with a timeout. Long-running background jobs are also supported, on the same queue (phase 4). |
| D5 | Docker is removed. SSH stays as an explicit remote-machine provider. |
| D6 | No networking in the sandbox at first. The job spec and the runner carry a network policy field from day one so adding a mode later changes one place. |

Consequences that follow from these, so they are settled too:

- **One job at a time per project workspace.** Two jobs on the same project
  queue behind each other. This removes most write-back conflicts and lets the
  workspace directory be reused as a cache.
- **Write-back never overwrites a newer file.** A job records each path's
  `Revision` when it starts. On write-back, a path whose revision changed
  meanwhile (the user or another session wrote it) is left as it is and
  reported in `conflicts`.
- **`fs.read` and `fs.write` need no sandbox.** They become files-store
  operations on the bound project.
- **Secrets reach a job only as declared environment variables**, resolved by
  the engine from the agent's credential allow-list, exactly as `http.request`
  auth is today. A command can read what it is given; nothing else.

---

## 5. Work, in order

### Phase 0: spike on real hardware

Nothing is merged from this phase. It answers questions the docs leave open.

1. Install `runsc` (aarch64 release) on the owner's Phytium board (kernel 6.6,
   `/dev/kvm` present, no Docker).
2. Without root, with `--network=none`: can `runsc --rootless run` execute a
   generated OCI bundle, or is only `runsc --rootless do` usable? The docs say
   rootless is "mainly only suitable for `runsc do`" and that `create` is not
   supported.
3. Confirm: read-only rootfs, one read-write bind mount, a tmpfs `/tmp`,
   environment variables, a non-zero exit code, and kill on timeout that leaves
   no process behind.
4. Measure cold start for `true` and for `python3 -c pass` (median of 20).
5. Find what limits apply without cgroups (rlimits for memory, processes and
   file size; tmpfs size).

Exit: a one-page note in the PR description with the commands that worked, the
numbers, and the chosen launch mode. If rootless cannot run a bundle at all,
stop and ask the owner (section 7, Q1).

### Phase 1: the job runner

New package `internal/drivers/sandbox`.

- `Spec`: command, argv shell, env, workdir, rootfs path, workspace path,
  timeout, output limits, `Network` (only `none` is accepted; anything else is
  an error naming the phase that adds it).
- `Runner` interface with two implementations: `runsc` (builds the bundle,
  runs, kills on deadline or context cancel, removes the bundle) and a fake for
  tests.
- `Result`: stdout, stderr, exit code, duration, `timed_out`, `truncated`.
  Output is capped while streaming, never read whole into memory.
- Environments: `Store` resolves a named environment to a local read-only
  rootfs directory. First source is a local path. The second is an OCI image
  reference pulled and flattened in pure Go (pinned by digest in config),
  cached by digest under the sandbox data directory.
- `doctor` gains checks: `runsc` found and its version, kernel 5.6+, user
  namespaces enabled, every declared environment resolvable.

Verify: integration test (skips without `runsc`) runs `echo`, a failing
command, a timeout, an output flood, and a write to the read-only rootfs.

### Phase 2: the workspace

New package `internal/core/workspace`.

- Local blob cache keyed by content handle, filled from the files store.
- `Prepare(scope, project)`: brings the workspace directory in line with the
  working tree. Reuses the directory from the last job on this instance and
  repairs only what differs. Records, per path, the handle, revision, size and
  modification time it wrote.
- `Collect()`: scans the directory after the job. A file whose size or time
  differs is hashed; new and changed files are `Put`, missing ones `Delete`d,
  subject to the revision rule in section 4.
- Limits, all configurable: per-file size (the files store's own), total
  changed bytes and changed-file count per job, ignore patterns. Anything
  skipped is listed in the result with the reason.
- Symlinks that point outside the workspace are not followed and not stored.
- Add `Mode` to `files.Entry` (executable bit only), carried through `Put`,
  commits and checkout.

Verify: unit tests for prepare/collect against a temp directory, including a
conflict, a delete, an oversized file, an ignored directory and an escaping
symlink.

### Phase 3: queue, ops, config, and removing Docker

- Queue in `internal/core/shelljobs`: per-instance concurrency, one running
  job per project, queue timeout, cancel when the turn is cancelled or the
  session stops. Metrics: queued, running, wait time, run time, timeouts.
- Ops and prelude (section 6.1): `shell.run` is new. `shell.spawn` without
  `provider = "ssh"` returns a virtual handle that only remembers its options;
  `shell.exec` on it runs a job; `shell.write` writes to the working tree;
  `shell.destroy` forgets it.
- `builtin:shell.exec` runs a job. `builtin:fs.read` / `fs.write` use the
  files store. `builtin:shell.destroy` stays for SSH and is a no-op otherwise.
- Config (section 6.2): `runtime.sandbox`, `environments`, and
  `profiles.shell.<name>.provider: sandbox | ssh`. `provider: docker` is
  refused at load with a message that names the replacement.
- Delete `docker.go`, `registry.go`, `caps/shellstore.go`, and the adoption and
  sweep paths in `manager.go`. What remains of the manager is SSH handles held
  in memory and reaped with their session.
- Studio (both apps): Lua table entries, docs pages, and the config reference.

Verify: `agentflow loop test` scenario that edits a file with a command and
reads it back with `files.read`; an existing spawn/exec loop runs unchanged;
race detector on the new packages.

### Phase 4: background jobs

- `shell.start(cmd, opts)` returns a job id at once. The result arrives in the
  session inbox as a message (`kind = "shell.result"`), so a loop handles it
  like any other event. `shell.status(id)`, `shell.cancel(id)`,
  `shell.output(id)` for the tail so far.
- A higher time limit than synchronous jobs, set separately in config.
- Job records in the runtime store, so a session that moved to another
  instance can still ask about its job. A job whose instance died is reported
  as failed with "the instance running it was lost"; it is not re-run.
- Write-back happens when the job ends, under the same revision rule. The
  project stays locked for other jobs meanwhile; `shell.run` on a locked
  project waits up to the queue timeout, then fails naming the running job.

### Phase 5 (later, not scheduled)

- Networking: a `proxy` policy where the sandbox's only route out is an
  engine-run proxy that applies `netguard` and the agent's `net.http`
  capability.
- Package lists in an environment, built into a cached layer (needs network at
  build time).
- cgroup limits through a small privileged helper.
- Warm start from a checkpoint, if phase 0's numbers call for it.

---

## 6. Contracts

### 6.1 Lua

```lua
-- New. One command, one sandbox.
local r = shell.run("pytest -q", {
  project = "site",        -- default: the agent's bound project
  env     = { CI = "1" },  -- merged over the environment's own
  timeout = 120,           -- seconds; capped by config
  workdir = "tests",       -- relative to the workspace root
})
-- r = { ok, exit_code, stdout, stderr, duration_ms, timed_out, truncated,
--       changed = { "out/report.xml" }, deleted = {}, conflicts = {}, skipped = {} }

-- Unchanged for existing loops.
local h = shell.spawn({ project = "site" })   -- virtual handle, no sandbox yet
shell.exec(h.id, "make")                       -- runs a job
shell.destroy(h.id)

-- SSH keeps real handles.
local s = shell.spawn({ provider = "ssh" })
```

A non-zero exit code is a completed job (`ok = true`, like `http.request`
with a 500). A job that did not run — queue timeout, sandbox unavailable,
unknown environment, a sandbox killed by a limit — raises, like every other
failed op, so a loop wraps the call in `pcall`.

Behaviour that changes for existing loops, to be stated in the docs: nothing
outside the workspace survives between `shell.exec` calls, and neither do
background processes or shell variables.

### 6.2 Config

```yaml
runtime:
  sandbox:
    runsc: ""              # default: found on PATH
    data_dir: ./data/sandbox
    concurrency: 4
    queue_timeout: 30s
    timeout: 60s           # default per job
    max_timeout: 10m
    background_max_timeout: 2h
    output_limit: 256KiB

environments:
  python:
    image: docker.io/library/python:3.12-slim@sha256:…   # or rootfs: /srv/envs/python
    env: { PYTHONUNBUFFERED: "1" }

profiles:
  shell:
    code:
      provider: sandbox     # or ssh
      environment: python
      network: none         # the only value accepted for now
      ignore: []            # write-back ignore patterns; empty by default
```

A project may carry `.agentflow/shell.yaml` in its working tree to customise
the template its agent's profile names. It can pick another operator-declared
environment, add environment variables, set a default `workdir` and `timeout`
(capped by `max_timeout`) and add `ignore` patterns. It cannot name an image
of its own, change the network policy or raise a limit.

```yaml
# .agentflow/shell.yaml
environment: node
env: { NODE_ENV: test }
workdir: app
timeout: 120s
ignore: ["node_modules/", "dist/"]
```

### 6.3 What a job may touch

| Path in the sandbox | Source | Access |
|---|---|---|
| `/` | The environment's rootfs | Read-only |
| `/workspace` | The project's working tree | Read-write, written back |
| `/tmp` | tmpfs, size-limited | Read-write, discarded |
| Network | none | — |

---

## 7. Questions the owner has answered

| # | Question | Answer |
|---|---|---|
| Q1 | Privileges, if rootless `runsc` is not enough | Run AgentFlow with the extra capabilities it needs. No setuid helper. |
| Q2 | Who declares an environment | The operator declares named templates in the deployment config. A user customises per project, within what the template allows (section 6.2). |
| Q3 | Packages before networking exists | A pinned image per environment is enough for now. |
| Q4 | Ignore patterns for write-back | Empty unless configured. |

---

## 8. Definition of done

- A loop can run a command against a project, see the files it changed with
  `files.read`, and nothing of the sandbox remains afterwards.
- Killing the instance mid-job leaves no process, no sandbox and no handle
  record; the next command on any instance works.
- Docker is not referenced anywhere in the binary, config schema or docs. SSH
  works as before.
- On Windows and macOS, and on Linux without `runsc`, a shell call fails with
  one sentence that says what to install.
- `doctor` reports whether the sandbox is usable and why not.
- Both Studio apps offer the new Lua entries and show the new config
  reference.

---

## 9. What was built

### Phase 0: what the spike found

On the owner's Phytium board (aarch64, kernel 6.6, no Docker, no passwordless
sudo), with gVisor `release-20261109.0`:

- `runsc --rootless --network=none run --bundle=…` runs a generated bundle
  without root. `create` was not needed.
- Start-up is about 30 ms for `true` and 50 ms for a small shell script with
  the default `systrap` platform; `kvm` was slower (75 ms).
- A read-only root, a read-write bind mount, size-limited tmpfs, environment
  variables and exit codes all behave as specified. Files the command creates
  belong to the engine's user on the host.
- Killing the launcher (SIGTERM or SIGKILL) takes every sandbox process with
  it. A state record is left behind, which `runsc delete --force` removes.
- Without cgroups: tmpfs size, `RLIMIT_NPROC` and `RLIMIT_FSIZE` are enforced;
  a memory limit in the OCI spec is ignored. `systemd-run --user --scope -p
  MemoryMax=…` in front of the launcher gives a real kernel limit without
  root, and an over-limit sandbox is killed in about 3 s.
- runsc reports its own failures with exit 128 and a line in `--log`; a
  command's exit code never touches the log. That is how the two are told
  apart.

So Q1 did not need answering in practice: no extra capabilities are required.
Memory and CPU limits ride on a systemd scope where the host has one.

### Where the code is

| Package | What it holds |
|---|---|
| `internal/drivers/sandbox` | `Runner` and the `runsc` implementation; OCI bundle; output caps; environments; a small registry client and layer unpacker |
| `internal/core/workspace` | `Prepare` and `Collect`, ignore patterns |
| `internal/core/shelljobs` | the queue, `Run`, background jobs and their records |
| `internal/core/shellops` | what a shell call means, shared by the Lua ops and the model-facing tools |
| `internal/core/caps/shell.go` | the `shell.*` ops |
| `internal/core/tools/builtins.go` | `builtin:shell.*` and `builtin:fs.*` |
| `internal/config/sandbox.go` | `runtime.sandbox`, `environments`, shell profile validation |
| `cmd/agentflow/doctor_sandbox.go` | the doctor checks |

Removed: `drivers/shell/docker.go`, `drivers/shell/registry.go`,
`caps/shellstore.go`, the adoption and sweep paths of the shell manager, and
the `shell-reclaim` housekeeping job.

### Differences from the plan above

- **A sandbox handle has no record at all.** The plan kept the idea of a
  virtual handle without saying where its options live. They are encoded into
  the handle id, so `shell.spawn` needs no storage and the id works on any
  instance.
- **Write-back also runs when a job fails or times out.** Whatever a command
  did before it ended is stored, so the project never depends on which
  instance a job died on. The exception is an engine that is killed mid-job:
  that job's unwritten work is discarded by the next `Prepare`.
- **Ignored paths are kept on the instance** as a best-effort cache instead of
  being cleaned. Everything else in the directory is brought back in line with
  the tree after every job.
- **A file tool call needs a bound shell profile even when it carries a
  handle.** A sandbox handle is only options and a model can write one out by
  hand; without this check it would have been a way to reach a project with
  neither the `files` nor the `shell.exec` capability.
- **`builtin:fs.read` and `builtin:fs.write` are forbidden together with the
  shell tools** for agents without `shell.exec`, since they now act on the
  same project.
- **`mem_limit` and `cpu_limit` on a spawn call are not carried.** The
  profile's and the deployment's limits apply.

### Not done

- Phase 5: networking, package lists, cgroup limits through a helper, warm
  start.
- A cross-instance lock per project. Two instances can run jobs on one project
  at once; the revision rule keeps them from overwriting each other.
- Running as root has not been exercised (no root on the development host).
  The code passes `--rootless` only when it is not root.
- Private registries and zstd-compressed layers.
- Running a job on a committed snapshot (`ref`), and `scratch_mount` in a
  sandbox. Both are refused with a message.
- An online doctor check that an image's registry is reachable.

