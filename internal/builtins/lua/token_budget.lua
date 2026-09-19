-- plugin:token_budget — the default prompt.budget handler.
--
-- Loaded into every session state before the loop plugin (as the global
-- `token_budget`). Approximates tokens as ceil(chars/4) — cheap and
-- model-agnostic; a real tokenizer can shadow this builtin by name later.
--
-- Contract: token_budget(messages, budget) -> messages
--   - the system message (messages[1] when role == "system") is always kept
--   - the most recent message is always kept
--   - oldest history messages are dropped from the front until it fits

local function est_tokens(content)
  if type(content) ~= "string" then return 0 end
  return math.ceil(#content / 4)
end

-- Real wire cost of one message. thinking_blocks replayed on assistant turns
-- ride outside content but are billed by the provider on every continuation
-- request (chars/4 over the thinking text and over redacted data blobs — the
-- latter overestimates, which is the safe direction).
local function est_msg(m)
  local n = est_tokens(m.content) + 4
  if type(m.thinking_blocks) == "table" then
    for _, b in ipairs(m.thinking_blocks) do
      if type(b) == "table" then
        n = n + est_tokens(b.thinking) + est_tokens(b.data)
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

function token_budget(messages, budget)
  budget = budget or 6000
  if total(messages) <= budget then return messages end

  local head = {}          -- system message(s)
  local tail_start = 1
  if messages[1] and messages[1].role == "system" then
    head[1] = messages[1]
    tail_start = 2
  end

  local body = {}
  for i = tail_start, #messages do body[#body + 1] = messages[i] end

  -- drop oldest history until the remainder fits
  local kept = {}
  for _, m in ipairs(head) do kept[#kept + 1] = m end
  local n = total(kept)
  local last = math.max(1, #body) -- always keep the newest message
  for i = #body, 1, -1 do
    local cost = est_msg(body[i])
    if i < last and n + cost > budget then break end
    table.insert(kept, #head + 1, body[i])
    n = n + cost
  end
  return kept
end
