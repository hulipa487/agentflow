-- builtin:react — the default session loop: message in → recall → tools → LLM → reply.
--
-- Phase 2: history is fetched from memory.recall and written back with
-- memory.write after the turn. Tool definitions from tools.list are passed to
-- the provider natively via llm.chat's tools opt; if the LLM returns
-- tool_calls, they are invoked via tools.run and the results are echoed back
-- as "tool" role turns for a final answer.

local function trim_messages(messages, budget)
  if token_budget then
    return token_budget(messages, budget)
  end
  return messages
end

local function fetch_history()
  local ok, records = pcall(memory.recall, { kind = "all", store = "dialogue" })
  if not ok then
    log.warn("memory.recall failed: " .. tostring(records))
    return {}
  end
  if not records then
    log.warn("memory.recall returned nil")
    return {}
  end
  log.debug("memory.recall returned " .. tostring(#records) .. " records")
  local messages = {}
  -- records are newest-first; reverse for prompt order.
  for i = #records, 1, -1 do
    local rec = records[i]
    if rec.value and rec.value.type == "turn" then
      if rec.value.user then
        messages[#messages + 1] = { role = "user", content = rec.value.user }
      end
      if rec.value.assistant then
        messages[#messages + 1] = { role = "assistant", content = rec.value.assistant }
      end
    end
  end
  return messages
end

local function fetch_tools()
  local ok, list = pcall(tools.list)
  if not ok or not list then return {} end
  return list
end

-- to_tool_defs flattens tools.list entries (OpenAI-shaped
-- {type="function", function={name,description,parameters}}) into the
-- provider-agnostic {name, description, parameters} the llm bridge expects.
local function to_tool_defs(tool_list)
  local defs = {}
  for _, t in ipairs(tool_list) do
    local fn = t["function"] or t
    defs[#defs + 1] = {
      name = fn.name,
      description = fn.description,
      parameters = fn.parameters or { type = "object", properties = {} },
    }
  end
  return defs
end

-- execute_tool_calls runs each native tool call and returns the "tool" role
-- messages to echo back, carrying tool_call_id + result for the provider.
local function execute_tool_calls(tool_calls)
  local turns = {}
  for _, tc in ipairs(tool_calls) do
    local tool_name = tc.name or (tc["function"] and tc["function"]["name"])
    local ok, res = pcall(tools.run, tool_name, tc.args or tc.arguments or {})
    if not ok then
      res = { ok = false, error = tostring(res) }
    end
    turns[#turns + 1] = {
      role = "tool",
      tool_call_id = tc.id or "",
      name = tool_name,
      tool_result = res,
    }
  end
  return turns
end

function loop()
  while true do
    local msg = session.inbox()

    if msg.type == "user" and msg.text then
      local info = agent.info()
      local history = fetch_history()
      local tool_list = fetch_tools()
      local tool_defs = to_tool_defs(tool_list)

      local messages = {}
      if info.instructions and info.instructions ~= "" then
        messages[#messages + 1] = { role = "system", content = info.instructions }
      end
      for _, h in ipairs(history) do messages[#messages + 1] = h end
      messages[#messages + 1] = { role = "user", content = msg.text }
      messages = trim_messages(messages, info.history_budget)

      local chat_opts = { model = info.model }
      if #tool_defs > 0 then
        chat_opts.tools = tool_defs
        chat_opts.tool_choice = "auto"
      end

      local ok, reply = pcall(llm.chat, messages, chat_opts)
      local text
      if ok then
        -- Native tool_calls from the provider.
        if type(reply) == "table" and reply.tool_calls and #reply.tool_calls > 0 then
          -- Echo the assistant tool-call turn + each tool result, then re-ask.
          messages[#messages + 1] = {
            role = "assistant",
            content = reply.text or "",
            tool_calls = reply.tool_calls,
          }
          local tool_turns = execute_tool_calls(reply.tool_calls)
          for _, tt in ipairs(tool_turns) do messages[#messages + 1] = tt end
          messages = trim_messages(messages, info.history_budget)
          local ok2, reply2 = pcall(llm.chat, messages, chat_opts)
          if ok2 then
            text = reply2.text or tostring(reply2)
          else
            text = tostring(reply2)
          end
        else
          text = reply.text or tostring(reply)
        end
      else
        log.warn("llm.chat failed: " .. tostring(reply))
        text = "⚠️ I couldn't reach my model right now. (" .. tostring(reply) .. ")"
      end

      session.send(text)

      -- Persist the turn if memory is configured.
      local mem_ok, mem_err = pcall(memory.write, {
        type = "turn",
        id = msg.id,
        ts = msg.ts,
        user = msg.text,
        assistant = text,
      })
      if not mem_ok then
        log.warn("memory.write failed: " .. tostring(mem_err))
      end

      -- after_turn taps
      if fact_extractor_turn then
        local tap_ok, facts = pcall(fact_extractor_turn, { id = msg.id, ts = msg.ts, user = msg.text, assistant = text })
        if tap_ok and facts then
          for _, fact in ipairs(facts) do
            pcall(memory.write, fact)
          end
        end
      end

    elseif msg.type == "system" then
      log.info("system message: " .. (msg.text or ""))
    else
      log.debug("ignoring " .. msg.type .. " message")
    end
  end
end
