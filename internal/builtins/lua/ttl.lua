-- builtin:ttl — shell.teardown handler.
--
-- Tracks shell handle creation times and reports whether a handle's TTL has
-- elapsed. The Go side can call shell_ttl.check(handle_id) before each exec
-- and destroy on expiry.
--
-- time.now_s() is provided by the Go runtime via agent.info().ts or via a
-- dedicated store op. For Phase 3a, use a simple tick counter.

shell_ttl = shell_ttl or {}
shell_ttl.max_age_seconds = 1800  -- 30 minutes
shell_ttl.created = {}
shell_ttl.tick = 0

-- Called by the Go side or Lua loop periodically.
function shell_ttl.update_tick()
  shell_ttl.tick = shell_ttl.tick + 1
end

function shell_ttl.register(handle_id, max_age)
  shell_ttl.created[handle_id] = {
    tick = shell_ttl.tick,
    max_age = max_age or shell_ttl.max_age_seconds,
  }
end

function shell_ttl.check(handle_id)
  local entry = shell_ttl.created[handle_id]
  if not entry then return false end
  return (shell_ttl.tick - entry.tick) > entry.max_age
end

function shell_ttl.expired()
  local expired_ids = {}
  for id, entry in pairs(shell_ttl.created) do
    if (shell_ttl.tick - entry.tick) > entry.max_age then
      expired_ids[#expired_ids + 1] = id
    end
  end
  return expired_ids
end
