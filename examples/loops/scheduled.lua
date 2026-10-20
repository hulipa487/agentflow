-- af-prelude-version: 1
-- Session timers.
--
-- Timers are ordinary mailbox messages (type="timer", payload.timer_id), so
-- the loop stays single-threaded: there is no callback running beside your
-- code. They are session-scoped and in memory, which has two consequences:
--
--   * a process restart forgets them; the loop re-registers on its next start
--   * a hot reload restarts the coroutine but not the session, so timers
--     registered by the previous run are still live. Re-registering blindly
--     would duplicate them.
--
-- ensure_timer below cancels any remembered id first, so it is idempotent
-- across reloads and harmless after a restart (an unknown id cancels with an
-- error that pcall swallows). It needs the session.state capability.

local function ensure_timer(key, seconds)
  local old = session.state.get(key)
  if old then pcall(scheduler.cancel, old) end
  local t = scheduler.every(seconds)
  session.state.set(key, t.id)
  return t.id
end

function loop()
  local tick = ensure_timer("timer:tick", 60)
  local sweep = ensure_timer("timer:sweep", 3600)
  log.info("timers registered: " .. tick .. ", " .. sweep)

  while true do
    local msg = session.inbox()

    if msg.type == "timer" then
      local id = msg.payload and msg.payload.timer_id
      if id == tick then
        local n = (session.state.get("ticks") or 0) + 1
        session.state.set("ticks", n)
        log.debug("tick " .. n .. " at " .. os.date("%H:%M:%S"))
      elseif id == sweep then
        log.info("hourly sweep at " .. os.date("%Y-%m-%d %H:%M:%S"))
      end
      -- A timer turn has no channel: session.send would fail. Push to a
      -- known recipient (needs channel.push + its channel/reply_to), or
      -- record state for the next user turn to report.
    elseif msg.type == "user" then
      session.send("ticks so far: " .. tostring(session.state.get("ticks") or 0))
    end
  end
end
