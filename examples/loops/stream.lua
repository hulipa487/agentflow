-- af-prelude-version: 1
-- Streaming text from the model.
--
-- Call next_delta directly. A generic `for d in llm.stream(...)` is refused:
-- the iterator yields across the native bridge, and Luau does not allow that
-- from the generic-for C boundary ("attempt to yield across metamethod/C-call
-- boundary"). The iterator closes itself at done; call close() if you exit
-- early, or the stream keeps its goroutine and provider connection open.
--
-- The iterator carries text deltas only. Use llm.chat when you also need
-- usage, tool_calls or a buffered reply.

function loop()
  while true do
    local msg = session.inbox()
    local info = agent.info()

    local next_delta, close = llm.stream({
      { role = "system", content = info.instructions or "Be concise." },
      { role = "user", content = msg.text or "" },
    }, { model = info.model })

    local parts = {}
    while true do
      local delta = next_delta()
      if delta == nil then break end
      parts[#parts + 1] = delta
    end
    close() -- no-op after done; required after an early break

    session.send(table.concat(parts))
  end
end
