-- af-prelude-version: 2
-- af-import: greeting

function loop()
  while true do
    local msg = session.inbox()
    session.send(Greeting.hello(msg.text))
  end
end
