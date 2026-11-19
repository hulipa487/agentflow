# AgentFlow examples

## Scenarios

Complete, runnable deployments: a config, its loops and prompts, loop tests
and a README each. Run any of them from the repository root with
`agentflow -config examples/<scenario>/agentflow.yaml`.

| Scenario | What it shows |
| --- | --- |
| [`qq-group-chat/`](qq-group-chat/) | A QQ group chatbot with memory and context: one session per group, speaker-aware prompts, recent context in `session.state`, long-term recall and `/remember` facts. |
| [`telegram-rag/`](telegram-rag/) | A Telegram bot that answers from a knowledge base: embeddings in SQLite, semantic recall, cited answers, and a webhook for bulk loading. |
| [`github-pr-agents/`](github-pr-agents/) | A GitHub pull request webhook that spawns a code agent per PR: the change copied into a gVisor sandbox project, inspected with shell commands, and reviewed in a comment. |
| [`chat-completions/`](chat-completions/) | A simple single-turn OpenAI-compatible `/v1/chat/completions` endpoint backed by an agent. |
| [`minimal/`](minimal/) | The smallest deployment: one webhook and an echo agent, no model needed. |
| [`configdir/`](configdir/) | The config-directory layout: `system.yaml`, `channels.yaml` and one file per agent. |

## Building blocks

| Folder | What is in it |
| --- | --- |
| [`loops/`](loops/) | Single-pattern loops the docs walk through: echo, chat with memory, a multi-round tool loop, streaming, timers, a daemon, delegation, files and OpenAI-channel memory. |
| [`loop-tests/`](loop-tests/) | Loop test files: scripted messages and model replies with expected sends (`agentflow loop test <file>`). |
| [`packages/`](packages/) | A Lua source package and its manifest (`-- af-import: greeting`). |
| [`tools/`](tools/) | A declarative HTTP tool (`tools.http`), no Go code needed. |
| [`prompts/`](prompts/) | A shared system prompt. |

Every loop here is compiled by the test suite, and every scenario config is
validated, so the examples cannot drift from the runtime.
