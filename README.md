<div align="center">
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/brand/logo-banner-darkmode.svg">
  <img src="assets/brand/logo-banner-lightmode.svg" alt="AgentFlow" height="96">
</picture>
<h1>AgentFlow</h1>
<p><i>Configure the ordinary, code the exceptionals.</i></p>
<p>
  <img src="https://img.shields.io/github/v/release/hulipa487/agentflow" alt="Release">
  <img src="https://img.shields.io/github/stars/hulipa487/agentflow" alt="Stars">
  <img src="https://img.shields.io/github/forks/hulipa487/agentflow" alt="Forks">
</p>
</div>


## What is AgentFlow

> TL;DR - A LLM Runtime with Lua programmable, hot reloadable loops

AgentFlow is an open-source, pure-Go single-binary runtime for LLM agents.

Configure all the routines - API keys, LLM Upstreams, Tools, Channels, Memory - with simple YAML, while you enjoy the fun of coding agentic loops in Lua, without sacrificing control! You name it, we've got it.
- [x] Direct r/w control over vectordb?
- [x] User prompts accessible to Lua?
- [x] Custom ReAct loops?
- [x] Per-user credentials your Lua can use but never see?
- [x] Hot reload without dropping a conversation?
- [ ] Flying pigs? (working on it)

Interact with your agents with Telegram, QQ, Feishu/Lark, WeCom and Webhooks, or through our first-class desktop tool **AgentFlow Studio**.

## How it works

```
 Telegram / QQ / Feishu / WeCom / Webhook / Bridge
                │
                ▼
     ┌─────────────────────┐
     │  gateway + router   │   who is this, and which agent gets it?
     └──────────┬──────────┘
                ▼
     ┌─────────────────────┐   one isolated Lua state per conversation,
     │  session (your loop)│   running *your* loop: inbox → think → send
     └──────────┬──────────┘
                ▼
   models · memory · tools · http · files · shell · timers · other agents
        (the engine owns these: keys, quotas, safety, audit)
```

Every conversation gets its own session running your loop. The loop decides *what* happens - which model, what to remember, which tool to call, when to hand off to another agent. The engine decides *whether it may*: it holds the API keys, enforces capabilities and quotas, runs the safety checks, and keeps the records. Your Lua stays small, and nothing it does can leak a key.

## A taste

`agentflow.yaml` - the ordinary:

```yaml
version: "1"

models:
  default:
    provider: openai          # anthropic, gemini, or any OpenAI-compatible server
    model: gpt-4.1-mini
    api_key: ${OPENAI_API_KEY}

agents:
  helper:
    loop: ./loops/helper.lua
    model: default
    instructions: ./prompts/helper.md
    capabilities: [llm.chat]

gateway:
  listen: ":8080"
  channels:
    - name: local
      type: webhook
      agent: helper
      path: /webhook/local/
```

`loops/helper.lua` - the exceptional:

```lua
-- af-prelude-version: 1
function loop()
  while true do
    local msg = session.inbox()
    local reply = llm.chat({
      { role = "system", content = agent.info().instructions },
      { role = "user", content = msg.text or "" },
    })
    session.send(reply.text or "")
  end
end
```

Then:

```bash
./agentflow run
curl -X POST localhost:8080/webhook/local/ \
     -H 'Content-Type: application/json' -d '{"from":"alice","text":"hello"}'
```

Edit the loop while it runs and every live session switches to the new code at its next safe point. A loop that fails to compile is refused, and the old one keeps serving.

## What's in the box

**🧠 Loops in Lua**
- A sandboxed pure-Go Lua 5.1 (`gopher-lua`) state per conversation, with an instruction budget and memory guards.
- One small API: `llm.chat` / `llm.stream` / `llm.embed` / `llm.rerank`, `memory.recall` / `memory.write`, `store.*`, `tools.run`, `http.request`, `files.*`, `shell.run`, `mail.*`, `scheduler.after` / `every` / `cron`, `agent.spawn` / `send` / `request`, `user.*`.
- Built-in plugins for the boring parts: recency and semantic recall, per-chat routing, token budgets.
- `agentflow loop test` runs scripted scenarios against a loop, so loops get tests too.

**🔌 Models and channels**
- Anthropic, OpenAI (Chat Completions and Responses), Gemini, and anything OpenAI-compatible (Ollama, vLLM, LiteLLM, OpenRouter...).
- Streaming, tool calls, thinking/reasoning capture, prompt-cache accounting, multimodal input.
- Telegram, QQ Bot (Tencent's official QQ Open Platform API, over its websocket gateway or signed webhooks), Feishu / Lark (`feishu`: persistent WebSocket or encrypted webhooks), WeCom (`wecom`: encrypted corporate callbacks & active messaging), generic webhooks (sync or async with a callback), GitHub webhooks, a `bridge` channel for forwarding between deployments, and an `openai` channel that makes an agent answer the OpenAI Chat Completions API — clients need only a base URL and a key.

**🗄️ Memory**
- SQLite (vectors built in), PostgreSQL (with or without pgvector), Qdrant, Redis, Redis vector search, and an in-process volatile store.
- Zero config to start: with no memory settings, memory lives in the runtime database.
- Scopes per user, per shared pool, or per agent.

**🧰 Tools and integrations**
- Declarative HTTP tools in YAML, or generate them from an OpenAPI spec with `agentflow tools import-openapi`.
- MCP servers as tool sources, a browser tool (Cloudflare Browser Rendering), web search and page fetch.
- A shell that leaves nothing behind: each command is a one-off [gVisor](https://gvisor.dev) (`provider: gvisor`, Linux) or [microsandbox](https://github.com/superradcompany/microsandbox) (`provider: microsandbox`, Linux KVM / macOS Apple Silicon / Windows WHP) sandbox on a user's project files, with a declared toolchain image, a time limit and no network. Changes are stored when it ends; the next command can run on any instance. SSH is there for real remote machines.
- Media and files on local disk or S3.

**👥 Users and tenancy**
- Profiles that link one person's handles across channels, invites, groups and shared memory pools.
- Per-user model and prompt overrides, and OIDC sign-in.
- An encrypted credential store: a loop calls `http.request{ auth = ... }` and the secret is injected by the engine.
- Daily token quotas per user, usage rollups per user/agent/model, budget pools.

**🛡️ Safety and control**
- Capabilities are declared per agent and enforced: an agent without `net.http` cannot make a request.
- Safety profiles and classifiers on input and output, a network guard on outbound calls, and an audit trail.
- Every message carries where it came from, and tenancy is checked from that record, never from what the Lua claims.

**📊 Operations**
- Pure headless runtime — one HTTP port for everything: the channels, `/healthz` / `/readyz` / `/metrics`, and the authenticated `/admin/api/` JSON & SSE API that **AgentFlow Studio** connects to.
- `doctor` checks (with `-online` probes of every backend), redacted support bundles, hot reload of loops and config, and live model/agent edits from Studio.

## Deployment, from laptop to planet

| Tier | Shape | Stores |
|---|---|---|
| `single` | One process | SQLite, or PostgreSQL. A second instance on the same store is refused. |
| `regional` | Several instances in one region | Shared PostgreSQL (+ pgvector), optional Redis, S3 |
| multi-region | One independent `regional` cell per region | The app in front pins each user to a region |

In a regional cell each conversation is owned by exactly one instance. Messages that land elsewhere are queued for the owner, a dead instance's sessions move to the others, an instance that loses the database stops its sessions before anyone else takes them, and a graceful shutdown drains and hands over. Moving up a tier is a copy, not a rewrite:

```bash
agentflow migrate store -from sqlite://./data/agentflow.db -to postgres://...
agentflow migrate blobs ./data/media -kind media
agentflow user export u_abc -pubkey eu-move.pub -out u_abc.zip   # move one user between cells
```

## Getting Started

Run the following auto-install script:

```bash
curl -fsSL agentflow.hulipa.my | sh
```

OR build from source — AgentFlow is 100% pure Go (`CGO_ENABLED=0`), so only Go 1.26+ is needed on Windows, macOS, or Linux (no C/C++ toolchain, no git submodules):

```bash
git clone https://github.com/hulipa487/agentflow.git
cd agentflow
go build -o agentflow ./cmd/agentflow   # or `make`
./agentflow run -config agentflow.example.yaml
```

Starting your own deployment:

```bash
agentflow init mybot             # a runnable starter: config + an echo loop
agentflow doctor -config mybot   # check config, loops, keys, models, channels
agentflow run -config mybot      # go
```

## The CLI at a glance

| Command | What it does |
|---|---|
| `agentflow run` | Run the engine (plain `agentflow` does the same) |
| `agentflow init [DIR]` | Write a starter deployment |
| `agentflow validate` / `doctor` | Compile every loop / check everything, `-online` to probe backends |
| `agentflow config schema \| path \| show` | JSON schema for editors, the resolved config, a summary |
| `agentflow loop check \| test` | Compile a loop / run a scenario against it |
| `agentflow tools import-openapi SPEC` | Turn an OpenAPI spec into HTTP tools |
| `agentflow cred set \| get \| list \| delete` | Manage stored credentials (values never go on argv) |
| `agentflow migrate store \| blobs` | Copy state to another backend |
| `agentflow user keygen \| export \| import` | Move one person between deployments |
| `agentflow support-bundle` | Collect redacted diagnostics |

`agentflow help <command>` prints a command's flags, generated from the binary itself.

## AgentFlow Studio & Documentation

AgentFlow itself is strictly a headless runtime; all UI, full documentation, and runnable examples live in [**AgentFlow Studio**](https://github.com/hulipa487/agentflow-studio). Connect Studio (desktop or web) to any deployment's `/admin/api/` endpoint to get an overview of sessions and instances, an agent builder with a Lua editor and analyzer, a playground with execution traces, and screens for models, channels, safety, users, credentials, usage, and logs. The complete guides (getting started, architecture, configuration reference, Lua API reference, deployment guides) ship inside Studio offline, and full runnable scenarios & single-pattern loops live under Studio's [`examples/`](https://github.com/hulipa487/agentflow-studio/tree/main/examples).

## For developers

| Path | What's there |
|---|---|
| `cmd/agentflow` | The binary: engine wiring and the CLI |
| `internal/vm` | The pure-Go Lua 5.1 VM (`gopher-lua`) and the prelude every loop runs on |
| `internal/adminapi` | The headless `/admin/api/*` JSON & SSE control plane for AgentFlow Studio |
| `internal/core` | Sessions, supervisor, router, memory, leases, quotas, identity, safety... |
| `internal/drivers` | Models, channels, memory backends, sandbox (`gVisor` & `microsandbox`), MCP, browser, S3... |
| `internal/luacode` | Lua source, package (`-- af-import:`), and shadow loader |
| `internal/builtins` | Core-contract embedded Lua defaults (router, memory handlers, `token_budget`) |
| `api/` | The contracts: the users OpenAPI spec and the Lua type stubs (`af.d.lua`) |

`go test ./...` and `go vet ./...` (or `make test` / `make vet`) work out of the box with `CGO_ENABLED=0`. The core and the Lua API move together: every Lua file declares the prelude version it targets (`-- af-prelude-version: 1`), and a mismatch is refused at load instead of half-working.

## Why exactly AgentFlow?
- **Full Transparency**: All model parameters are within your control.
- **Ease of Development**: High level Lua language with unified calls.
- **Safe by Default**: Keys, quotas and capabilities stay with the engine, not the script.
- **Scales When You Do**: Same config and loops from one laptop to a cell per region.
- **Cross Platform**: Windows, MacOS, Linux, we've got you covered.

## Contributors
<a href="https://github.com/hulipa487/agentflow/graphs/contributors">
  <img src="https://contrib.rocks/image?repo=hulipa487/agentflow" />
</a><p></p>

**LICENSE**: AGPLv3

Made with Vibe and ❤️ by hulipa and her team.
