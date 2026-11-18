-- af-prelude-version: 1
-- plugin:token_budget — the default prompt.budget handler.
--
-- Loaded into every session state before the loop plugin (as the global
-- `token_budget`). Approximates tokens as ceil(chars/4) — cheap and
-- model-agnostic; a real tokenizer can shadow this builtin by name later.
--
-- Contract: token_budget(messages, budget) -> messages
--   - a history that fits comes back as it is (the same table)
--   - the system message (messages[1] when role == "system") is always kept
--   - the most recent message is always kept
--   - oldest history is dropped from the front until it fits
--   - a tool exchange is kept or dropped as one: an assistant turn that asked
--     for tools and the results that answer it never part company
--
-- The last rule is what keeps a trimmed history sendable. A provider checks
-- that every tool result answers a tool call in the turn before it, and
-- rejects the request otherwise; a cut that lands inside an exchange leaves
-- exactly that, and the loop sees a failed model call on a conversation that
-- was fine one turn earlier. It also means "the most recent message" is the
-- whole exchange when that message is a tool result, as it is between the
-- rounds of a tool loop — even when the exchange alone is over budget. An
-- over-budget request the provider can read beats a smaller one it refuses.
--
-- This function trims; it does not repair. A history that arrives with a stray
-- tool result is passed through with the stray in it.

local function est_tokens(content)
  if type(content) ~= "string" then return 0 end
  return math.ceil(#content / 4)
end

-- Characters in a value of unknown shape: tool-call arguments arrive either as
-- the model's own JSON text or as the parsed table. The table form is sized by
-- walking it rather than encoding it, so this chunk needs nothing but the
-- language. Depth is bounded; arguments are data, but they are not ours.
local function est_chars(v, depth)
  local t = type(v)
  if t == "string" then return #v end
  if t == "number" or t == "boolean" then return #tostring(v) end
  if t ~= "table" or depth > 8 then return 0 end
  local n = 2
  for k, item in pairs(v) do
    n = n + est_chars(k, depth + 1) + est_chars(item, depth + 1) + 4
  end
  return n
end

-- Real wire cost of one message. Two things ride outside content and are
-- billed on every later request all the same: thinking_blocks replayed on
-- assistant turns (chars/4 over the thinking text and over redacted data blobs
-- — the latter overestimates, which is the safe direction), and the tool calls
-- an assistant turn made, whose arguments can be far larger than its text.
local function est_msg(m)
  local n = est_tokens(m.content) + 4
  if type(m.thinking_blocks) == "table" then
    for _, b in ipairs(m.thinking_blocks) do
      if type(b) == "table" then
        n = n + est_tokens(b.thinking) + est_tokens(b.data)
      end
    end
  end
  if type(m.tool_calls) == "table" then
    for _, c in ipairs(m.tool_calls) do
      if type(c) == "table" then
        local args = c.arguments
        if args == nil then args = c.args end
        n = n + math.ceil((est_chars(c.name, 0) + est_chars(args, 0)) / 4) + 4
      end
    end
  end
  return n
end

local function total(messages)
  local n = 0
  for _, m in ipairs(messages) do n = n + est_msg(m) end
  return n
end

local function asks_for_tools(m)
  return m.role == "assistant" and type(m.tool_calls) == "table" and #m.tool_calls > 0
end

-- Split history into the units trimming works in. A unit is one message, or
-- an assistant turn that asked for tools together with the tool results that
-- follow it. Results are taken by position, not by matching ids: they are the
-- run of role="tool" messages right after the turn, which is the only place a
-- provider accepts them.
local function units_of(body)
  local units = {}
  local i = 1
  while i <= #body do
    local unit = { messages = { body[i] }, cost = est_msg(body[i]) }
    local j = i + 1
    if asks_for_tools(body[i]) then
      while body[j] and body[j].role == "tool" do
        unit.messages[#unit.messages + 1] = body[j]
        unit.cost = unit.cost + est_msg(body[j])
        j = j + 1
      end
    end
    units[#units + 1] = unit
    i = j
  end
  return units
end

function token_budget(messages, budget)
  budget = budget or 6000
  if total(messages) <= budget then return messages end

  local kept = {}          -- system message, then history oldest → newest
  local tail_start = 1
  local n = 0
  if messages[1] and messages[1].role == "system" then
    kept[1] = messages[1]
    n = est_msg(messages[1])
    tail_start = 2
  end

  local body = {}
  for i = tail_start, #messages do body[#body + 1] = messages[i] end
  local units = units_of(body)

  -- Walk back from the newest unit, which is always kept, and stop at the
  -- first one that does not fit: history stays contiguous, so the model never
  -- sees a conversation with a hole in the middle.
  local first = #units + 1
  for i = #units, 1, -1 do
    if i < #units and n + units[i].cost > budget then break end
    n = n + units[i].cost
    first = i
  end
  for i = first, #units do
    for _, m in ipairs(units[i].messages) do kept[#kept + 1] = m end
  end
  return kept
end
