# QQ group chatbot with memory and context

A bot for QQ groups that talks like a member: it knows who said what, follows
the recent conversation, remembers older exchanges and facts the group taught
it, and answers in Markdown.

| | |
| --- | --- |
| Channel | `qq`, websocket mode, `scenes: [group]` |
| Loop | [`loops/group_chat.lua`](loops/group_chat.lua) |
| Memory | SQLite: `qq_dialogue` (every exchange), `qq_facts` (`/remember`) |
| Needs | a QQ bot (q.qq.com) and any OpenAI-compatible model |

## How it works

QQ delivers a group message to a bot only when someone @-mentions it. The
router keeps **one session per group** (`group:<group_openid>`), so each group
has its own conversation. For every message the loop builds the prompt from:

1. **Context** — the group's last 10 exchanges, word for word, kept in
   `session.state`. A quiet group's session is unloaded after 30 minutes
   (`runtime.sessions.idle_unload`); the context survives that, and restarts.
2. **Memory** — older exchanges from the same group that match the new
   message, found by full-text recall over `qq_dialogue`.
3. **Facts** — what members asked the bot to keep with `/remember …`.

Every line in the prompt is labelled with the speaker's group nickname, so the
model can tell members apart. `/forget` clears the recent context.

## Run it

1. Create a bot at [q.qq.com](https://q.qq.com), add it to a group (or to a
   sandbox group while it is unpublished), and add your server's outgoing IP
   to the console's IP allowlist.
2. Export the keys and start the runtime from the repository root:

   ```bash
   export OPENAI_API_KEY=sk-...  QQ_APPID=102xxxxxx  QQ_APPSECRET=...
   agentflow -config examples/qq-group-chat/agentflow.yaml
   ```

   The log shows `qq gateway ready`. Websocket mode connects out, so nothing
   has to be reachable from the internet.
3. In the group: `@bot what should we cook tonight?`, then
   `@bot /remember Bob is vegetarian`, then ask again.

`CHAT_MODEL` and `OPENAI_BASE_URL` point the model at another
OpenAI-compatible endpoint (Ollama, vLLM, OpenRouter, …).

## Make it yours

- **Persona** — [`prompts/persona.md`](prompts/persona.md).
- **Private chats too** — `scenes: [group, c2c]`; each private chat becomes
  its own session with the same loop.
- **Semantic memory** — give the memory profile an `embed_model` and
  `recall: plugin:semantic` (see the Telegram RAG example) to recall by
  meaning instead of matching words.
- **No Markdown permission** — leave `markdown: true`: a refused Markdown
  reply is resent as plain text and Markdown pauses for 10 minutes.

## Test it

```bash
agentflow loop test examples/qq-group-chat/tests/group_chat.yaml
```
