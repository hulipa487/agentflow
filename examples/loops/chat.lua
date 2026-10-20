-- af-prelude-version: 1
-- A conversational loop with memory:
--   1. recall earlier turns from the dialogue store (full-text query),
--   2. build the prompt (system instructions + recalled turns + this turn),
--   3. trim it to the agent's history budget,
--   4. ask the model, reply, and record the turn.
--
-- The memory block is skipped when the agent has no memory profile, so the
-- same loop also works as a plain chat agent.

function loop()
  while true do
    local msg = session.inbox()
    local info = agent.info()
    local text = msg.text or ""

    local messages = {
      { role = "system", content = info.instructions or "You are a helpful assistant." },
    }

    local mem = info.memory or {}
    local stores = mem.stores or {}
    local has_dialogue = stores.dialogue ~= nil

    if has_dialogue then
      local hits = memory.recall({
        store = "dialogue",
        kind = "text",
        text = text,
        k = 5,
      })
      for _, hit in ipairs(hits or {}) do
        local v = hit.value
        if type(v) == "table" then
          if v.text and v.text ~= "" then
            table.insert(messages, { role = "user", content = v.text })
          end
          if v.reply and v.reply ~= "" then
            table.insert(messages, { role = "assistant", content = v.reply })
          end
        end
      end
    end

    table.insert(messages, { role = "user", content = text })
    -- token_budget is a support chunk loaded into every session.
    messages = token_budget(messages, info.history_budget or 6000)

    local reply = llm.chat(messages)
    session.send(reply.text or "")

    if has_dialogue then
      memory.write({
        type = "turn",
        id = msg.id,
        text = text,
        reply = reply.text or "",
        from = msg.from,
      })
    end
  end
end
