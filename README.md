# AgentFlow

A single-binary, self-hosted LLM agent runtime written in **Go** with **Luau** scripting (via cgo, built with zig). Configure the ordinary; program the exceptional.

Every agent session is an actor — one goroutine, one mailbox, one Luau state. Lua coroutines park on a single async bridge (`__af_op`) instead of blocking OS threads, so a busy instance stays cheap. YAML wires the topology; Lua plugins carry the behavior.

## Features

- **Actor-model sessions** — per-session Luau state, message-passing only, hot reload of loops and instructions.
- **Multi-agent** — `agent.send` / `agent.request` / `agent.reply` / `agent.spawn`, address authority with `can_contact` ACLs, ephemeral children with budget/lifetime limits.
- **Memory** — provider → backend → store layering; `builtin:conversational` preset; retention/window GC.
- **Tools** — filesystem & git inside shell handles, honest-degradation `web_search`, and MCP stdio servers discovered at boot.
- **Shell** — Docker and SSH providers with resource limits and an exec-policy filter.
- **HTTP** — `http.request` / `os.env` Lua ops with scheme validation, body cap, and secret-header redaction.
- **Credentials** — encrypted-at-rest, per-tenant credential store; loops reference a key by `{service=...}` and Go resolves and injects it at request time.
- **Scheduler** — `scheduler.every/after/cron`; timers arrive as mailbox messages, never a cross-goroutine Luau call.
- **Budget** — per-agent token pools with reserve/commit/release around LLM calls, daily reset.
- **Safety** — core-owned ingress/egress chain (source-attribution, signal-gate, steady-directive, support-offer, affect-guard) that cannot be uninstalled from Lua.
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

The runtime ships as a standalone engine. Reference product apps built on top
of agentflow — for example a full multi-agent orchestrator (main + expert +
project manager + workers), with its own loops, route, and instruction files —
live in a separate repo and consume agentflow as a binary/library. This repo
holds only the runtime and generic, runnable examples.

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
│   ├── core/           # actor, supervisor, router, scheduler, safety, memory, budget, metrics, credentials, ...
│   ├── drivers/        # llm, memory backends, telegram, webhook, shell, mcp
│   ├── builtins/       # embedded Lua builtins (react loop, per_chat route, support chunks)
│   └── vm/             # Luau cgo bridge + embedded prelude
├── plugins/examples/   # example Lua loop plugins
├── examples/           # runnable YAML configs
├── instructions/       # agent system-prompt guidance (see instructions/README.md)
├── docs/               # documentation website (GitHub Pages-ready)
└── third_party/luau/   # vendored Luau 0.731 (MIT, committed)
```

## Notes

- **Vendored Luau** (0.731) is committed under `third_party/luau/`, so the repo builds from a clean clone. To upgrade it, replace that directory with a newer release and re-run `make`.
- **`config.yaml`** is gitignored — it's the local instance config and may contain credentials. Keep secrets in it or in environment variables, never in tracked files.
- Runtime state (sqlite stores) lives under `data/` and is gitignored.
