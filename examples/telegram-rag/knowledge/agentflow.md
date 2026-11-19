AgentFlow is a self-hosted runtime for LLM agents. Each agent is a Luau loop
that receives messages from channels, calls models and tools through
capability-gated operations, and replies. The runtime owns safety filters, the
audit journal and provenance, so a loop cannot remove or forge them.

Channels connect agents to the outside world: Telegram, QQ Bot, generic
webhooks (synchronous or asynchronous), GitHub webhooks, a bridge for other
platforms, and an OpenAI-compatible chat/completions endpoint. All HTTP
channels share one listener, set by gateway.listen.

Memory is configured per agent as a profile of named stores. A store is a
table on a backend such as SQLite, Postgres, pgvector, Qdrant or Redis.
memory.write routes records to stores, and memory.recall reads them back by
recency, full text or meaning (vector search with an embedding model).

Shell commands run as one-off gVisor sandbox jobs. A job gets a declared
environment as its read-only root filesystem and one project from the files
store at /workspace, runs under a time limit with no network, and whatever it
writes into the project is stored when it ends.

AgentFlow Studio is the companion desktop and web app. It builds agents, runs
them in a playground with a live trace, edits channels, models and safety
profiles, and shows usage and logs.
