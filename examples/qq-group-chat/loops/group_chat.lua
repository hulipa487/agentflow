-- af-prelude-version: 1
-- QQ group chatbot with memory and context.
--
-- A QQ group hears the bot only when someone @-mentions it, and the router
-- keeps one session per group (payload.chat_id = "group:<group_openid>"). For
-- every message the loop builds the prompt from three layers:
--
--   context  the group's last exchanges, word for word. They live in
--            session.state, so they survive the session being unloaded when
--            the group goes quiet, and a restart.
--   memory   older exchanges from this group that match the new message,
--            found by full-text recall over the dialogue store.
--   facts    what members asked the bot to keep ("/remember ..."), recalled
--            the same way.
--
-- Every speaker is named in the prompt, so the model can tell members apart
-- and answer the right person.
--
-- Commands: /remember <fact>, /forget (clears the recent context).

local CONTEXT_TURNS = 10   -- exchanges kept word for word
local RECALL_K = 4         -- older exchanges or facts recalled per message
local MAX_LINE = 800       -- characters kept from one message in the context

local function chat_of(msg)
  local p = msg.payload or {}
  return tostring(p.chat_id or msg.reply_to or "local")
end

local function speaker_of(msg)
  local p = msg.payload or {}
  return tostring(p.name or p.username or p.member_openid or msg.from or "member")
end

local function clip(s)
  s = tostring(s or "")
  if #s > MAX_LINE then
    return s:sub(1, MAX_LINE) .. "…"
  end
  return s
end

local function load_context()
  local ctx = session.state.get("context")
  if type(ctx) ~= "table" then
    return {}
  end
  return ctx
end

local function save_exchange(ctx, who, text, reply)
  table.insert(ctx, { who = who, text = clip(text), reply = clip(reply) })
  while #ctx > CONTEXT_TURNS do
    table.remove(ctx, 1)
  end
  session.state.set("context", ctx)
end

-- recall returns this group's records from a store that match text. The
-- store is shared by every group the bot is in, so other groups' rows are
-- filtered out here.
local function recall(store, chat, text, skip)
  local hits = memory.recall({ store = store, kind = "text", text = text, k = RECALL_K * 3 }) or {}
  local out = {}
  for _, hit in ipairs(hits) do
    local v = hit.value
    if type(v) == "table" and v.chat == chat and not (skip and skip[v.text or ""]) then
      table.insert(out, v)
      if #out >= RECALL_K then
        break
      end
    end
  end
  return out
end

local function build_prompt(info, chat, who, text, ctx)
  local messages = {
    { role = "system", content = info.instructions or "You are a friendly member of this QQ group." },
  }

  local facts = recall("facts", chat, text)
  if #facts > 0 then
    local lines = {}
    for _, f in ipairs(facts) do
      table.insert(lines, "- " .. f.text .. " (from " .. tostring(f.by or "a member") .. ")")
    end
    table.insert(messages, { role = "system", content = "Facts this group asked you to remember:\n" .. table.concat(lines, "\n") })
  end

  -- Older exchanges, minus the ones the context already holds word for word.
  local recent = {}
  for _, e in ipairs(ctx) do
    recent[e.text] = true
  end
  local older = recall("dialogue", chat, text, recent)
  if #older > 0 then
    local lines = {}
    for _, e in ipairs(older) do
      table.insert(lines, "- " .. tostring(e.who) .. ": " .. clip(e.text) .. "\n  you: " .. clip(e.reply))
    end
    table.insert(messages, { role = "system", content = "Earlier in this group (may be relevant):\n" .. table.concat(lines, "\n") })
  end

  for _, e in ipairs(ctx) do
    table.insert(messages, { role = "user", content = e.who .. ": " .. e.text })
    table.insert(messages, { role = "assistant", content = e.reply })
  end
  table.insert(messages, { role = "user", content = who .. ": " .. text })

  -- token_budget is a support chunk: it trims the oldest turns first and
  -- keeps the system messages and the newest turn whole.
  return token_budget(messages, info.history_budget or 6000)
end

function loop()
  while true do
    local msg = session.inbox()
    local text = msg.text or ""
    local chat = chat_of(msg)
    local who = speaker_of(msg)

    local fact = text:match("^%s*/remember%s+(.+)$")
    if fact then
      memory.write({
        type = "fact",
        key = chat .. ":" .. tostring(os.time()) .. ":" .. tostring(#fact),
        chat = chat,
        text = fact,
        by = who,
      })
      session.send("Noted, I'll remember: " .. fact)
    elseif text:match("^%s*/forget%s*$") then
      session.state.delete("context")
      session.send("Cleared what I remember of the recent conversation here.")
    elseif text:match("%S") then
      local ctx = load_context()
      local reply = llm.chat(build_prompt(agent.info(), chat, who, text, ctx))
      local answer = reply.text or ""
      session.send(answer)
      memory.write({ type = "turn", id = msg.id, chat = chat, who = who, text = text, reply = answer })
      save_exchange(ctx, who, text, answer)
    end
  end
end
