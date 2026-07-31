-- stream.lua — phase-1 example: consuming llm.stream.
-- Each delta is a separate yield/resume round trip across the bridge.
--
-- NOTE: call the stream function directly (`local d = next_chunk()`), never
-- in a generic-for (`for d in llm.stream(...)`) — Luau inherits the Lua 5.1
-- restriction that the for-loop protocol cannot yield.

function loop()
  while true do
    local msg = session.inbox()
    if msg.type == "user" and msg.text then
      local info = agent.info()
      local next_chunk = llm.stream({ { role = "user", content = msg.text } }, { model = info.model })
      local parts = {}
      while true do
        local delta = next_chunk()
        if delta == nil then break end
        parts[#parts + 1] = delta
      end
      session.send(table.concat(parts))
    end
  end
end
