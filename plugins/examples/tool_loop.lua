-- tool_loop.lua — example loop that lets the LLM call tools, then invokes them.
-- This is essentially a minimal version of builtin:react for learning.

function loop()
  while true do
    local msg = session.inbox()
    if msg.type == "user" and msg.text then
      local info = agent.info()
      local tools = tools.list()

      local chat_opts = { model = info.model }
      if #tools > 0 then
        chat_opts.tools = tools
        chat_opts.tool_choice = "auto"
      end

      local ok, reply = pcall(llm.chat,
        { { role = "user", content = msg.text } },
        chat_opts)

      if not ok then
        session.send("error: " .. tostring(reply))
      elseif reply.tool_calls and #reply.tool_calls > 0 then
        for _, tc in ipairs(reply.tool_calls) do
          local name = tc.name or (tc["function"] and tc["function"]["name"])
          local args = tc.args or (tc["function"] and tc["function"]["arguments"]) or {}
          local r = tools.run(name, args)
          log.info("tool result: " .. json.encode(r))
        end
        session.send(reply.text or "done")
      else
        session.send(reply.text or "")
      end
    end
  end
end
