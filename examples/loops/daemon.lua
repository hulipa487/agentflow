-- af-prelude-version: 1
-- A daemon (agents.<name>.persistent: true).
--
-- The supervisor delivers a synthetic boot message at startup:
--   { type = "boot", from = "system:supervisor", text = "", channel = "" }
-- so the loop can do setup work before any traffic. A daemon also receives
-- ordinary user messages and its own timer messages. There is no channel on a
-- boot or timer turn, so session.send is not available there: keep state and
-- report it on the next user turn, or use session.push / session.push_user
-- (cap: channel.push).
--
-- Timers are re-registered idempotently for the same reason as scheduled.lua:
-- a hot reload restarts the coroutine but the session (and its timers) lives on.

local function ensure_timer(key, seconds)
  local old = session.state.get(key)
  if old then pcall(scheduler.cancel, old) end
  local t = scheduler.every(seconds)
  session.state.set(key, t.id)
  return t.id
end

function loop()
  local tick = ensure_timer("timer:daemon", 300)

  while true do
    local msg = session.inbox()

    if msg.type == "boot" then
      session.state.set("booted_at", os.time())
      log.info("daemon booted at " .. os.date("%Y-%m-%d %H:%M:%S"))
    elseif msg.type == "timer" then
      if msg.payload and msg.payload.timer_id == tick then
        local last = session.state.get("last_tick") or 0
        session.state.set("last_tick", os.time())
        log.debug("daemon tick; previous " .. tostring(last))
      end
    elseif msg.type == "user" then
      session.send("daemon up since " .. os.date("%Y-%m-%d %H:%M:%S", session.state.get("booted_at") or os.time()))
    end
  end
end
