<div align="center">
<h1>
🤖 AgentFlow
<p>
</h1>
<i> Configure the ordinary, code the exceptionals. <p></p></i>
<img src="https://img.shields.io/github/v/release/hulipa487/agentflow">
<img src="https://img.shields.io/github/stars/hulipa487/agentflow">
<img src="https://img.shields.io/github/forks/hulipa487/agentflow"><p></p>
<img width="1444" height="691" alt="image" src="https://github.com/user-attachments/assets/0817ad4c-da1e-4502-9aaf-3bf15dca0394" />
</div>


## What is AgentFlow

> TL;DR - A LLM Runtime with Lua programmable, hot reloadable loops

AgentFlow is an open-source, single-binary Go runtime for LLM agents.

Configure all the routines - API keys, LLM Upstreams, Tools, Channels, Memory - with simple YAML, while you enjoy the fun of coding agentic loops in simple Lua (Luau, to be exact), and without sacrificing controls too! You name it, we've got it.
- [x] Direct r/w control over vectordb?
- [x] User prompts accessible to Lua?
- [x] Custom ReAct loops?
- [x] Per-user credentials your Lua can use but never see?
- [x] Hot reload without dropping a conversation?
- [ ] Flying pigs? (working on it)

Interact with your agents with Telegram and Webhooks, or through our first-class desktop tool **AgentFlow Studio**.

## How it works

```
 Telegram / Webhook / GitHub / Bridge
                │
                ▼
     ┌─────────────────────┐
     │  gateway + router   │   who is this, and which agent gets it?
     └──────────┬──────────┘
                ▼
     ┌─────────────────────┐   one isolated Luau state per conversation,
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

**🧠 Loops in Luau**
- A sandboxed Lua state per conversation, with a memory cap and an instruction budget.
- One small API: `llm.chat` / `llm.stream` / `llm.embed` / `llm.rerank`, `memory.recall` / `memory.write`, `store.*`, `tools.run`, `http.request`, `files.*`, `shell.exec`, `mail.*`, `scheduler.after` / `every` / `cron`, `agent.spawn` / `send` / `request`, `user.*`.
- Built-in plugins for the boring parts: recency and semantic recall, fact extraction, per-chat routing, token budgets.
- `agentflow loop test` runs scripted scenarios against a loop, so loops get tests too.

**🔌 Models and channels**
- Anthropic, OpenAI (Chat Completions and Responses), Gemini, and anything OpenAI-compatible (Ollama, vLLM, LiteLLM, OpenRouter...).
- Streaming, tool calls, thinking/reasoning capture, prompt-cache accounting, multimodal input.
- Telegram, generic webhooks (sync or async with a callback), GitHub webhooks, a `bridge` channel for forwarding between deployments, and an `openai` channel that makes an agent answer the OpenAI Chat Completions API — clients need only a base URL and a key.

**🗄️ Memory**
- SQLite (vectors built in), PostgreSQL (with or without pgvector), Qdrant, Redis, Redis vector search, and an in-process volatile store.
- Zero config to start: with no memory settings, memory lives in the runtime database.
- Scopes per user, per shared pool, or per agent.

**🧰 Tools and integrations**
- Declarative HTTP tools in YAML, or generate them from an OpenAPI spec with `agentflow tools import-openapi`.
- MCP servers as tool sources, a browser tool (Cloudflare Browser Rendering), web search and page fetch.
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
- One HTTP port for everything: the channels, and the admin plane under `/admin/` - an API for state, metrics, logs, agents, models, users, credentials and usage, plus a small web console.
- A terminal dashboard, `doctor` checks (with `-online` probes of every backend), redacted support bundles.
- Hot reload of loops and config; live model edits from Studio.

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

Run the following auto-install script.

```bash
curl -fsSL agentflow.hulipa.my | sh
```

OR

Build from source. You need Go 1.26+, GNU make, and a C/C++ toolchain (gcc on Linux, Xcode's clang on macOS, [zig](https://ziglang.org) on Windows) for the vendored Luau:

```bash
git clone --recurse-submodules https://github.com/hulipa487/agentflow.git
cd agentflow
make
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

## AgentFlow Studio

The desktop companion: connect to any deployment and get an overview of sessions and instances, an agent builder with a Luau editor and analyzer, a playground with execution traces, and screens for models, channels, safety, users, credentials, usage and logs. The full documentation - configuration reference, Luau API reference, deployment guides - ships inside Studio and works offline.

## For developers

| Path | What's there |
|---|---|
| `cmd/agentflow` | The binary: engine wiring and the CLI |
| `internal/vm` | The Luau VM (cgo) and the prelude every loop runs on |
| `internal/core` | Sessions, supervisor, router, memory, leases, quotas, identity, safety... |
| `internal/drivers` | Models, channels, memory backends, MCP, browser, S3... |
| `internal/builtins` | The built-in Lua plugins |
| `api/` | The contracts: the users OpenAPI spec and the Luau type stubs (`af.d.luau`) |
| `examples/` | Loops, configs, scenarios and a package to start from |

`make test` and `make vet` run with the right cgo toolchain for your OS. The core and the Lua API move together: every Lua file declares the prelude version it targets (`-- af-prelude-version: 1`), and a mismatch is refused at load instead of half-working.

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

**LICENSE**: MIT

Made with Vibe and ❤️ by hulipa and her team.
