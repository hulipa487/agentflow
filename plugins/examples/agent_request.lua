-- agent_request.lua — example loop that asks another agent a question and
-- replies with its answer.
-- Requires the agent capabilities include agent.request and agent.send.

function loop()
  while true do
    local msg = session.inbox()
    if msg.type == "user" and msg.text then
      local ok, answer = pcall(agent.request, "agent:expert", {
        question = msg.text,
      }, 30)
      if ok then
        session.send("expert says: " .. (answer.payload and answer.payload.answer or "?"))
      else
        session.send("expert unavailable: " .. tostring(answer))
      end
    end
  end
end
