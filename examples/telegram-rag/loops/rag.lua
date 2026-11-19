-- af-prelude-version: 1
-- Telegram bot that answers from a knowledge base (retrieval-augmented
-- generation).
--
-- Knowledge lives in one vector store. A question is embedded and the closest
-- passages come back through memory.recall (the profile's recall handler is
-- plugin:semantic, and an optional rerank model sharpens the order). The model
-- answers from those passages only and cites them as [1], [2], ...; with no
-- passage close enough it says so instead of guessing, without a model call.
--
-- Teaching it:
--   /learn <text>  splits the text into passages and stores them with their
--                  embeddings. Only editors may (extras.editors in the agent's
--                  config, as "user:telegram:<id>"), and so may the ingest
--                  webhook, whose secret path is the authorisation — see
--                  README.md for loading a whole document that way.

local PASSAGE_CHARS = 900   -- target passage size when splitting a document
local TOP_K = 4             -- passages given to the model
local MIN_SCORE = 0.25      -- rerank score below which a passage is ignored

local function is_editor(msg)
  if msg.channel == "ingest" then
    return true
  end
  local extras = (agent.config() or {}).extras or {}
  for _, who in ipairs(extras.editors or {}) do
    if who == msg.from then
      return true
    end
  end
  return false
end

-- split cuts a document into passages at blank lines, packing paragraphs
-- together up to PASSAGE_CHARS so each passage carries some context.
local function split(doc)
  local passages, current = {}, ""
  for para in (doc .. "\n\n"):gmatch("(.-)\n%s*\n") do
    para = para:gsub("^%s+", ""):gsub("%s+$", "")
    if para ~= "" then
      if #current > 0 and #current + #para + 2 > PASSAGE_CHARS then
        table.insert(passages, current)
        current = para
      elseif #current > 0 then
        current = current .. "\n\n" .. para
      else
        current = para
      end
    end
  end
  if #current > 0 then
    table.insert(passages, current)
  end
  return passages
end

local function learn(doc, source)
  local passages = split(doc)
  local stamp = tostring(os.time())
  for i, passage in ipairs(passages) do
    -- Explicit targets route straight to the knowledge store; memory.write
    -- embeds the text because the profile names an embed_model.
    memory.write({
      targets = {
        {
          store = "knowledge",
          key = "doc:" .. stamp .. ":" .. i,
          value = { text = passage, source = source },
        },
      },
    })
  end
  return #passages
end

local function retrieve(question)
  local hits = memory.recall({ store = "knowledge", text = question, k = TOP_K }) or {}
  local out = {}
  for _, hit in ipairs(hits) do
    local score = hit.meta and hit.meta.rerank_score
    local v = hit.value
    if type(v) == "table" and v.text and (score == nil or score >= MIN_SCORE) then
      table.insert(out, v)
    end
  end
  return out
end

local function answer(question, passages)
  local numbered = {}
  for i, p in ipairs(passages) do
    table.insert(numbered, "[" .. i .. "] (" .. tostring(p.source or "knowledge base") .. ")\n" .. p.text)
  end
  local info = agent.info()
  local reply = llm.chat({
    { role = "system", content = info.instructions or "Answer from the passages and cite them." },
    { role = "system", content = "Passages:\n\n" .. table.concat(numbered, "\n\n") },
    { role = "user", content = question },
  })
  return reply.text or ""
end

function loop()
  while true do
    local msg = session.inbox()
    local text = msg.text or ""
    local doc = text:match("^%s*/learn%s+(.+)$")
    if doc then
      if is_editor(msg) then
        local p = msg.payload or {}
        local source = p.source or p.username or msg.from or "chat"
        local n = learn(doc, tostring(source))
        session.send("Learned " .. n .. " passage" .. (n == 1 and "" or "s") .. ".")
      else
        session.send("Only editors can teach me. Ask the bot's owner to add you.")
      end
    elseif text:match("^%s*/start") then
      session.send("Ask me anything about what I've been taught. I answer from my knowledge base and cite it.")
    elseif text:match("%S") then
      local passages = retrieve(text)
      if #passages == 0 then
        session.send("I don't have anything on that in my knowledge base yet.")
      else
        session.send(answer(text, passages))
      end
    end
  end
end
