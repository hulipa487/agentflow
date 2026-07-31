-- echo.lua — phase-0 example loop plugin.
--
-- Demonstrates the async bridge: session.inbox() and session.send() look
-- synchronous but park the coroutine; time.sleep yields without blocking
-- any thread. Edit this file while the runtime is up to see hot reload.

log.info("echo agent online")

function loop()
  while true do
    local msg = session.inbox()
    log.info("message from " .. (msg.from or "?"))
    time.sleep(0.3) -- pretend to think; proves yield/resume mid-turn
    session.send("echo: " .. (msg.text or ""))
  end
end
