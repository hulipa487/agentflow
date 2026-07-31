-- builtin:react — the default session loop: message in → recall → tools → LLM → reply.
--
-- Phase 2: history is fetched from memory.recall and written back with
-- memory.write after the turn. Tool definitions from tools.list are included
-- in the system prompt; if the LLM returns tool_calls, they are invoked via
-- tools.run and the results are sent back for a final answer.

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

local function build_tool_prompt(tool_list)
  if #tool_list == 0 then return "" end
  local lines = { "\nYou have access to the following tools:" }
  for _, t in ipairs(tool_list) do
    local fn = t["function"] or t
    lines[#lines + 1] = "- " .. tostring(fn.name) .. ": " .. tostring(fn.description)
  end
  lines[#lines + 1] = "\nIf you want to use a tool, reply with JSON: {\"tool_calls\":[{\"name\":\"...\",\"arguments\":{}}]} and nothing else."
  return table.concat(lines, "\n")
end

local function execute_tool_calls(tool_calls)
  local results = {}
  for _, tc in ipairs(tool_calls) do
    local tool_name = tc.name or (tc["function"] and tc["function"]["name"])
    local ok, res = pcall(tools.run, tool_name, tc.arguments or tc.args or {})
    if not ok then
      res = { ok = false, error = tostring(res) }
    end
    results[#results + 1] = { name = tool_name, result = res }
  end
  return results
end

function loop()
  while true do
    local msg = session.inbox()

    if msg.type == "user" and msg.text then
      local info = agent.info()
      local history = fetch_history()
      local tool_list = fetch_tools()

      local tool_prompt = build_tool_prompt(tool_list)
      local messages = {}
      if info.instructions and info.instructions ~= "" then
        messages[#messages + 1] = { role = "system", content = info.instructions .. tool_prompt }
      elseif tool_prompt ~= "" then
        messages[#messages + 1] = { role = "system", content = tool_prompt }
      end
      for _, h in ipairs(history) do messages[#messages + 1] = h end
      messages[#messages + 1] = { role = "user", content = msg.text }
      messages = trim_messages(messages, info.history_budget)

      local ok, reply = pcall(llm.chat, messages, { model = info.model })
      local text
      if ok then
        -- Support structured tool_calls responses from the LLM.
        if type(reply) == "table" and reply.tool_calls and #reply.tool_calls > 0 then
          local results = execute_tool_calls(reply.tool_calls)
          local result_text = ""
          for _, r in ipairs(results) do
            local txt = r.result.text or r.result.error or tostring(r.result)
            result_text = result_text .. "[" .. r.name .. "] " .. txt .. "\n"
          end
          messages[#messages + 1] = { role = "assistant", content = reply.text or "" }
          messages[#messages + 1] = { role = "user", content = "Tool results:\n" .. result_text }
          messages = trim_messages(messages, info.history_budget)
          local ok2, reply2 = pcall(llm.chat, messages, { model = info.model })
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
