-- af-prelude-version: 1
-- Parent agent delegating to an ephemeral child.
--
-- Needs: agent.spawn, agent.request, and a spawn template named here under
-- profiles.agent (the "researcher" profile is an example — add one, or change
-- the name). The child's capabilities are the intersection of this agent's
-- and the template's; nothing can be widened by spawning.
--
-- The worker side (the template's loop) answers a request by replying with
-- the request id from the core-stamped provenance:
--
--   if msg.type == "agent" then
--     local rid = msg.provenance and msg.provenance.request_id
--     if rid then agent.reply(rid, { answer = "..." }) end
--   end
--
-- Do not build the id from msg.payload: a payload is loop-controlled, the
-- provenance is engine-stamped.

local function handle(msg)
  local ok, child = pcall(agent.spawn, "researcher")
  if not ok then
    session.send("could not start a researcher: " .. tostring(child))
    return
  end

  -- child.address is a session:<id> address and is the one to use for a
  -- spawned child; agent:<name> addresses a configured static agent.
  local ok2, answer = pcall(agent.request, child.address, { question = msg.text }, 120)
  if not ok2 then
    session.send("the researcher did not answer: " .. tostring(answer))
    return
  end

  local payload = answer.payload or {}
  session.send(payload.answer or json.encode(payload))
end

function loop()
  while true do
    local msg = session.inbox()
    if msg.type == "user" and msg.text then
      handle(msg)
    end
  end
end
