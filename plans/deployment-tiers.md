# AgentFlow deployment tiers: implementation plan and handoff

Status: approved design, not started. Written 2026-11-07 for an engineer or
LLM picking the work up cold. Read sections 1–4 before writing code.

---

## 0. What this is and how to use it

AgentFlow can already run as a fleet: instances coordinate through rows in a
shared SQL store (leases, a session inbox). But whether a deployment is
"single instance" or "a fleet" is implied by a dozen independent settings, and
a fleet can be misconfigured without any error (local blobs, per-instance
SQLite memory, per-boot admin tokens). There are also real correctness gaps in
the fleet code (section 4).

The goal is to make topology **explicit**: one setting picks a tier, the tier
fills in defaults, boot refuses configurations the tier cannot honour, and the
fleet code is fixed so the tier's promises are true.

How to use this document:

- Section 1 is decided. Do not re-litigate it; if something there turns out to
  be impossible, stop and ask the owner.
- Section 2 is how to work in these repos without tripping over their quirks.
- Section 3 explains the current code you will be changing, with file
  references. Line numbers drift; **find code by the symbol names given**.
- Section 4 lists verified gaps. Each phase fixes some of them.
- Sections 5–6 are the design and the phased task list, each phase with
  acceptance criteria. Do the phases in order: 0, 1, 1.5, 2, 4. (There is no
  phase 3: it was the dropped global tier.)
- Section 9 lists assumptions to confirm with the owner before the phase that
  needs them.

---

## 1. Decisions already made

1. **Two tiers only: `single` and `regional`.**
   - `single`: one AgentFlow process.
   - `regional`: several instances across availability zones (AZs) in one
     region, sharing one PostgreSQL.
2. **There is no global tier.** A multi-region deployment is one independent
   `regional` cluster per region. The application in front (web app, SaaS,
   overlay) decides which region each user belongs to and routes them there.
   Regions share nothing at runtime. Rationale: each region is its own failure
   domain (a region outage stops only that region's users), there is no
   cross-region latency on the hot path, and data residency comes for free.
   The previously considered global tier (one cross-region Postgres primary,
   read replicas, trigger scopes, home-region placement) was dropped.
3. **Supported backends per tier:**

   | Backend | `single` | `regional` |
   |---|---|---|
   | Runtime/identity/credentials/lease/inbox store | SQLite or PostgreSQL | PostgreSQL only |
   | Memory kv: `sqlite` | yes | refused |
   | Memory kv: `postgres` | yes | yes |
   | Vectors: `pgvector` (and SQLite vectors, phase 2) | yes | pgvector yes |
   | Vectors: `qdrant` | yes (split mode) | yes |
   | Vectors: `redisvector` | yes | yes |
   | `redis` (Lua memory store) | instance-local | one region-wide cluster |
   | `volatile` (Lua memory store) | instance-local | instance-local |
   | Blobs (`media`, `files`) | local dir or S3 | S3 only |
   | Log plane directory | allowed | refused |

   Supported deployment shapes in `single`:
   - "Single-database mode": SQLite (+ vectors, phase 2) or Postgres + pgvector.
   - "Split mode": SQLite or Postgres for kv, plus Qdrant (or redisvector) for vectors.

4. **Redis and volatile are Lua memory stores, not a cache layer.** AgentFlow
   has no cache tier and this plan does not add one. Leases and the inbox stay
   in SQL (they need durable atomic updates).
5. **Locality is explicit.** Every memory store has a locality the engine
   derives from its backend and the tier (`deployment`, `region`, `instance`).
   Lua can read it, and boot refuses putting durable conversation stores on
   local backends unless the store opts in.
6. **`runtime.cluster.persistence` is deprecated.** It existed only so regions
   could share one lease/inbox store across regions. Leases and the inbox now
   always live in the runtime store.

---

## 2. Repository context and working conventions

### 2.1 Repositories

- `agentflow` (`/home/hulipa/Documents/Projects/agentflow`): the runtime,
  module `agentflow`, Go 1.26.
  - Branches as of writing:
    - `main`
    - `docs-drift`: one docs commit on top of `main`.
    - `fix-tui-terminal-restore`: two TUI fixes on top of `docs-drift`.
  - Nothing is pushed. **Start this work on a new branch.**
  - Ask the owner which base to use; `fix-tui-terminal-restore` contains
    everything so far.
- `agentflow-studio` (sibling directory): the Fyne desktop app, module
  `agentflow-studio`, Go 1.27, branch `studio-ops-screens`.
  - **All user-facing documentation lives here** in `docs/*.md`, embedded in
    the app. The runtime repo has no docs directory, and `/docs` on the
    runtime answers 410.

### 2.2 Build and test

- The runtime links Luau through cgo: `make` builds
  `third_party/luau` → `internal/vm/lib/libluau.a`, then the binary.
  - `make clean && make` rebuilds for the host architecture.
  - The checked-in `libluau.a` may be for another architecture; if linking
    fails with "file format not recognized", rebuild it.
- Packages without cgo (most of `internal/core/*`, `internal/config`,
  `internal/tui`) test fine with `CGO_ENABLED=0 go test ./internal/...`.
  `go vet ./cmd/agentflow` works without linking.
- Backend integration tests are opt-in via environment variables (see the
  existing tests in `internal/core/runtime/backend_test.go`,
  `internal/core/memory/contract_test.go`):
  - `AGENTFLOW_TEST_POSTGRES` (a DSN)
  - `AF_REDIS_URL`
  - `AF_QDRANT_URL`

  Write new multi-instance tests the same way: skip when the variable is unset.
- `make test` runs `go test ./...`. `make fmt` / `make fmt-check` format only
  files changed in the working tree (the repo baseline predates current gofmt;
  never reformat the whole tree).

### 2.3 Line endings (important)

The `agentflow` working tree has CRLF line endings on almost every file while
the committed blobs are LF. `git status` therefore shows nearly every file as
modified; `git diff --ignore-cr-at-eol` shows real changes.

- When editing an existing CRLF file, preserve CRLF (read bytes, edit, write
  back with `\r\n`), or the diff becomes the whole file.
- **Commit LF content only, and only the files you changed.** The technique
  used so far, which leaves the working tree alone:
  ```bash
  for f in <changed files>; do
    mode=$(git ls-files -s "$f" | cut -d' ' -f1); [ -z "$mode" ] && mode=100644
    h=$(tr -d '\r' < "$f" | git hash-object -w --stdin)
    git update-index --add --cacheinfo "$mode,$h,$f"
  done
  git diff --cached --stat   # confirm only your files; `git diff --cached | grep -c $'\r'` must be 0
  ```
- gofmt rejects CRLF files; check a CRLF file with `tr -d '\r' < f | gofmt -l`.
- The Studio repo uses LF except `README.md` (CRLF).

### 2.4 Commits

- Commit only when the owner asks. Never push.
- Author identity used so far: `oscar <oscar@local>` (the Studio repo has none
  configured; pass `-c user.name=oscar -c user.email=oscar@local`).
- End commit messages with:
  `Co-Authored-By: <your model name> <noreply@anthropic.com>` if you are a
  Claude model, or follow your own harness's attribution rule.
- Style: conventional prefix (`fix(scope):`, `feat(scope):`, `docs:`), a body
  that says why, and what verification showed.

### 2.5 Code conventions observed in this codebase

- Comments explain *why*, at length, including what failed before. Match that.
- Fail loudly at boot rather than degrade silently, except where the code
  already chooses "honest degradation" (e.g. an unresolvable credential skips
  an optional component with a warning).
- Config validation lives in `internal/config/config.go` (`validate`, plus
  `validate*` helpers). `config.Load` and `config.LoadDir` both run it. It is
  also what `agentflow validate`, `agentflow doctor`, the console's config
  editor and Studio's Config screen use, so a rule added there is enforced
  everywhere.
- New warnings for settings that have no effect go in `warnInertConfig` in
  `cmd/agentflow/main.go`.
- The docs in `agentflow-studio/docs` were stale before and were recently
  corrected; when behaviour changes, update `docs/configuration.md` and
  `docs/architecture.md` in the same piece of work.

---

## 3. How multi-instance works today

### 3.1 Storage targets

`internal/core/storedb/storedb.go`:
- One SQL dialect runs on SQLite (`modernc.org/sqlite`, pure Go) and
  PostgreSQL (`pgx`).
- Stores opened on the same target share one pool, capped at
  `maxOpenPostgres = 10` connections per instance.
- `ParseTarget` accepts `sqlite://`, a bare path, `postgres://` /
  `postgresql://`, and for the log plane a directory or `file://`.

Target resolution (`internal/config/config.go`):

| Store | Method | Resolution |
|---|---|---|
| runtime | `PersistencePath()` | `runtime.persistence`, default `sqlite://./data/agentflow.db` |
| identity | `IdentityStore()` | `runtime.identity.persistence`, else the same DSN if persistence is Postgres, else `identity.db` beside the runtime file |
| credentials | `CredentialsStore()` | `runtime.credentials.path`, same rule |
| leases + inbox | `ClusterStore()` | `runtime.cluster.persistence`, else `PersistencePath()` |
| log plane | `LogPlaneDir()` | `runtime.log_plane.persistence` (a directory), empty = stays in the runtime store |
| local blobs | `DataDir()` | beside the SQLite file, or `./data` |

`cmd/agentflow/main.go` opens them in order:
- `runtime.OpenStore(cfg.PersistencePath())`
- `lease.Open(cfg.ClusterStore(), lease.OwnerID(), lease.DefaultTTL)`
- later, `inbox.Open(cfg.ClusterStore(), leaseMgr.Owner())`

It already warns when:
- the lease store's backend differs from the runtime store's;
- `runtime.region` is set while the lease store is SQLite;
- identity and runtime stores are on different backends.

### 3.2 Instance identity

`lease.OwnerID()` = `host-pid-<4 random bytes hex>`, unique per process.
`runtime.region` is a label only (metric label, `/admin/api/state` field, boot
warnings).

### 3.3 Leases

`internal/core/lease/lease.go`, table `leases(name, owner, expires_at)`.
- `AcquireFor` is one compare-and-set:
  `INSERT … ON CONFLICT DO UPDATE … WHERE expires_at < now OR owner = me`;
  one affected row means you hold it.
- `Release` deletes only if you are the owner.
- `Held` asks whether anyone holds it.
- Deadlines are unix nanoseconds from each machine's clock.

Names in use: `session:<agent|key>`, `trigger:<name>`, and housekeeping names
(`files-scratch`, `files-gc`, `shell-reclaim`, `rows-sweep`, `usage-prune`,
`journal-prune`).

### 3.4 Session ownership (the session hub)

`internal/core/sessionhub/sessionhub.go`. A session is `(agent, key)`;
`SessionKey(agent, key) = agent + "|" + key`.

- `Supervisor.Deliver` (`internal/core/supervisor/supervisor.go`) calls
  `hub.Route` when a hub is set. `main.go` always sets one via `sup.SetHub(hub)`.
- `Route`: take or renew `session:<key>` (TTL 30s, `DefaultSessionTTL`).
  - If held: `deliver` to the local actor (`sup.DeliverLocal`). On failure,
    still post to the inbox.
  - If not held: `queue.Post` and return.
- `Drain` (every 250ms, `DefaultPoll`) → `drainOnce`:
  1. `queue.Pending` finds which held sessions have waiting rows, in one query.
  2. Renew those sessions' leases first.
  3. `queue.Claim` up to 50 per session, deliver, `Ack`.
  4. Idle held sessions renew only every TTL/3 (`renewIfDue`).
- `claim()`: on losing a lease it deletes the session from `held`/`renewed`,
  **and nothing else** (see gap G3).
- `Release` on shutdown hands every session back.
- `Live` checks `leases.Held("session:"+key)`; the shell reclaim pass uses it.
- Tunables: `runtime.cluster.poll_interval`, `runtime.cluster.session_ttl`.

### 3.5 Session inbox

`internal/core/inbox/inbox.go`, table `session_inbox(session_key, msg_id, ts,
body, claimed_by, claimed_at, done)`, primary key `(session_key, msg_id)`.
- `msg_id` is `origin|<message id>`, because message ids are only unique per
  instance.
- `Claim` is an atomic UPDATE with a 60s visibility window
  (`DefaultVisibility`), so delivery is at-least-once.
- `Ack` marks rows `done = 1`.
- `Sweep` deletes acked rows past `DefaultRetention` (1h), **but nothing calls
  it** (gap G1).

### 3.6 Work that must run once

- **Triggers** (`internal/core/triggers/triggers.go`): every instance runs
  every timer; `claim(j)` takes `trigger:<name>` before firing.
  - `leaseTTL`: `every:` holds the lease for interval + 30s; `cron:` releases
    30s before the next occurrence.
  - Fails closed if the store is down.
- **Housekeeping**: `singleton := func(ctx, name, fn)` in `main.go` takes a
  lease with `lease.DefaultTTL` (2 minutes) and runs `fn`.
  - `files-scratch` and `files-gc` run once at boot only.
  - `shell-reclaim` runs on a ticker.
  - `rows-sweep`, `usage-prune` and `journal-prune` run on 24h tickers
    started at each instance's boot (see gap G4).
- **Daemons** (`persistent: true`): `Supervisor.BootPersistent` →
  `bootPersistentOne` (`internal/core/supervisor/registry.go`) claims
  `<agent>|boot` (`daemonKey = "boot"`) through `hub.Claim`.
  - The winner delivers a synthetic boot message.
  - It runs at startup and when an agent is installed at runtime, **never
    again** (gap G2).

### 3.7 State that moves with a session

- **Memory backends** (`memory.backends`): history survives a move only on a
  shared backend.
  - Provider features (from each driver's `Features()`):
    - `sqlite`: kv, prefix_scan, text_search, ttl, transaction
    - `postgres`: kv, prefix_scan, text_search, transaction
    - `pgvector`: vector
    - `qdrant`: vector
    - `redis`: kv, prefix_scan, ttl
    - `redisvector`: kv, prefix_scan, ttl, vector
    - `volatile`: kv, prefix_scan, ttl
  - SQLite returns an explicit error for vector queries
    (`internal/drivers/sqlite/sqlite.go`, `case "vector"`).
- **Runtime store rows**: `session.state.*`, route state (`router.state`),
  file-store metadata, shell handle records.
- **Shell handles** (`internal/drivers/shell/registry.go`):
  - Docker containers are adopted by id; SSH handles are re-dialled from the
    profile.
  - `shellMgr.Sweep(ctx, shell.ReclaimGrace, hub.Live)` reclaims orphans.
- **Budgets**: agent daily budgets refresh from the shared ledger at most
  every 15s (`internal/core/budget/pool.go`, `DefaultRefreshInterval`).
- **Per-user quotas** read the ledger but hold in-flight reservations in
  process memory (gap G5).

### 3.8 Per-instance admin state

All of this is in-process and not shared:
- hot-applied model edits (`/admin/api/models`);
- the Studio broker (synthetic `studio` channel, trace rings);
- the console log ring;
- the metrics history.

---

## 4. Verified gaps (all confirmed in code)

| Id | Gap | Evidence | Effect | Fixed in |
|---|---|---|---|---|
| G1 | Inbox never swept | `inbox.Queue.Sweep` has no caller; the `rows-sweep` job in `main.go` calls only `rtStore.SweepExpired` and `tenReg.Sweep` | `session_inbox` grows without bound | Phase 1 |
| G2 | Daemons don't fail over | `bootPersistentOne` is only called from `BootPersistent` (boot) and the runtime agent registry (install) | A daemon on a dead instance stays down until some instance restarts | Phase 1 |
| G3 | Losing a lease doesn't stop the local actor; timers skip the hub | `Hub.claim` only deletes map entries; `schedulerAdapter.deliver` (`internal/core/supervisor/scheduler_adapter.go`) writes straight to `actor.Mailbox` | A stalled or partitioned instance keeps running timer turns while the new owner runs the same conversation. This breaks "one conversation in one place" | Phase 1 |
| G4 | Housekeeping runs once per *instance* per day | The `singleton` lease is 2 minutes, the tickers are 24h from each boot, and `files-scratch`/`files-gc` have no ticker | Every instance runs every daily job (idempotent, wasteful); file scratch and GC only run at boot | Phase 1 |
| G5 | Quota in-flight holds are per process | `accounting.Quota.reserved map[string]int64` | Concurrent calls for one user on different instances can each spend the user's remaining budget | Phase 1 |
| G6 | Admin edits apply to one instance silently | §3.8 | An operator believes a model edit is fleet-wide | Phase 1 (label it) |
| G7 | Fleet misconfiguration is silent | `fs` blobs, SQLite/volatile memory, log-plane dir, per-boot admin token are all accepted with a Postgres runtime store | Each instance silently keeps its own copy of data | Phase 0 |
| G8 | Two processes on one `single` store behave as an unplanned fleet | No instance registry | Surprising behaviour; nothing warns | Phase 0 |

---

## 5. Target design

### 5.1 New and changed settings

```yaml
runtime:
  topology: single        # single | regional. Unset = single (backward compatible).
  region: eu-west         # label (unchanged); optional; recommended for multi-region cells
  zone: eu-west-1a        # NEW optional label: metrics, instance registry
  cluster:
    persistence: …        # DEPRECATED: warn; for regional, must equal persistence or be unset
    poll_interval: 250ms  # unchanged
    session_ttl: 30s      # unchanged
```

- Add `Topology string` and `Zone string` to `config.Runtime`, with
  validation: `topology` must be empty, `single` or `regional`. Add
  `func (c *Config) Topology() string` returning `"single"` when empty.
- If `topology` is unset and `runtime.persistence` is Postgres, warn once:
  "this looks like a fleet; set runtime.topology: regional to have it
  checked".
- `runtime.cluster.persistence`: warn "deprecated" when set. In `regional`,
  refuse it unless it resolves to the same target as `runtime.persistence`.

### 5.2 Tier rules (enforced in `config.validate`)

Every refusal message names the setting and the fix, for example:
`regional topology: media.backend is fs; every instance would keep its own
blobs. Set media.backend: s3`.

**single**
- Anything currently valid stays valid.
- The instance-registry check (5.4) refuses a second live instance at boot.
  This is a boot-time check, not part of config validation.

**regional**: refuse when:
1. `runtime.persistence` is not Postgres.
2. `runtime.identity.persistence` or `runtime.credentials.path` is set to a
   SQLite target, or to a different server than `runtime.persistence` (warn
   for a different Postgres server; refuse SQLite).
3. `runtime.cluster.persistence` is set to anything other than the runtime target.
4. Any `memory.backends` entry uses provider `sqlite`.
5. `media.backend` or `files.backend` is not `s3`.
6. `runtime.log_plane.persistence` is set.
7. `ADMIN_TOKEN` is unset and `runtime.admin.operators` is empty.
   - This is an environment check, so it lives at boot in `main.go` next to
     `resolveAdminTokenWithOperators`, not in `config.validate`.
   - In `regional`, the per-boot token is refused rather than minted.
8. A store used for durable conversation data is on a local backend without
   `locality_ok: true` (see 5.3).

### 5.3 Locality model (phase 1.5)

Derived, never configured (except the opt-in flag):

| Provider | `single` | `regional` |
|---|---|---|
| `postgres`, `pgvector`, `qdrant` | `deployment` | `deployment` |
| `sqlite` | `deployment` (one instance) | refused |
| `redis`, `redisvector` | `deployment` (one instance) | `region` |
| `volatile` | `instance` | `instance` |

Notes:
- In `single`, the instance is the deployment, so only `volatile` is
  "instance" in a way that matters across restarts: volatile data dies with
  the process, and the label says so.
- Exposure to Lua: `agent.info().memory.stores[name].locality`, next to the
  existing `features` (built in `Actor.infoJSON`,
  `internal/core/session/actor.go`, which already emits `backend`, `table`,
  `features`).
  - Adding a field to `agent.info` changes what a loop is handed. Read
    literally, the contract in `internal/vm/version.go` ("any edit to … the
    shape of what a loop is handed bumps the version") calls for bumping
    `PreludeVersion`, even though the change is additive. **The owner
    decided against a bump (section 9.1): keep version 1.** A bump means updating every
    Lua file's `-- af-prelude-version:` line (builtins, examples, tests) and
    Studio's analyzer, all in one change.
  - Also add it to `api/luau/af.d.luau` (and run Studio's `make docs-sync`).
- Durable-store check (`regional` only):
  - `plugin:recency` and `plugin:semantic` recall from the profile's
    `dialogue` store; `plugin:routing_table` writes turns to the stores its
    routing returns. The built-in default profile
    (`config.DefaultMemoryProfile`) uses stores `dialogue` and `facts` on
    backend `main_db`.
  - Rule: in `regional`, any memory profile store named `dialogue` or
    `facts`, or used as the recall store of `plugin:recency` /
    `plugin:semantic`, must have locality `deployment` or `region`, unless
    the store sets `locality_ok: true`.
  - Add `LocalityOK bool yaml:"locality_ok"` to `config.Store`.
- `volatile` bound: require `config.max_entries` (new, enforced by the
  volatile driver) or a TTL on every store bound to it. Today volatile is
  unbounded process memory shared by all sessions on the instance.
- Ownership generation, so a returning session never reads stale local data:
  - Add `generation BIGINT NOT NULL DEFAULT 0` to `leases`, incremented when
    `AcquireFor` changes the owner (`owner <> excluded.owner`). Use
    `CREATE TABLE IF NOT EXISTS` plus a guarded `ALTER TABLE … ADD COLUMN`
    that tolerates "duplicate column" on both backends.
  - `AcquireFor` returns, or a new `Generation(name)` reads, the generation.
  - For stores with locality `instance`, prefix the store scope with
    `g<generation>:` for session-scoped keys. Stores with locality `region`
    or `deployment` are shared by every instance that can own the session, so
    they need no prefix. Old generations' rows age out through TTL /
    `max_entries`.
  - The store scope is built in `internal/core/memory/scope.go`; read it
    before changing anything.

### 5.4 Instance registry (phase 0)

New table in the runtime store (`internal/core/runtime` or a small new
package `internal/core/fleet`):

```sql
CREATE TABLE IF NOT EXISTS instances (
  owner TEXT PRIMARY KEY, region TEXT, zone TEXT, version TEXT,
  topology TEXT, config_epoch TEXT, started_at BIGINT, last_seen BIGINT)
```

- Heartbeat every 10s; rows are "live" when `last_seen` is within 30s.
- Delete your own row on shutdown; sweep rows older than a day in `rows-sweep`.
- `single`: at boot, if another live row exists with a different owner, refuse
  to start. Message: another AgentFlow instance (owner, host, last seen) uses
  this store; `single` topology allows one; stop it or set `regional`. Add an
  escape hatch, `-force-single`, for a crashed peer whose row has not expired
  (or just wait 30s).
- `/admin/api/state` gains `topology`, `zone` and `instances: [...]`, flagging
  peers whose `config_epoch` differs. Studio's Overview shows them (Studio repo,
  `internal/ui/overview.go`, `internal/api/operate.go` `State`).

### 5.5 `agentflow doctor` live checks (phase 0)

`cmd/agentflow/cli.go`, the `doctor` command: today offline. Add an
`-online` flag (default on when topology is `regional`):
- connect to the runtime DSN;
- confirm the `vector` extension exists when a `pgvector` (or vector-capable
  postgres) backend is configured;
- put/get/delete a probe object in each S3 bucket;
- ping Redis and Qdrant.

Print one line per check with pass/fail and the fix.

### 5.6 Multi-region (no tier; a documented pattern, phase 4)

One `regional` cell per region. The application in front:
- pins each user to a home region and sends sign-up, invites, credential
  setup and traffic there;
- owns any shared chat integration and forwards to the user's region with the
  `bridge` channel. A Telegram bot has one receiver, and two regions polling
  one bot conflict;
- fans out admin and billing (`/admin/api/usage` per region);
- runs disaster recovery: a standby Postgres in another region, promoted to
  stand up a replacement cell.

---

## 6. Phases

Each phase: implement, add tests, update docs in `agentflow-studio/docs`,
then stop for the owner's review. Keep each phase a separate commit series.

### Phase 0: topology setting, validation, registry, doctor (small)

Tasks:
1. Config fields and validation (5.1, 5.2 rules 1–6, 8 deferred to phase
   1.5). Unit tests in `internal/config` (pattern: `validate_*_test.go`), one
   test per refusal plus a valid regional config.
2. Admin-token rule 7 in `main.go`: in `regional`, `resolveAdminTokenWithOperators`
   must not mint a per-boot token; error instead. Unit test in
   `cmd/agentflow/admin_test.go` (it already tests token resolution).
3. Deprecation warning for `runtime.cluster.persistence` (`warnInertConfig`
   is for no-effect settings; add a sibling `warnDeprecatedConfig`).
4. Instance registry (5.4), heartbeat goroutine started in `main.go` after
   the runtime store opens, the `single` refusal, `/admin/api/state` fields
   (`internal/webui/webui.go`, `handleState`).
5. `doctor -online` (5.5).
6. Remove the now-redundant boot warnings in `main.go` that compare the lease
   store's backend to the runtime store's, and `runtime.region` against a
   SQLite lease store, *only* once the leases and inbox are guaranteed to be
   on the runtime store (i.e. `cluster.persistence` refused or equal). Keep
   the identity/runtime backend warning.

Acceptance:
- `agentflow validate` on a `regional` config with `fs` blobs fails with a
  message naming `media.backend`; every rule has a test.
- Starting two `single` instances on one SQLite file: the second refuses with
  the peer's owner id. Test with two `lease`/registry handles on a temp SQLite
  file (no binary needed).
- Existing configs without `topology` boot unchanged (run the existing config
  tests and `agentflow validate agentflow.example.yaml`).

Docs: `configuration.md` (new keys, a "Topology" section with both tiers'
tables), `architecture.md` "Deployment" section (replace the tier ladder with
`single` and `regional` plus the multi-region pattern pointer).

### Phase 1: make `regional` correct (medium)

Tasks, each with a test:

1. **G3, lost lease stops the local session.**
   - `Hub.claim`: when an acquire returns false for a session in `held`, call
     a new callback `onLost(sessKey)`. `main.go` wires it to
     `sup.StopSession(sessKey)`. `StopSession` cancels the actor's context,
     and `onActorExit` then calls `s.sched.CancelOwner(id.SessionID)`, so its
     timers go too (verified).
   - **Trap:** the supervisor keeps a `retired` set of session ids that ran
     and exited, so sends to a dead `session:<id>` address fail. A session
     stopped because it lost its lease must be re-spawnable when this
     instance later wins the lease back. Read how `retired` is filled in
     `onActorExit` and consulted in `ensureSession` / `deliverAgentMessage`,
     and either skip retiring on lease loss or clear the entry on re-claim.
     Cover it with a test (A loses, B owns, B releases, A wins again and the
     session runs).
   - Route timer ticks through the hub: in `schedulerAdapter.deliver`, when
     `s.hub != nil`, call `s.hub.Route(ctx, agent, key, msg)` instead of
     writing to the mailbox. You need agent and key from the owner session
     id; the owner is the session key `agent|key`, so use
     `sessionhub.SplitSessionKey`.
   - Timer message ids today are `"timer:" + owner + ":" + timerID`, the same
     for every tick of a timer. The inbox deduplicates on (session, origin|id),
     so a queued tick would swallow later ticks. Make the id unique per fire
     (append the fire's unix-nanos, or a counter).
   - Test: two hubs on one SQLite file with a short TTL; instance A holds a
     session; stop A's renewals; B takes over; assert A's `onLost` fires and
     a timer tick on A is queued for B, not delivered on A.
2. **G2, daemon failover.**
   - A loop in the supervisor, or in `main.go` next to `go hub.Drain(ctx)`,
     every `session_ttl/2`: for each persistent agent def, if
     `hub.Live(ctx, name+"|boot")` is false, call `bootPersistentOne`. It
     already claims through the hub, so only one instance wins.
   - Test with two hubs: A boots the daemon, A releases (or its lease
     expires), B's loop boots it.
3. **G1, inbox sweep.** Call `hubQueue.Sweep(ctx)` in the `rows-sweep`
   singleton function in `main.go`; log the count. Test: `inbox` package,
   ack rows, age them past retention, `Sweep` removes them.
4. **G4, housekeeping once per deployment.**
   - Change `singleton` to take a TTL: `singletonFor(ctx, name, ttl, fn)`,
     where ttl = the job's interval + slack (same idea as triggers'
     `leaseTTL`): 24h + 5m for the daily jobs; `shell.ReclaimInterval` +
     slack for shell reclaim.
   - Give `files-scratch` and `files-gc` hourly tickers.
   - Keep the boot run.
   - Test the TTL helper, and that a second instance's tick within the
     interval skips.
5. **G5, shared quota holds** (regional only; `single` keeps the in-memory map).
   - Table `quota_holds(id TEXT PRIMARY KEY, user_id TEXT, amount BIGINT,
     expires_at BIGINT)` in the runtime store.
   - `Reserve` atomically checks `ledger used + SUM(live holds) + amount <=
     limit` and inserts a hold with a TTL (e.g. 10 min, longer than any model
     call); `Lease.Release`/commit deletes it.
   - Doing the check and insert atomically needs a transaction: on Postgres,
     `SELECT … FOR UPDATE` on a per-user row, or an advisory lock keyed by a
     hash of user_id; SQLite in `single` keeps the old path.
   - Put it behind an interface so `accounting.Quota` picks the
     implementation from topology. The quota code is in
     `internal/core/accounting/quota.go`; `Status` must sum holds too.
   - Test with `AGENTFLOW_TEST_POSTGRES`: two `Quota` instances, concurrent
     reservations, total never exceeds the limit.
6. **Drain on shutdown.**
   - On SIGTERM: `admin.SetReady(false)` (`internal/core/metrics/metrics.go`)
     → wait `runtime.shutdown_grace` (new, default 5s in `regional`, 0 in
     `single`) → existing shutdown (`hub.Release`, …).
   - The shutdown block is at the end of `main()`, after `<-ctx.Done()`.
7. **G6, label instance-local admin edits.**
   - `PUT/DELETE /admin/api/models/{name}` responses gain `"scope": "instance"`.
   - Studio's Models screen shows "applied to this instance only; Save to
     config and roll the fleet" when topology is `regional` (topology comes
     from `/admin/api/state`).
   - Studio code: `internal/ui/models.go`.

Acceptance: all tests above pass; a two-process manual check on one Postgres
(two `./agentflow` processes with different `gateway.listen` and
`runtime.admin.listen`): kill -9 one, its sessions and daemon resume on the
other within `session_ttl`, nothing runs twice.

Docs: `architecture.md` "What multi-active guarantees" table: update rows for
housekeeping, quotas and daemons, and remove the limits now fixed.

### Phase 1.5: locality (small to medium)

Tasks: everything in 5.3.
- Locality derivation in `internal/core/memory` (a function of provider +
  topology).
- Exposure in `Actor.infoJSON`.
- `locality_ok` and the durable-store rule in `config.validate`.
- volatile `max_entries` in `internal/drivers/volatile`.
- Lease `generation` and the scope prefix.
- Update `api/luau/af.d.luau` and Studio's analyzer entry for
  `agent.info` (`agentflow-studio/internal/lua/api.go`).

Acceptance:
- a `regional` config with `dialogue` on `volatile` fails validation, and
  passes with `locality_ok: true`;
- `agent.info()` shows locality in a loop test (`internal/looptest` or
  `internal/vm` tests);
- a session moved A→B→A does not see its first-generation volatile/redis
  keys on A (test at the store-scope level).

### Phase 2: single-instance simplifications (small to medium)

1. **One `postgres` memory provider with vectors.**
   - `internal/drivers/postgres`: at `Open`, check
     `SELECT 1 FROM pg_extension WHERE extname='vector'`; if present, add
     `vector` to the features the backend reports and serve vector queries
     with the pgvector code path (reuse `internal/drivers/pgvector`).
   - Keep the `pgvector` provider as an alias for compatibility.
   - Note that `Features()` is per provider today, not per backend instance;
     check how `memory.Registry.Features(name)` is computed and make it
     per-backend if needed.
2. **SQLite vectors without an extension.**
   - Store vectors as BLOB (little-endian float32) in the sqlite driver's
     table and serve `vector` queries with a brute-force cosine scan,
     capped at a configurable row limit (default 50k; error beyond it, with
     advice to use pgvector).
   - Replace the current "unsupported" error in
     `internal/drivers/sqlite/sqlite.go`.
   - Real sqlite-vec would need a different SQLite driver (the current one is
     pure Go and cannot load C extensions); out of scope.
3. **Default `main` backend.** When `memory.backends` is empty, register
   backend `main_db` on the runtime store target with the provider matching
   its backend (sqlite/postgres). `config.DefaultMemoryProfile` already
   references `main_db`; check what happens today when it is undeclared
   (`ResolveMemoryProfile`, `RebindSkipped`) before changing it.
4. **Migration commands** in `cmd/agentflow/cli.go`:
   - `agentflow migrate store --from <target> --to <target> [--tables …]`
     copies the runtime, identity, credentials and tenancy tables in batches
     (same schema on both backends, so it is a copy). Refuse when the
     destination is not empty unless `--append`. Credentials are copied as
     ciphertext; the master key must be the same.
   - `agentflow migrate blobs --from fs:<dir> --to s3` copies blobs (named by
     content hash, so skip ones already present).
   - Test with SQLite→SQLite, and SQLite→Postgres under
     `AGENTFLOW_TEST_POSTGRES`.

Docs: `getting-started.md` (minimal config is now `persistence` + `agents`),
`configuration.md` memory section (vector support per provider).

### Phase 4: multi-region pattern support (medium)

1. **User migration bundle.** Today's `GET /admin/api/users/{id}/export`
   (`internal/webui/users.go`, `handleUserExport`) is a privacy export: it
   carries usage, audit, project *names* and credential *references*, **not
   memory rows, file contents or secret values**. Build a separate
   full-fidelity bundle:
   - `GET /admin/api/users/{id}/bundle` returns a tar or zip containing:
     profile, linked identities, memory rows in the user's scope across every
     store, file trees plus blobs, credentials re-encrypted to a
     caller-supplied public key, and tenancy memberships.
   - `POST /admin/api/users/import` takes the bundle and the private key, and
     re-encrypts credentials with this region's master key.
   - CLI equivalents.
   - The user scope prefix is `user:<uuid>`; see `internal/core/memory/scope.go`
     and the files manager's `Projects`.
2. **Region in the user API.** `GET /v1/users/config` (`internal/core/users`)
   and `/admin/api/state` report `runtime.region`. Update `api/openapi/users.yaml`
   and Studio's copy (`make docs-sync`).
3. **Studio connection profiles.** `agentflow-studio/internal/appconfig`:
   - change `Config` to hold `Profiles []Profile{Name, URL, Token,
     ForgetToken}` and `Active string`, migrating the current single URL and
     token into one profile;
   - add a profile switcher in the rail footer (`internal/ui/app.go`,
     `showMain`) and a profiles list on the login screen
     (`internal/ui/login.go`).
4. **Docs**, new `docs/multi-region.md` in Studio:
   - the cell pattern;
   - the bridge front-door pattern for a shared bot;
   - Telegram's single-receiver limit;
   - per-region billing fan-out;
   - the DR runbook (cross-region Postgres standby, promote, point a new cell
     at it, re-point the edge).

---

## 7. Testing guidance

- Unit-test every config rule (positive and negative). Use the existing
  `internal/config/*_test.go` style.
- Multi-instance logic is testable without the binary: construct two
  `lease.Manager` + `inbox.Queue` + `sessionhub.Hub` on one temp SQLite file
  (see existing tests in `internal/core/sessionhub` and `internal/core/lease`
  for setup), with short TTLs and polls.
- Postgres-only behaviour (quota holds, migration to Postgres): gate on
  `AGENTFLOW_TEST_POSTGRES`.
- Run `CGO_ENABLED=1 go test -race` on packages with goroutines you add.
- Before claiming a phase done: `make vet`, `make test`, `agentflow validate`
  on `agentflow.example.yaml` and on `examples/configdir`, plus the manual
  two-process check for phase 1.
- Studio changes: `go test ./...` in `agentflow-studio`. Its UI tests use the
  headless Fyne driver, which runs `fyne.Do` callbacks on the calling
  goroutine, so give async loads time to settle (see `ui_test.go`'s
  `settle`).

---

## 8. Docs to update (agentflow-studio/docs)

- `configuration.md`: `runtime.topology`, `runtime.zone`,
  `runtime.shutdown_grace`, `locality_ok`, volatile `max_entries`,
  deprecation of `runtime.cluster.persistence`, the tier tables, `doctor -online`.
- `architecture.md`: "Deployment" section rewritten around `single` /
  `regional` / the multi-region pattern; the guarantees table updated after
  phase 1.
- `loops-reference.md` / `loops.md`: `agent.info().memory.stores[*].locality`
  and guidance (durable data on `deployment` stores, caches on local ones).
- `getting-started.md`: minimal config after phase 2.
- New `multi-region.md` (phase 4).
- `docs/luau/af.d.luau` via `make docs-sync` from `agentflow/api/luau/af.d.luau`.

---

## 9. Open questions and assumptions (confirm before the phase that needs them)

1. **Prelude version bump for `locality`** (phase 1.5): additive field in
   `agent.info`. Bump `PreludeVersion` or not? The contract in
   `internal/vm/version.go` says "any edit to the prelude's surface, or to the
   shape of what a loop is handed, bumps the version". Read literally, this
   bumps. **Answered (2026-11-08): no bump.** The owner chose to keep
   `PreludeVersion` at 1; `locality` ships as an additive field. A bump made
   on `feat/deployment-tiers` was reverted.
2. **Redis in `single`** was described by the owner as "local to each
   instance". With one instance that is the same as deployment-wide; this
   plan labels it `deployment`. Confirm the label.
3. **`regional` with Redis**: one region-wide cluster (locality `region`), as
   in the owner's original proposal. Confirm AZ-local Redis is not wanted
   inside a regional cell.
4. **Shared quota holds cost** one extra write per model call in `regional`.
   Acceptable?
5. **`shutdown_grace` default** of 5s in `regional`.
6. **Branch base** for the work (section 2.1).
7. **SQLite brute-force vector cap** default (50k rows).

---

## 10. Out of scope / do not do

- No global tier, cross-region database, read replicas, home-region session
  placement or cross-region Qdrant sync (all considered and dropped).
- No cache layer, and no leases or inbox on Redis.
- No MQTT/NATS/Kafka.
- Do not reformat untouched files, and do not commit the CRLF noise.
- Do not change `PreludeVersion`: the owner answered 9.1 with no bump.
- Do not push, and do not merge branches without being asked.
