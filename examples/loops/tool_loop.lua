-- af-prelude-version: 1
-- A multi-round tool loop.
--
-- The engine executes one tool when asked; driving the conversation is the
-- loop's job. That includes replaying the assistant turn that requested the
-- tools, appending each result as a role="tool" message, and asking again —
-- a model that requests a second tool will not get it otherwise.
--
-- Needs: llm.chat and tools. The agent must be able to see the tools it uses
-- (skills, or tools.policy.default).

local MAX_ROUNDS = 4

local function handle(msg)
  local info = agent.info()
  local messages = {
    { role = "system", content = info.instructions or "Use the available tools when they help." },
    { role = "user", content = msg.text or "" },
  }
  local available = tools.list()

  for _ = 1, MAX_ROUNDS do
    local opts = { model = info.model }
    if #available > 0 then
      opts.tools = available
      opts.tool_choice = "auto"
    end

    local reply = llm.chat(messages, opts)
    local calls = reply.tool_calls or {}
    if #calls == 0 then
      session.send(reply.text or "")
      return
    end

    -- Replay the assistant turn verbatim. thinking_blocks matters on
    -- Anthropic when thinking is enabled; other providers ignore it.
    table.insert(messages, {
      role = "assistant",
      content = reply.text or "",
      tool_calls = calls,
      thinking_blocks = reply.thinking_blocks,
    })

    for _, call in ipairs(calls) do
      local ok, result = pcall(tools.run, call.name, call.args or {})
      local body
      if ok then
        body = json.encode(result)
      else
        body = json.encode({ ok = false, error = tostring(result) })
      end
      table.insert(messages, {
        role = "tool",
        tool_call_id = call.id,
        content = body,
      })
    end
  end

  session.send("I stopped after " .. MAX_ROUNDS .. " tool rounds without a final answer.")
end

function loop()
  while true do
    local msg = session.inbox()
    if msg.text and msg.text ~= "" then
      local ok, err = pcall(handle, msg)
      if not ok then
        log.error("tool turn failed: " .. tostring(err))
        session.send("Something went wrong while handling that.")
      end
    end
  end
end
