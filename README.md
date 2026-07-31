# AgentFlow

A single-binary, self-hosted LLM agent runtime written in **Go** with **Luau** scripting (via cgo, built with zig). Configure the ordinary; program the exceptional.

Every agent session is an actor — one goroutine, one mailbox, one Luau state. Lua coroutines park on a single async bridge (`__af_op`) instead of blocking OS threads, so a busy instance stays cheap. YAML wires the topology; Lua plugins carry the behavior.

## Features

- **Actor-model sessions** — per-session Luau state, message-passing only, hot reload of loops and instructions.
- **Multi-agent** — `agent.send` / `agent.request` / `agent.reply` / `agent.spawn`, address authority with `can_contact` ACLs, ephemeral children with budget/lifetime limits.
- **Memory** — provider → backend → store layering; `builtin:conversational` preset; retention/window GC.
- **Tools** — filesystem & git inside shell handles, honest-degradation `web_search`, and MCP stdio servers discovered at boot.
- **Shell** — Docker and SSH providers with resource limits and an exec-policy filter.
- **Scheduler** — `scheduler.every/after/cron`; timers arrive as mailbox messages, never a cross-goroutine Luau call.
- **Budget** — per-agent token pools with reserve/commit/release around LLM calls, daily reset.
- **Safety** — core-owned ingress/egress chain (source-attribution, signal-gate, steady-directive, support-offer, affect-guard) that cannot be uninstalled from Lua.
- **Multi-agent** — static agents with per-model config + `can_contact` ACLs, ephemeral children via spawn profiles, and correlated RPC (`agent.request`/`agent.reply`); an orchestrator pattern (main + experts + project manager + workers) ships under `plugins/orchestrator/`.
- **Observability** — `/healthz`, `/readyz`, `/metrics`, `/v1/sessions` on loopback with optional bearer-token auth.

### Driver set

| Domain | Providers |
|---|---|
| LLM | `anthropic` (Messages API), `openai` (Chat Completions), `openai-responses` (Responses API) — all pointed at compatible endpoints via `base_url` |
| Storage | SQLite, Redis, MongoDB, PostgreSQL |
| Vector | pgvector (cosine similarity) |
| Channels | webhook, Telegram (polling + webhook) |
| Shell | Docker, SSH |
| Tools | builtins + MCP stdio |

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
make run        # build, then run (CONFIG=examples/agentflow.minimal.yaml by default)
make clean      # remove build objects, static lib, and binary
```

The Makefile detects the host via `go env GOHOSTOS`/`GOHOSTARCH` and picks the compiler, archiver, and (on Windows) zig's `-target` triple accordingly. To cross-compile instead, set `GOOS`/`GOARCH` and a matching zig `-target` manually — native builds on each target OS are the recommended path.

## Quick start

No model needed — the minimal example replies with a file-based echo loop:

```bash
./agentflow -config examples/agentflow.minimal.yaml
curl -X POST localhost:8080/webhook -d '{"from":"alice","text":"hello"}'
```

For an LLM-backed bot, point a compatible endpoint at it (OpenAI-compatible local endpoints like Ollama work out of the box):

```yaml
models:
  default:
    provider: openai
    model: gpt-4o-mini
    base_url: http://127.0.0.1:11434/v1   # Ollama / vLLM / LiteLLM / OpenRouter
    api_key: ${OPENAI_API_KEY}            # may be empty for keyless local endpoints
```

## Examples

| Config | Demonstrates |
|---|---|
| `examples/agentflow.minimal.yaml` | Webhook + file-based echo loop, no LLM |
| `examples/agentflow.compatible.yaml` | Anthropic/OpenAI-compatible model endpoints |
| `examples/agentflow.keyless-chat.yaml` | Local keyless model over webhook |
| `examples/agentflow.memory-default.yaml` | `builtin:conversational` memory (auto sqlite backend) |
| `examples/agentflow.memory-tools.yaml` | Memory + tools + webhook |
| `examples/agentflow.stream.yaml` | `llm.stream` delta-by-delta consumption |
| `examples/agentflow.shell-tools.yaml` | Shell (Docker) + filesystem/git tools |
| `examples/agentflow.telegram.yaml` | Anthropic + Telegram polling |
| `examples/agentflow.openai-telegram.yaml` | OpenAI-compatible + Telegram |
| `examples/agentflow.orchestrator.yaml` | Multi-agent: main + expert + PM + workers |

## Multi-agent orchestration

The `plugins/orchestrator/` example wires four agent roles, each with its own model and system prompt:

- **Main** (`main.lua`) — the only user-facing agent. Chats, consults experts, and launches projects. Emits fenced-JSON directives the runtime acts on, then re-asks the LLM for the final reply.
- **Expert** (`expert.lua`) — static, channel-less. Receives a question via `agent.request`, returns a second opinion via `agent.reply`; never talks to the user.
- **Project manager** (`pm.lua`) — one ephemeral instance per project (`agent.spawn("project_manager")`). Decomposes the brief into tasks, spawns workers, dispatches them, and reports milestones/decisions back to main.
- **Worker** (`worker.lua`) — ephemeral, one task at a time. Produces an artifact and reports to its PM; exits on shutdown.

Background updates reach the user through `session.push` (a new capability-gated op for proactive channel egress), and finished agents clean up with `session.exit`. See `examples/agentflow.orchestrator.yaml` for the wiring and `instructions/` for the role prompts.

```bash
./agentflow -config examples/agentflow.orchestrator.yaml
curl -X POST localhost:8080/webhook -d '{"from":"alice","text":"plan and write a fizzbuzz script"}'
```

## Documentation

Full docs (project summary, architecture, config reference, Lua plugin design, hook specs, capability API) are a static site in [`docs/`](docs/). Serve locally:

```bash
cd docs && python -m http.server 8888
# open http://localhost:8888
```

On GitHub, this can be published via GitHub Pages → "Deploy from a branch" → `docs/` folder.

## Repository layout

```
agentflow/
├── Makefile            # build: vendored Luau -> libluau.a -> agentflow binary
├── cmd/agentflow/      # main entrypoint
├── internal/           # core runtime + drivers (not importable — internal module)
│   ├── core/           # actor, supervisor, router, scheduler, safety, memory, budget, metrics, ...
│   ├── drivers/        # llm, memory backends, telegram, webhook, shell, mcp
│   ├── builtins/       # embedded Lua builtins (react loop, per_chat route, support chunks)
│   ├── plugins/examples/   # example Lua loop plugins
│   ├── plugins/orchestrator/ # multi-agent: main + expert + PM + workers
│   └── vm/             # Luau cgo bridge + embedded prelude
├── examples/           # runnable YAML configs
├── instructions/       # agent system-prompt files (incl. orchestrator roles)
├── docs/               # documentation website (GitHub Pages-ready)
└── third_party/luau/   # vendored Luau 0.731 (MIT, committed)
```

## Notes

- **Vendored Luau** (0.731) is committed under `third_party/luau/`, so the repo builds from a clean clone. To upgrade it, replace that directory with a newer release and re-run `make`.
- **`config.yaml`** is gitignored — it's the local instance config and may contain credentials. Keep secrets in it or in environment variables, never in tracked files.
- Runtime state (sqlite stores) lives under `data/` and is gitignored.
