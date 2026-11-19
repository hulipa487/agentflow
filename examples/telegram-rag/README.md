# Telegram bot with RAG

A Telegram bot that answers questions from a knowledge base you load into it,
cites the passages it used, and says so when it doesn't know — instead of
making something up.

| | |
| --- | --- |
| Channels | `telegram` (polling), `webhook` for bulk loading |
| Loop | [`loops/rag.lua`](loops/rag.lua) |
| Memory | SQLite vector store `knowledge`, `recall: plugin:semantic` |
| Needs | a Telegram bot token and an OpenAI-compatible chat + embedding model |

## How it works

- **Loading.** `/learn <text>` splits the text into passages at paragraph
  breaks (about 900 characters each) and stores them; the memory profile's
  `embed_model` attaches each passage's embedding as it is written. SQLite
  holds the vectors, so no extra database is needed.
- **Answering.** A question is embedded and the closest passages come back
  through `memory.recall`. The model answers from those passages only and
  cites them as `[1]`, `[2]`. With nothing relevant, the bot replies that it
  doesn't know without calling the model at all.
- **Who can teach it.** Only editors: Telegram users listed under
  `agents.rag.extras.editors`, and the ingest webhook, whose unguessable path
  is the authorisation.

## Run it

1. Talk to [@BotFather](https://t.me/BotFather) to create a bot and get its
   token.
2. Change the ingest path in `agentflow.yaml`
   (`/webhook/ingest/change-me-5f0c9e2a/`) to something of your own, then:

   ```bash
   export OPENAI_API_KEY=sk-...  TELEGRAM_BOT_TOKEN=123456:ABC...
   agentflow -config examples/telegram-rag/agentflow.yaml
   ```

3. Load the sample knowledge (any text or Markdown file works):

   ```bash
   jq -Rs '{from: "loader", text: ("/learn " + .)}' examples/telegram-rag/knowledge/agentflow.md |
     curl -s localhost:8080/webhook/ingest/change-me-5f0c9e2a/ -d @-
   ```

   The reply says how many passages were learned.
4. Ask the bot on Telegram: *"What is a sandbox job?"*

To let yourself `/learn` from Telegram, find your numeric user id (for
example from [@userinfobot](https://t.me/userinfobot)) and add
`user:telegram:<id>` to `extras.editors`.

## Make it yours

- **Better ordering** — uncomment the `rerank` model and `rerank_model`: the
  store is oversampled and a cross-encoder picks the best passages. Passages
  it scores below 0.25 are dropped.
- **Bigger corpora** — point the `knowledge` store at pgvector or Qdrant
  (`memory.backends`); the loop does not change.
- **Answer style** — [`prompts/answer.md`](prompts/answer.md).

## Test it

```bash
for t in examples/telegram-rag/tests/*.yaml; do agentflow loop test "$t"; done
```
