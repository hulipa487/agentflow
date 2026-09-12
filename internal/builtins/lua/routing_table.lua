-- builtin:routing_table — default memory.write filter chain.
--
-- The chain receives the record written by the agent and returns a list of
-- targets. Each target is { store = <logical>, key = ..., value = ..., ttl = ... }.
--
-- Default: route "turn" records to the "dialogue" store and "fact" records
-- to the "facts" store. The agent info exposes the declared stores so a
-- custom chain can inspect them.

function memory_routing_table(record)
  if not record then return {} end

  local info = agent.info()
  local stores = (info.memory and info.memory.stores) or {}

  local targets = {}

  if record.type == "turn" then
    if stores.dialogue then
      local key = record.id or record.ts
      targets[#targets + 1] = {
        store = "dialogue",
        key = "turn:" .. tostring(key or "unknown"),
        value = record,
        ttl = record.ttl,
      }
    end

  elseif record.type == "fact" then
    if stores.facts then
      targets[#targets + 1] = {
        store = "facts",
        key = tostring(record.key or record.id or "unknown"),
        value = record,
        ttl = record.ttl,
      }
    end

  elseif record.targets then
    -- Explicit routing provided by the caller.
    for _, t in ipairs(record.targets) do
      targets[#targets + 1] = t
    end
  end

  return targets
end
