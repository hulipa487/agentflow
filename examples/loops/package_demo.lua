-- af-prelude-version: 1
-- af-import: greeting

function loop()
  while true do
    local msg = session.inbox()
    session.send(Greeting.hello(msg.text))
  end
end
