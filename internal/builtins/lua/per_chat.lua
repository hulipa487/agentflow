-- builtin:per_chat — the default gateway.route handler.
--
-- Runs in the router's service state (singleton), not in a session. One
-- session per (agent, channel, chat): two users on the same channel never
-- share history, and one user's two chats don't either.
--
-- Router op contract (service-state ops, distinct from session ops):
--   inbox   -> { message = Message, agent = string }   (next inbound event)
--   deliver { agent, key, message }                    (resolve/forward)
--
-- Routing key: channel-qualified chat id; falls back to the sender when a
-- channel has no chat concept (e.g. webhook).

local function deliver(item, key)
  af.op({
    type = "deliver",
    agent = item.agent,
    key = key,
    message = item.message,
  })
end

function loop()
  while true do
    local item = session.inbox()
    local msg = item.message
    local chat
    if msg.payload then chat = msg.payload.chat_id end
    local key = (msg.channel or "?") .. ":" .. tostring(chat or msg.from or "?")
    deliver(item, key)
  end
end
