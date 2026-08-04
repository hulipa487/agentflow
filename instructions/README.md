# Agent instructions

An agent's **instructions** are its system prompt — the text loaded into the
LLM as the leading `system` message on every turn. agentflow loads them from a
plain markdown file per agent.

## Wiring

Point an agent at its instructions file in the config:

```yaml
agents:
  main:
    model: default
    instructions: ./instructions/main.md   # any path; the file is read at boot
    loop: ./plugins/examples/echo.lua
    capabilities: [llm.chat]
```

At boot the runtime reads the file and stores it on the agent's info; each
`llm.chat` the loop issues gets it prepended as the system message. The file is
hot-reloaded when `runtime.reload.watch: true` (edits take effect on the next
turn, no restart).

## What to put in them

Write instructions as you would any system prompt — plain prose, markdown is
fine. Keep them:

- **Role-specific** — one file per agent role (a user-facing main agent, a
  background worker, an expert). Don't share one file across roles with
  different jobs.
- **Behavioral, not infrastructural** — describe *what* the agent should do and
  its style/tone, not how the runtime works. The runtime's capabilities (tools,
  memory, shell, inter-agent messaging) are surfaced to the loop, not the model;
  the model sees tools and the system prompt.
- **Honest about tools** — if the agent has tools (fs.read, an API tool, etc.),
  name them and say when to use them. If it has none, don't promise any.

## Example

```markdown
# Greeter

You are a friendly greeter. You only talk to the user via the webhook channel.

## Your job
- Greet the user by name when you can infer it.
- Keep replies to one short sentence.
- If you don't know something, say so — never invent details.
```

## Notes

- Product/reference apps (e.g. a full multi-agent orchestrator with main /
  expert / project-manager / worker roles) ship their own instruction files in
  their own repo; this directory holds only this generic guidance for the
  runtime itself.
- Secrets never belong in instructions — use `os.env` from a loop, or the
  credential store (`auth={service=...}` on `http.request`).
