-- af-prelude-version: 1
-- The smallest runnable loop: no model, no tools. It replies with the text it
-- received, which makes it the first thing to test after a boot.

function loop()
  while true do
    local msg = session.inbox()
    session.send("echo: " .. (msg.text or ""))
  end
end
