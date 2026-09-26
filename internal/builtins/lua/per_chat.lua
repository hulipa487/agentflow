-- af-prelude-version: 1
-- plugin:per_chat — the default gateway.route handler.
--
-- Runs in the router's service state (singleton), not in a session. One
-- session per (agent, channel, chat, identity): two users on the same channel
-- never share history, one user's two chats don't either, and a chat acting
-- in a group context is a different session from the same chat acting
-- personally — the identity is part of the key, because sharing one session
-- between the two would leak private history into the group (§5.7).
--
-- Router op contract (service-state ops, distinct from session ops):
--   inbox   -> { message = Message, agent = string }   (next inbound event)
--   deliver { agent, key, message, identity_group? }   (resolve/forward;
--             identity_group is a PROPOSAL — the engine validates the
--             membership and stamps the acting identity, or refuses)
--   router.state.get/set/delete/list                   (durable, fleet-wide;
--     a handler's Lua state is per process, so anything it must remember goes
--     there instead — see router.state in the docs)
--
-- Routing key: channel-qualified chat id, plus the acting group when the chat
-- is in group mode. The mode is durable per chat (B10), stored in
-- router.state under "mode:<channel>:<chat>", so two instances routing the
-- same message reach the same session key.
--
-- The reference toggle is a chat command ("/group <uuid>" / "/personal");
-- which commands exist, and who may use them, is policy — replace this
-- handler wholesale if the product wants something else. The engine-side
-- validation this handler's proposal flows into is not optional.

local function chat_of(msg)
  if msg.payload and msg.payload.chat_id then
    return tostring(msg.payload.chat_id)
  end
  return tostring(msg.from or "?")
end

local function mode_key(msg)
  return "mode:" .. (msg.channel or "?") .. ":" .. chat_of(msg)
end

local function deliver(item, key, group)
  local op = {
    type = "deliver",
    agent = item.agent,
    key = key,
    message = item.message,
  }
  if group then op.identity_group = group end
  af.op(op)
end

function loop()
  while true do
    local item = session.inbox()
    local msg = item.message
    local channel = msg.channel or "?"
    local base = channel .. ":" .. chat_of(msg)
    local mk = mode_key(msg)
    local group

    -- Toggle commands, resolved before routing so the message itself lands
    -- in the context it selects.
    if msg.text then
      local picked = msg.text:match("^/group%s+([%w%-]+)$")
      if picked then
        af.op({ type = "router.state.set", key = mk, value = picked })
        group = picked
      elseif msg.text == "/personal" then
        af.op({ type = "router.state.delete", key = mk })
      end
    end

    -- Durable mode: a chat in group mode routes to the group's session and
    -- proposes the group; the engine validates the sender's membership.
    -- state.set stored a Lua string, which arrives back as a JSON string.
    if group == nil then
      local mode = json.decode(af.op({ type = "router.state.get", key = mk }))
      if mode ~= nil and mode ~= "" then
        group = tostring(mode)
      end
    end

    if group then
      deliver(item, base .. ":" .. group, group)
    else
      deliver(item, base)
    end
  end
end
