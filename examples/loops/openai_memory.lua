-- af-prelude-version: 1
-- openai_memory — pass-through with memory, the "exceptional" side of the
-- OpenAI channel.
--
-- The default loop forwards the client's request untouched, because a client
-- that is itself an agent should get exactly a model. This loop is the other
-- choice: it remembers things about the person and prepends what it recalled
-- as a system message, while still forwarding the client's own conversation,
-- tools and sampling parameters.
--
-- The facts store is user-scoped by the engine (the turn's identity, from the
-- bearer token), so one person's memories never reach another's request. With
-- auth: key or none there is no tenant, and the recall is empty — the loop
-- still works, it just has nothing to remember.

local function facts_system_message(msg)
  local hits = memory.recall({
    store = "facts",
    kind = "text",
    text = msg.text or "",
    k = 5,
  })
  if hits == nil or #hits == 0 then
    return nil
  end
  local lines = {}
  for _, hit in ipairs(hits) do
    local v = hit.value
    local text = nil
    if type(v) == "table" then
      text = v.text or v.fact or v.value
    elseif type(v) == "string" then
      text = v
    end
    if type(text) == "string" and text ~= "" then
      table.insert(lines, "- " .. text)
    end
  end
  if #lines == 0 then
    return nil
  end
  return {
    role = "system",
    content = "Known facts about the person you are talking to:\n" .. table.concat(lines, "\n"),
  }
end

function loop()
  while true do
    local msg = session.inbox()
    local facts = facts_system_message(msg)
    local opts = nil
    if facts then
      opts = { prepend = { facts } }
    end
    local reply = llm.forward(opts)
    session.reply(reply)
  end
end
