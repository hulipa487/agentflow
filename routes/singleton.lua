-- routes/singleton.lua
-- A constant-key route that collapses all inbound traffic into one session:
-- agent|singleton. Per-user isolation is handled by memory key partitioning
-- in the agent's loop (see plugins/orchestrator/main/10_partition.lua).

local function deliver(item)
  af.op({
    type = "deliver",
    agent = item.agent,
    key = "singleton",
    message = item.message,
  })
end

function loop()
  while true do
    local item = session.inbox()
    deliver(item)
  end
end
