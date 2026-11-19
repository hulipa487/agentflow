# Single-turn chat/completions

An OpenAI-compatible `/v1/chat/completions` endpoint backed by an agent: any
client or SDK that speaks the OpenAI API works with only a base URL and a key.

| | |
| --- | --- |
| Channel | `openai`, `auth: key` |
| Loop | [`loops/single_turn.lua`](loops/single_turn.lua) |
| Needs | any model AgentFlow can call (OpenAI-compatible, Anthropic, Gemini, …) |

## How it works

Each request is one turn in a session that exists for that request only. The
loop answers the request's last user message with the agent's own
instructions and model, and returns it in the OpenAI response shape (text,
finish reason, token usage). The `model` field in the request names the
agent; `GET /v1/models` lists the exposed agents.

AgentFlow adds what a bare model endpoint lacks: per-key access, metering,
safety filters and the audit journal (`journal: metadata` by default for this
channel type, so request text is not stored).

## Run it

```bash
export OPENAI_API_KEY=sk-...  COMPLETIONS_API_KEY=$(openssl rand -hex 24)
agentflow -config examples/chat-completions/agentflow.yaml
```

```bash
curl -s localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $COMPLETIONS_API_KEY" -H 'Content-Type: application/json' \
  -d '{"model": "assistant", "messages": [{"role": "user", "content": "Say hi in French."}]}'
```

With the OpenAI SDK:

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="<COMPLETIONS_API_KEY>")
print(client.chat.completions.create(model="assistant",
      messages=[{"role": "user", "content": "Say hi in French."}]).choices[0].message.content)
```

## Make it yours

- **Pass-through** — `loop: plugin:openai_passthrough` forwards the client's
  whole request (history, tools, sampling parameters) to the agent's model:
  the endpoint then behaves exactly like a model with AgentFlow's controls.
- **Per-user keys** — `auth: user_token` with `runtime.identity.enabled`
  gives each person their own token, quota and memory scope.
- **Conversations** — a client that sends `metadata.conversation_id` (or the
  `X-AgentFlow-Conversation` header) keeps one session across requests.

## Test it

```bash
agentflow loop test examples/chat-completions/tests/single_turn.yaml
```
