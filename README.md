# AgentFlow

A single-binary, self-hosted LLM agent runtime written in **Go** with **Luau** scripting (via cgo, built with zig). Configure the ordinary; program the exceptional.

Every agent session is an actor — one goroutine, one mailbox, one Luau state. Lua coroutines park on a single async bridge (`__af_op`) instead of blocking OS threads, so a busy instance stays cheap. YAML wires the topology; Lua plugins carry the behavior.

## Features

- **Actor-model sessions** — per-session Luau state, message-passing only, hot reload of loops and instructions.
- **Multi-agent** — `agent.send` / `agent.request` / `agent.reply` / `agent.spawn`, address authority with `can_contact` ACLs, ephemeral children with budget/lifetime limits.
- **Memory** — provider → backend → store layering; `builtin:conversational` preset; retention/window GC. Stores are isolated per agent by default (`<agent>.<table>`); `shared: true` opts a store into deliberate cross-agent sharing.
- **Embeddings & reranking** — `llm.embed` (OpenAI-compatible `/embeddings`) and `llm.rerank` (Jina/Cohere/TEI/vLLM `/rerank`); pgvector ingest on write and a `builtin:semantic` recall pipeline (embed → vector k-NN → rerank). Multimodal embeddings follow the Jina convention (`jina-embeddings-v5-omni-small`: text + image/video/audio/pdf in one vector space); `memory.write` embeds attachment-carrying records as one merged vector.
- **Tools** — filesystem ops inside shell handles, a multi-engine `web_search` tool (Doubao / Ollama / StackOverflow / GitHub / YouTube, selected per call via `engine`; honest-degradation when unconfigured), a `legal_search` / `legal_fetch` pair over HKLII (Hong Kong case law + legislation) and the NPC China national laws database, and MCP stdio servers discovered at boot. Deployments retitle/re-document any tool via `tools.policy.overrides` (description + per-param descriptions, boot-validated against the registry), and loops declare their own model-visible tools in Lua with `tool.def` (merged into `tools.list`, dispatched by `tools.run`).
- **Shell** — Docker and SSH providers with resource limits and an exec-policy filter. An agent with a bound shell profile gets `builtin:shell.exec` / `builtin:shell.write` / `builtin:shell.destroy` registry tools that lazily spawn and reuse its handle — no Lua glue; gated on the `shell.exec` capability.
- **HTTP** — `http.request` / `os.env` Lua ops with scheme validation, body cap, and secret-header redaction.
- **Mail** — `mail.imap_fetch` / `mail.smtp_send` Lua ops (cap `net.mail`); passwords resolve from the credential store at call time and never cross the Lua bridge.
- **Multimodal** — channels ingest media into a content-addressed blob store (per-channel allow-list + size ceiling; local filesystem or S3/MinIO backend via the top-level `media:` config); loops forward part descriptors into `llm.chat` and the runtime resolves them at request time. Every provider covers images + PDFs; audio on the OpenAI shapes (input_audio) and Gemini; video via the MiniMax/Kimi/GLM `video_url` convention on the OpenAI shapes, a video block on Anthropic, and the Gemini Files API (resumable upload, referenced by URI, auto-deleted after 48h) on Gemini. Opt-in via `media:` on a channel.
- **Audit journal** — core-owned `message_journal` in the runtime store: every inbound (router) and outbound (session egress) message is recorded with channel, sender, agent, session, text, attachment handles, and delivery status — loops and channels can neither bypass nor forge it. Retention is configurable (`audit.retention_days`, default 90, 0 = forever).
- **Credentials** — encrypted-at-rest, per-tenant credential store; loops reference a key by `{service=...}` and Go resolves and injects it at request time. Secret config fields (`models.*.api_key`, channel tokens, backend urls/passwords, …) accept literals, `${VAR}`, or `cred:<service>`; resolution is env → store → honest degradation (optional components skip with a warning naming the credential; a model key fails the first LLM call clearly).
- **Web console** — embedded single-page operator UI on the admin server (no build step): hot model management with test/persist, a validated config editor, an API-key manager over the credential store, live sessions, and in-process metrics sparklines. Token-authenticated; secrets are write-only.
- **Scheduler** — `scheduler.every/after/cron`; timers arrive as mailbox messages carrying `payload.timer_id` (multiple independent timers per session), never a cross-goroutine Luau call. Daemon agents (`persistent: true`) boot on a synthetic message instead of waiting for traffic.
- **Config directories** — `-configdir <dir>` loads `system.yaml` + `channels.yaml` + `profiles/*.yaml` (one agent per file, `spawn: true` routes to `profiles.agent`) + `triggers/*.yaml` instead of a single `-config` file; same strict validation after merge, and the directory is the base for every relative path.
- **Budget** — per-agent token pools with reserve/commit/release around LLM calls; daily reset or rolling-window accounting; spawn profiles get their own shared pool.
- **Safety** — core-owned ingress/egress chain (source-attribution, signal-gate, steady-directive, support-offer, affect-guard) that cannot be uninstalled from Lua.
- **Observability** — `/healthz`, `/readyz`, `/metrics`, `/v1/sessions` on loopback; an embedded web console on the admin server (token-authenticated, `-no-webui` to disable); shared channel listener serves `GET /health`.

### Driver set

| Domain | Providers |
|---|---|
| LLM | `anthropic` (Messages API), `openai` (Chat Completions + Embeddings), `openai-responses` (Responses API), `gemini` (Interactions API), `rerank` (cross-encoder rerank) — all pointed at compatible endpoints via `base_url` |
| Storage | SQLite, Redis, MongoDB, PostgreSQL, in-memory volatile |
| Media store | local filesystem (default), S3 / MinIO (hand-rolled SigV4, no AWS SDK) — content-addressed `media:<sha256>` handles |
| Vector | pgvector (cosine similarity) |
| Channels | webhook (sync reply with configurable `timeout`, or `async: true` fire-and-poll via `GET <path>result/<id>` / `callback_url`), GitHub webhook (`ghhook`), Telegram (polling + webhook + auto) |
| Mail | IMAP fetch + SMTP send (in-process, cap `net.mail`) |
| Shell | Docker, SSH |
| Tools | builtins + MCP stdio |
| Web search | Doubao (Volcano Engine), Ollama (hosted web search), StackOverflow (StackExchange API), GitHub (repository search), YouTube (Data API v3) — one `builtin:web_search` tool, per-call `engine` param; honest-unavailable when unconfigured |
| Legal | HKLII (Hong Kong case law + legislation, with citations) and NPC (China National Database of Laws and Regulations) — `builtin:legal_search` + `builtin:legal_fetch` (full text, extracted from Word docs via the built-in parser); free, no key |

## Requirements

- Go 1.25+
- GNU make
- A C/C++ toolchain for the Luau cgo bridge — the Makefile auto-selects by host OS:
  - **Windows**: [zig](https://ziglang.org/) (hermetic; no system gcc needed)
  - **Linux**: system gcc (`cc`/`c++`/`ar`)
  - **macOS**: Xcode/clang (`clang`/`clang++`/`ar`)

Luau is vendored under `third_party/luau/` (currently **0.731**, MIT license — see `third_party/luau/LICENSE.txt`) and is committed to the repo, so a fresh clone builds without fetching anything.

## Build

```bash
make            # compile vendored Luau -> internal/vm/lib/libluau.a, then the agentflow binary
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
    server_tools: []                      # provider-native tools, e.g. [google_search] on gemini
```

`provider` selects the request/response shape (`anthropic` | `openai` | `openai-responses` | `gemini` | `rerank`); `base_url` selects the host. `server_tools` injects provider-native, server-side tools (e.g. Google Search grounding on `gemini`, `web_search` on `openai-responses`) that run inside the provider's completion.

The runtime ships as a standalone engine. Reference product apps built on top
of agentflow — for example a full multi-agent orchestrator (main + expert +
project manager + workers), with its own loops, route, and instruction files —
live in a separate repo and consume agentflow as a binary/library. This repo
holds only the runtime and generic, runnable examples.

## Documentation

Full docs (install guide, architecture, config reference, comprehensive Lua plugin development guide, hook specs, capability API) are embedded in the binary and served by the web console at **`/docs/`**:

```bash
./agentflow -config config.yaml
# open http://127.0.0.1:9090/docs/
```

No separate server or build step — the site is `go:embed`ded from `internal/webui/docs/` and readable without the admin token.

## Repository layout

```
agentflow/
├── Makefile            # build: vendored Luau -> libluau.a -> agentflow binary
├── cmd/agentflow/      # main entrypoint
├── internal/           # core runtime + drivers (not importable — internal module)
│   ├── core/           # actor, supervisor, router, scheduler, safety, memory, budget, metrics, credentials, ...
│   ├── drivers/        # llm, memory backends, telegram, webhook, ghhook, httpd, shell, mcp
│   ├── builtins/       # embedded Lua builtins (per_chat route + support chunks)
│   ├── tui/            # terminal dashboard (bubbletea): sessions, stats, logs
│   ├── webui/          # embedded operator console (SPA + JSON API) + docs site (at /docs/)
│   └── vm/             # Luau cgo bridge + embedded prelude
└── third_party/luau/   # vendored Luau 0.731, trimmed to the 5 compiled components (MIT, committed)
```

## Breaking changes

### Memory stores are private per agent by default

Two agents on the same memory profile (including `builtin:conversational`) no
longer share physical tables — each binds `<agent>.<table>` (e.g.
`writer.dialogue`), so conversation history cannot leak across agents. Loops
are unaffected (they use the profile's table names; the prefix is applied at
bind time).

**Migration:** a store that is deliberately shared across agents (a common
knowledge base) must opt in:

```yaml
profiles:
  memory/team:
    stores:
      kb: { backend: main_db, table: kb, shared: true }
```

Rows written before this change live under the old unprefixed table; set
`shared: true` to keep reading them, or migrate the rows.

### Unified webhook listener (per-channel `listen` removed)

All HTTP channels now mount paths on **one shared HTTP server** instead of each
binding its own port. The per-channel `listen:` field is gone — configs that
still carry it fail to load with
`field listen not found in type config.Channel`.

**Before** (one server per channel):

```yaml
gateway:
  channels:
    - type: webhook
      listen: ":8080"        # removed
      path: /webhook
      agent: bot
    - type: ghhook
      listen: ":8081"        # removed — was needed to dodge port clashes
      path: /hooks/github
      agent: bot
```

**After** (one listener, many paths):

```yaml
gateway:
  listen: ":8080"            # the single shared listener (default :8080)
  public_url: ""             # optional external base, e.g. https://bot.example.com
  channels:
    - type: webhook
      path: /webhook/dev/<uuid>/   # paths must end in /
      agent: bot
    - type: ghhook
      path: /webhook/github/<uuid>/
      agent: bot
```

Migration checklist:

- Move each channel's `listen:` value to `gateway.listen` (one value now; pick
  one port — behind a reverse proxy, loopback is typical).
- Give every HTTP channel a `path` ending in `/`; the UUID-style segment is an
  unguessable secret prefix (recommended — with an empty default the subtree
  stays open).
- Set `gateway.public_url` to your externally-reachable base if you want
  webhooks registered for you. Telegram's new `mode: auto` health-probes
  `<public_url>/health` at startup and calls `setWebhook` when reachable,
  falling back to `deleteWebhook` + long-poll when not.
- The shared server serves `GET /health` → `200 {"ok":true}` for probes and
  reverse-proxy health checks.

## Notes

- **Log levels** — five additive levels, selected with `-log-level` (default `info`): `error` (runtime cannot continue), `warn` (degraded: upstream 429/502, retries), `info` (lifecycle: launch, channels, webhooks), `debug` (every interaction: LLM API request, memory put/query, telegram update, op dispatch), `dev` (temporary development logs). Setting a level prints it and everything above.
- **Vendored Luau** (0.731) is committed under `third_party/luau/`, so the repo builds from a clean clone. Only the five components the build compiles are vendored (`VM`, `Common`, `Ast`, `Bytecode`, `Compiler`, plus their includes and the license files) — the upstream tests/bench/CLI/CodeGen/etc. are not. To upgrade, replace those components with a newer release and re-run `make`.
- **`config.yaml`** is gitignored — it's the local instance config and may contain credentials. Keep secrets in it or in environment variables, never in tracked files.
- Runtime state (sqlite stores) lives under `data/` and is gitignored.

## License

AgentFlow is released under the [MIT License](LICENSE).

The vendored Luau interpreter under `third_party/luau/` is licensed separately
under its own MIT license — see `third_party/luau/LICENSE.txt` (Copyright
Roblox Corporation and Lua.org/PUC-Rio).
