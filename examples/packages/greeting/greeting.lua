-- af-prelude-version: 2
Greeting = {}

function Greeting.hello(name)
  return "hello, " .. (name or "world")
end
