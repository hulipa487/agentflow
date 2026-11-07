-- af-prelude-version: 1
Greeting = {}

function Greeting.hello(name)
  return "hello, " .. (name or "world")
end
