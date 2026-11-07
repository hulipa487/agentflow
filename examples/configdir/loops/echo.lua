-- af-prelude-version: 2
function loop()
  while true do
    local msg = session.inbox()
    session.send("echo: " .. (msg.text or ""))
  end
end
