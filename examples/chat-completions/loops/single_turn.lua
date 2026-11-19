-- af-prelude-version: 1
-- Single-turn chat/completions.
--
-- The OpenAI channel turns every POST /v1/chat/completions into one turn: the
-- request's last user message is msg.text, and the session exists for this
-- request only (unless the client sends a conversation id). This loop answers
-- that one message with the agent's own instructions and model, and nothing
-- else from the client's request — a small, predictable endpoint for scripts,
-- form fills and classification calls.
--
-- To forward the client's whole request (its messages, tools and sampling
-- parameters) instead, use loop: plugin:openai_passthrough.

function loop()
  while true do
    local msg = session.inbox()
    local info = agent.info()
    local reply = llm.chat({
      { role = "system", content = info.instructions or "You are a helpful assistant." },
      { role = "user", content = msg.text or "" },
    })
    session.reply({
      text = reply.text or "",
      finish_reason = reply.finish_reason,
      usage = reply.usage,
    })
  end
end
