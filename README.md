# AgentFlow

A single-binary, self-hosted LLM agent runtime written in **Go** with **Luau** scripting (via cgo, built with zig). Configure the ordinary; program the exceptional.

Every agent session is an actor — one goroutine, one mailbox, one Luau state. Lua coroutines park on a single async bridge (`__af_op`) instead of blocking OS threads, so a busy instance stays cheap. YAML wires the topology; Lua plugins carry the behavior.

## Features

- **Actor-model sessions** — per-session Luau state, message-passing only, hot reload of loops and instructions.
- **Multi-agent** — `agent.send` / `agent.request` / `agent.reply` / `agent.spawn`, address authority with `can_contact` ACLs, per-child budgets. Spawn profiles carry their own `extras` and credential allow-list through to each child.
- **Memory** — provider → backend → store layering; `conversational` preset; retention/window GC. Stores are isolated per agent by default (`<agent>.<table>`); `shared: true` opts a store into deliberate cross-agent sharing. Private stores are further scoped per user at the key level (engine-enforced: a channel turn reads only its own scope + service + pre-upgrade rows; engine-fired maintenance contexts like the nightly distiller read all scopes with scope-explicit writes and enumerate them via the maintenance-only `store.scopes` op; `scope: agent` opts back into one pool). A backend whose credential is unresolvable is skipped with a warning, and its stores rebind to a surviving backend (vector → full-text search) or drop — never a boot failure.
- **Embeddings & reranking** — `llm.embed` (OpenAI-compatible `/embeddings`) and `llm.rerank` (Jina/Cohere/TEI/vLLM `/rerank`); pgvector ingest on write and a `plugin:semantic` recall pipeline (embed → vector k-NN → rerank). Multimodal embeddings follow the Jina convention (`jina-embeddings-v5-omni-small`: text + image/video/audio/pdf in one vector space); `memory.write` embeds attachment-carrying records as one merged vector.
- **Tools** — three surfaces, kept deliberately distinct:
  - **Builtins** registered by the runtime: filesystem ops inside shell handles, `web_search` (multi-engine), `legal_search` / `legal_read`, `browser`, `fetch`, `shell.exec` / `shell.write` / `shell.destroy` — plus MCP stdio servers discovered at boot.
  - **Loop-declared tools**: a loop defines model-visible tools in Lua with `tool.def` (merged into `tools.list`, dispatched by `tools.run`).
  - **Ops** (`http.request`, `llm.chat`, `store.*`, `shell.*`, `mail.*`): reachable from Lua only — `tools.run` resolves names through the exposed tool set, so a model can never dispatch one.
- **Tool policy** — `tools.policy.default: none|all`, per-agent `skills:` filters (one shared loop directory can carry tools only some agents see), and per-tool `overrides` that retitle/re-document any tool (description + per-param descriptions; `{prompt: key}` resolves from the prompt registry). Overrides reach loop-declared tools too.
- **Capabilities, enforced** — `agents.<name>.capabilities` gates ops at runtime with a refusal naming what is missing; `plugins.enforce_capabilities: false` restores the old declare-only behavior. Defaults: `llm.chat`, `memory`, `tools`, `agent.send`, `net.http` (`net.mail` and `shell.exec` must be declared).
- **Naming** — `builtin:` is the tool namespace alone: loop plugins are `plugin:<name>`, memory providers are bare (`sqlite`), the built-in memory profile is `conversational`. A name in the wrong field fails at boot with the corrective spelling.
- **HTTP** — `http.request` / `os.env` Lua ops with scheme validation, body cap, secret-header redaction, and an address guard that refuses private, loopback, link-local and reserved destinations in the dialer (DNS rebinding and redirect hops covered).
- **Mail** — `mail.imap.fetch` / `mail.smtp.send` Lua ops (cap `net.mail`); passwords resolve from the credential store at call time and never cross the Lua bridge.
- **Multimodal** — channels ingest media into a content-addressed blob store (per-channel allow-list + size ceiling; local filesystem or S3/MinIO backend via the top-level `media:` config); loops forward part descriptors into `llm.chat` and the runtime resolves them at request time. Every provider covers images + PDFs; audio on the OpenAI shapes (input_audio) and Gemini; video via the MiniMax/Kimi/GLM `video_url` convention on the OpenAI shapes, a video block on Anthropic, and the Gemini Files API (resumable upload, referenced by URI, auto-deleted after 48h) on Gemini. Opt-in via `media:` on a channel.
- **Files** — user-scoped project trees with engine-native snapshot versioning (commits + named refs on a parent chain — deliberately not a git server). Scope is engine-resolved per turn (`user:<uuid>` for channel turns, else `agent:<name>`); bytes are content-addressed (`files:` config, fs or S3-compatible); snapshot metadata lives in the runtime sqlite store, inspectable with plain SQL. `files.put/read/list/delete/commit/checkout` + per-session `files.scratch.*` with TTL (cap `files`); puts accept content, base64, or an existing `{handle=...}` so rollback can restore a commit's tree without bytes crossing Lua. A boot-time mark-sweep GC reclaims orphaned blobs past `files.gc_grace` (commit trees stay live so rollback keeps working). Shell spawns can mount volumes and check out a snapshot (or the session's scratch) into the container before it reports ready; `http.request` / `builtin:fetch` / `builtin:browser` can save bodies straight to scratch.
- **Audit journal** — core-owned `message_journal` in the runtime store: every inbound (router) and outbound (session egress) message is recorded with channel, sender, agent, session, text, attachment handles, and delivery status — loops and channels can neither bypass nor forge it. Retention is configurable (`audit.retention_days`, default 90, 0 = forever).
- **Config at runtime** — loops read their deployment instead of having it baked in: `agent.config()` (own profile incl. `goal` + extras + the resolved `prompts` registry, secrets as opaque markers), `runtime.triggers()` (the merged triggers list, answered to both loops and the gateway route — replaces baked CRON_TASKS/ROUTES tables), and `credential.get(name)` (allow-listed, logged, counted; the value never touches the journal). `os.time()` and `os.date()` give loops a UTC wall clock.
- **Prompt registry** — a top-level `prompts:` map holds deployment prompt text in one place (`file:` resolved relative to the configdir, or `inline:`/`text:` literals) instead of hardcoding it in Lua. The resolved text is surfaced read-only to loops as `agent.config().prompts`; `instructions` accepts `{prompt: <key>}` as well as a file path, and a tool override's `description` accepts the same form. A `file:`-backed entry stays live: editing the file republishes the text to every loop and refreshes in place the system prompt of any agent sourced from that key, with no session restart. An unreadable prompt file or an unknown key fails the boot, never a silently empty prompt.
- **Credentials** — encrypted-at-rest, per-tenant credential store; loops reference a key by `{service=...}` and Go resolves and injects it at request time. Secret config fields (`models.*.api_key`, channel tokens, backend urls/passwords, …) accept literals, `${VAR}`, or `cred:<service>`; resolution is env → store → honest degradation (optional components skip with a warning naming the credential; a model key fails the first LLM call clearly). Manage engine-wide entries with `agentflow cred set|get|list|delete <name>` (stdin/prompt only, never argv; `list` shows names only).
- **Terminal dashboard** — a Bubbletea TUI that attaches automatically when stdout is a TTY (`-no-tui` to disable): the curated counters sectioned by subsystem with in-process sparklines, the live session counts in the header, and a log footer that scales with the terminal (3–8 lines). Read-only by design — it renders metrics and mirrors the log stream, and nothing it shows can change the runtime; `q`/`esc` quits. A terminal too short for the pane clips it and says so rather than dropping rows silently, and one too short for both panes drops the metrics pane entirely so the frame always fits. Piped output (systemd, `| tee`) keeps the plain logger instead.
- **User API** — a JSON-only, frontend-agnostic profile API on the shared public listener (`runtime.users`): channel linking proven by an in-channel challenge (the engine hands the profile owner a code, and only the handle's owner can send it), self-service usage/quota/projects, and per-profile API tokens. **The engine has no login of its own**: with `runtime.users.oidc` set it verifies access tokens from an external identity provider (Better Auth, Keycloak, Auth0, …) against that issuer's published JWKS, checks `iss`/`aud`/`exp`, and takes the subject claim as the identity — provisioning a profile on first login unless `jit_provisioning: false`. Every request is a bearer token, so there is no cookie, no server-side session, and nothing ambient for another site to replay; `POST /register` still mints `afu_…` tokens for scripts and for deployments without a provider. CORS is off until an operator lists the frontend's origin, and the engine ships no user interface — an external frontend deploys on its own cadence against the published contract (`internal/webui/docs/openapi/users.yaml`, served at `/docs/openapi/users.yaml`).
- **Web console** — embedded single-page operator UI on the admin server (no build step): hot model management with test/persist (including the per-model `thinking` level), a validated config editor, an API-key manager over the credential store, live sessions, and in-process metrics sparklines. Token-authenticated; secrets are write-only.
- **Scheduler** — `scheduler.every/after/cron`; timers arrive as mailbox messages carrying `payload.timer_id` (multiple independent timers per session), never a cross-goroutine Luau call. Daemon agents (`persistent: true`) boot on a synthetic message instead of waiting for traffic.
- **Scheduled triggers** — `every:` / `cron:` triggers declared in config are executed by the **engine**, not by an agent: when due, the runtime enqueues a `Message{ type = "cron" }` (payload copied from the trigger) into the target profile's session — a configured agent or a spawn profile. No capability is involved, so a deployment needs no user-space scheduler agent. `every:` takes `ms/s/m/h/d` (`30s`, `6h`, `1d`), `cron:` the classic 5 fields (`*`, `N`, `N-M`, `*/N`, lists) matched in `runtime.timezone_offset_hours` (default UTC, a fixed offset — no DST drift). `run_on_boot` fires once at startup. A malformed expression or unknown target is logged and skipped, never a boot failure, and a `-configdir` re-reads `triggers/*.yaml` on a poll so edits apply without a restart.
- **Config directories** — `-configdir <dir>` loads `system.yaml` + `channels.yaml` + `profiles/*.yaml` (one agent per file, `spawn: true` routes to `profiles.agent`) + `triggers/*.yaml` instead of a single `-config` file; same strict validation after merge, and the directory is the base for every relative path.
- **Budget** — per-agent token pools with reserve/commit/release around LLM calls; daily reset or rolling-window accounting; spawn profiles get their own shared pool.
- **Safety** — core-owned ingress/egress chain (source-attribution, signal-gate, steady-directive, support-offer, affect-guard) that cannot be uninstalled from Lua.
- **Observability** — `/healthz`, `/readyz`, `/metrics`, `/admin/sessions` on loopback; an embedded web console on the admin server (token-authenticated, `-no-webui` to disable); shared channel listener serves `GET /health`.

### Driver set

| Domain | Providers |
|---|---|
| LLM | `anthropic` (Messages API), `openai` (Chat Completions + Embeddings), `openai-responses` (Responses API), `gemini` (Interactions API), `rerank` (cross-encoder rerank) — all pointed at compatible endpoints via `base_url` |
| Storage | SQLite, Redis, MongoDB, PostgreSQL, in-memory volatile |
| Media store | local filesystem (default), S3 / MinIO (hand-rolled SigV4, no AWS SDK) — content-addressed `media:<sha256>` handles |
| File store | same content-addressed blob family (`files:` config, fs or S3-compatible incl. Cloudflare R2); snapshot metadata in the runtime sqlite `files_meta` table |
| Vector | pgvector (cosine similarity), Qdrant, Redis |
| Channels | webhook (sync reply with configurable `timeout`, or `async: true` fire-and-poll via `GET <path>result/<id>` / `callback_url`), GitHub webhook (`ghhook`), Telegram (polling + webhook + auto) |
| Mail | IMAP fetch + SMTP send (in-process, cap `net.mail`) |
| Shell | Docker, SSH |
| Tools | builtins + MCP stdio |
| Web search | Doubao (Volcano Engine), Ollama (hosted web search), StackOverflow (StackExchange API), GitHub (repository search), YouTube (Data API v3) — one `builtin:web_search` tool, per-call `engine` param; honest-unavailable when unconfigured |
| Legal | HKLII (Hong Kong case law + legislation, with citations) and NPC (China National Database of Laws and Regulations) — `builtin:legal_search` + `builtin:legal_read` (full text, extracted from Word docs via the built-in parser); free, no key |
| Browser | Cloudflare Browser Run (formerly Browser Rendering) Quick Actions — one `builtin:browser` tool with a per-call `action` (`markdown`, `content`, `links`, `scrape`, `json`, `accessibility_tree`); reads a page through a real headless browser, so JavaScript-rendered content is present where a raw HTTP fetch returns an empty shell; honest-unavailable when unconfigured |
| HTTP | `builtin:fetch` — a curl-like client (method, headers, `body`/`json`, query, redirects, timeout, and an alerted TLS-verification skip) for APIs and plain pages. Connections to private, loopback, link-local, ULA and reserved addresses are refused **in the dialer**, so redirect hops and DNS rebinding are covered too — including a zoned address, which a prefix check alone would miss. `net.http.allow_private: true` opts out for deployments that call internal services |

## Requirements

- Go 1.25+
- GNU make
- A C/C++ toolchain for the Luau cgo bridge — the Makefile auto-selects by host OS:
  - **Windows**: [zig](https://ziglang.org/) (hermetic; no system gcc needed)
  - **Linux**: system gcc (`cc`/`c++`/`ar`)
  - **macOS**: Xcode/clang (`clang`/`clang++`/`ar`)

Luau is a git submodule at `third_party/luau`, pinned to upstream release **0.731** (MIT license — see `third_party/luau/LICENSE.txt`). Clone with `git clone --recurse-submodules`, or run `git submodule update --init` in an existing checkout — `make` builds the pinned commit's sources into `internal/vm/lib/libluau.a`.

## Build

```bash
make            # compile the pinned Luau submodule -> internal/vm/lib/libluau.a, then the binary
make test       # go test with the host C/C++ toolchain
make vet        # go vet
make run        # build, then run (CONFIG=config.yaml by default)
make clean      # remove build objects, static lib, and binary
```

The Makefile detects the host via `go env GOHOSTOS`/`GOHOSTARCH` and picks the compiler, archiver, and (on Windows) zig's `-target` triple accordingly. To cross-compile instead, set `GOOS`/`GOARCH` and a matching zig `-target` manually — native builds on each target OS are the recommended path.

## Quick start

No model needed — a file-based echo loop replies out of the box. Save the loop as `echo.lua`:

```lua
function loop()
  while true do
    local msg = session.inbox()
    session.send("echo: " .. (msg.text or ""))
  end
end
```

and this as `config.yaml`:

```yaml
version: "1"

agents:
  echo:
    loop: ./echo.lua

gateway:
  listen: ":8080"            # one shared listener; channels mount paths on it
  channels:
    - type: webhook
      path: /webhook/
      agent: echo
```

```bash
./agentflow -config config.yaml
curl -X POST localhost:8080/webhook/ -d '{"from":"alice","text":"hello"}'
```

> The webhook path ends in `/` (a subtree mount). Posting to `/webhook` without
> the slash returns a `307` redirect, which `curl -d` does not follow — include
> the trailing slash (or pass `-L`).

For an LLM-backed bot, add a compatible endpoint (OpenAI-compatible local endpoints like Ollama work out of the box) and point an agent's `model` at it:

```yaml
models:
  default:
    provider: openai
    model: gpt-4o-mini
    base_url: http://127.0.0.1:11434/v1   # Ollama / vLLM / LiteLLM / OpenRouter
    api_key: ${OPENAI_API_KEY}            # may be empty for keyless local endpoints
    thinking: medium                      # off | low | medium | high | xhigh | max; empty = provider default
    server_tools: []                      # provider-native tools, e.g. [google_search] on gemini
```

`provider` selects the request/response shape (`anthropic` | `openai` | `openai-responses` | `gemini` | `rerank`); `base_url` selects the host. `thinking` sets the model's default thinking level — one provider-neutral vocabulary (`off` | `low` | `medium` | `high` | `xhigh` | `max`) mapped per provider (Anthropic `budget_tokens`, OpenAI `reasoning_effort`, Gemini `thinking_budget`); overridable per call via `llm.chat` `opts.thinking`. Whatever reasoning content a provider sends is surfaced passively on the reply (`reply.thinking` text, `reply.thinking_blocks` raw blocks, `usage.reasoning` tokens); an Anthropic multi-round tool loop replays the blocks verbatim via `thinking_blocks` on the assistant turn. `server_tools` injects provider-native, server-side tools (e.g. Google Search grounding on `gemini`, `web_search` on `openai-responses`) that run inside the provider's completion.

The runtime ships as a standalone engine. Reference product apps built on top
of agentflow — for example a full multi-agent orchestrator (main + expert +
project manager + workers), with its own loops, route, and instruction files —
live in a separate repo and consume agentflow as a binary/library. This repo
holds only the runtime.

## Documentation

Full docs (install guide, architecture, config reference, Lua plugin development guide, capability API) are embedded in the binary and served by the web console at **`/docs/`**:

```bash
./agentflow -config config.yaml
# open http://127.0.0.1:9090/docs/
```

No separate server or build step — the site is `go:embed`ded from `internal/webui/docs/` and readable without the admin token. The config reference there covers every key this README only summarizes: models, memory, gateway/channels (including Telegram's `mode: polling | webhook | auto`, `secret_token` webhook auth, and the stale-webhook cleanup on polling), tools policy, media, audit, triggers, and the `-configdir` layout.

## Repository layout

```
agentflow/
├── Makefile            # build: Luau submodule -> libluau.a -> agentflow binary
├── cmd/agentflow/      # main entrypoint
├── internal/           # core runtime + drivers (not importable — internal module)
│   ├── builtins/       # embedded Lua builtins (per_chat route + support chunks)
│   ├── core/           # actor/session, supervisor, router, gateway, scheduler, triggers,
│   │                   #   safety, memory, budget, netguard, tools, caps, credentials,
│   │                   #   identity, users, accounting, reload, metrics, runtime, ...
│   ├── drivers/        # llm, search, legal, browser, fetch, shell, mcp, httpd, telegram,
│   │                   #   webhook, ghhook, sqlite/redis/mongodb/postgres/volatile,
│   │                   #   pgvector/qdrant/redisvector, s3media, docparse
│   ├── tui/            # terminal dashboard (bubbletea): sectioned metrics + log footer
│   ├── webui/          # embedded operator console (SPA + JSON API) + docs site (at /docs/)
│   └── vm/             # Luau cgo bridge + embedded prelude
└── third_party/luau/   # git submodule: luau-lang/luau @ 0.731 (full upstream tree; MIT)
```

## Upgrade notes

- **The admin plane always requires a token now, and a public listen needs a real one.**
  `ADMIN_TOKEN` is honoured exactly as before, but a per-boot token is minted whenever it is
  unset — previously that happened only when the console was enabled, so `-no-webui` left
  `/metrics`, `/admin/sessions` and `/admin/credentials` (which provisions and lists encrypted
  per-tenant keys) unauthenticated on whatever address `runtime.admin.listen` named. A per-boot
  token is only a secret if nothing else can reach the port, so a **non-loopback
  `runtime.admin.listen` with no `ADMIN_TOKEN` is now a boot error** instead of a quietly open
  endpoint. Set `ADMIN_TOKEN` in any deployment that binds the admin server to a non-loopback
  address, or that scrapes `/metrics` without a bearer token.
- **Channels authenticate, or they refuse.** Three changes, each closing a path that used to let a delivery through:
  - **`ghhook` requires `secret`.** Its HMAC check returned `true` when no secret was configured, so a channel without one accepted any event posted to its path and handed it to the router as an agent turn. A ghhook channel with no secret is now a boot error; an unresolvable `${VAR}`/`cred:<service>` reference skips the channel at construction rather than running it unauthenticated.
  - **telegram refuses deliveries it cannot authenticate.** A missing webhook secret token (a generation failure — it is minted per boot otherwise) now returns `503` instead of admitting the update. Its `allow_users` list is also checked *before* media is downloaded and written to the blob store, so a non-allowed sender can no longer make the runtime fetch and persist a file.
  - **`webhook`'s `callback_url` is now address-guarded.** It goes through the same outbound guard `http.request` and `builtin:fetch` share, so a caller can no longer have the runtime POST an agent's reply to a private, loopback or link-local address. A deployment whose callback receiver *is* on a private network must set `net.http.allow_private: true` — which relaxes that guard on all three paths at once.
- **Memory stores are private per agent by default.** Two agents on the same memory profile bind `<agent>.<table>` (e.g. `writer.dialogue`), so history cannot leak across agents. Loops are unaffected (table names are the profile's; the prefix is applied at bind time). A deliberately shared store opts in with `shared: true`:
  ```yaml
  profiles:
    memory/team:
      stores:
        kb: { backend: main_db, table: kb, shared: true }
  ```
- **One shared HTTP listener.** Every HTTP channel mounts its `path` on the single `gateway.listen` server (per-channel `listen:` fields no longer exist). Paths must end in `/`; a UUID-style segment is a recommended unguessable secret prefix. Set `gateway.public_url` for Telegram webhook/auto; the shared server serves `GET /health` for probes.

## Notes

- **Log levels** — five additive levels, selected with `-log-level` (default `info`): `error` (runtime cannot continue), `warn` (degraded: upstream 429/502, retries), `info` (lifecycle: launch, channels, webhooks), `debug` (every interaction: LLM API request, memory put/query, telegram update, op dispatch), `dev` (temporary development logs). Setting a level prints it and everything above.
- **Luau is a git submodule**, not vendored code: `third_party/luau` pins upstream `luau-lang/luau` at release **0.731** (the full upstream tree; only `VM`, `Common`, `Ast`, `Bytecode` and `Compiler` are compiled). A fresh clone needs `git clone --recurse-submodules` (or `git submodule update --init`) before `make`. To upgrade: `git -C third_party/luau fetch --tags && git -C third_party/luau checkout <new-tag> && git add third_party/luau`, then `make` and run the test suite.
- **`config.yaml`** is gitignored — it's the local instance config and may contain credentials. Keep secrets in it or in environment variables, never in tracked files.
- Runtime state (sqlite stores) lives under `data/` and is gitignored.

## License

AgentFlow is released under the [MIT License](LICENSE).

The Luau interpreter at `third_party/luau/` (git submodule) is licensed
separately under its own MIT license — see `third_party/luau/LICENSE.txt`.
