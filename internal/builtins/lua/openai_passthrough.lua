-- af-prelude-version: 1
-- plugin:openai_passthrough — make an agent look exactly like a model.
--
-- This is the default loop for the OpenAI-compatible channel. It forwards the
-- client's own request (messages, tools, tool_choice, sampling parameters) to
-- the agent's model and returns the model's text and tool calls unchanged. The
-- client is the agent here: its system prompt, its tools, its conversation.
-- AgentFlow contributes identity, metering, safety and routing — not meaning.
--
-- Tool calls are never executed by the engine. A client that wants its tools
-- run must come back with a tool result, which the next request carries and
-- this loop forwards like any other turn.
--
-- Use plugin:openai_agent instead when the client should talk to an assistant
-- (the agent's own instructions and server-side tools) rather than to a model.

function loop()
  while true do
    local msg = session.inbox()
    local reply = llm.forward()
    session.reply(reply)
  end
end
