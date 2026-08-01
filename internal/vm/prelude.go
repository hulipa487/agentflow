package vm

// jsonLib is a compact pure-Lua JSON implementation, preloaded into every
// state as the `json` global (values cross the Go boundary as JSON).
const jsonLib = `
json = {}

-- encode --------------------------------------------------------------------

local escapes = {
  ['"'] = '\\"', ['\\'] = '\\\\', ['\b'] = '\\b', ['\f'] = '\\f',
  ['\n'] = '\\n', ['\r'] = '\\r', ['\t'] = '\\t',
}

local function is_array(t)
  local n = #t
  -- Empty tables encode as objects ({}), not arrays. Every op payload table
  -- in this runtime is object-shaped (map[string]any on the Go side); an
  -- empty {} must not cross the bridge as [] or cgo unmarshal fails.
  if n == 0 then return false end
  local i = 0
  for k in pairs(t) do
    if type(k) ~= "number" then return false end
    i = i + 1
  end
  return i == n
end

local encode_value
local function encode_string(s)
  return '"' .. s:gsub('[%c"\\]', escapes) .. '"'
end

local function encode_table(t)
  if is_array(t) then
    local parts = {}
    for i = 1, #t do parts[i] = encode_value(t[i]) end
    return "[" .. table.concat(parts, ",") .. "]"
  end
  local parts = {}
  for k, v in pairs(t) do
    parts[#parts + 1] = encode_string(tostring(k)) .. ":" .. encode_value(v)
  end
  return "{" .. table.concat(parts, ",") .. "}"
end

encode_value = function(v)
  local ty = type(v)
  if v == nil then return "null" end
  if ty == "string" then return encode_string(v) end
  if ty == "number" then
    if v ~= v or v == math.huge or v == -math.huge then return "null" end
    if v == math.floor(v) and math.abs(v) < 2^52 then return string.format("%d", v) end
    return string.format("%.14g", v)
  end
  if ty == "boolean" then return v and "true" or "false" end
  if ty == "table" then return encode_table(v) end
  error("json: cannot encode " .. ty)
end

function json.encode(v) return encode_value(v) end

-- decode --------------------------------------------------------------------

local function decode_error(s, pos, msg)
  error(string.format("json: %s at position %d", msg, pos), 0)
end

local parse_value

local function skip_ws(s, pos)
  local _, e = s:find("^[ \n\r\t]*", pos)
  return (e or pos - 1) + 1
end

local function parse_string(s, pos)
  local parts = {}
  local i = pos + 1
  while true do
    local j = s:find('["\\]', i)
    if not j then decode_error(s, pos, "unterminated string") end
    parts[#parts + 1] = s:sub(i, j - 1)
    local c = s:sub(j, j)
    if c == '"' then
      return table.concat(parts), j + 1
    end
    local esc = s:sub(j + 1, j + 1)
    local map = { ['"'] = '"', ['\\'] = '\\', ['/'] = '/',
      b = '\b', f = '\f', n = '\n', r = '\r', t = '\t' }
    if map[esc] then
      parts[#parts + 1] = map[esc]
      i = j + 2
    elseif esc == 'u' then
      local hex = s:sub(j + 2, j + 5)
      local cp = tonumber(hex, 16) or decode_error(s, j, "bad \\u escape")
      parts[#parts + 1] = utf8.char(cp)
      i = j + 6
    else
      decode_error(s, j, "bad escape " .. esc)
    end
  end
end

local function parse_number(s, pos)
  local num = s:match("^-?%d+%.?%d*[eE]?[+-]?%d*", pos)
  if not num or #num == 0 then decode_error(s, pos, "invalid number") end
  return tonumber(num), pos + #num
end

local function parse_array(s, pos)
  local arr = {}
  pos = skip_ws(s, pos + 1)
  if s:sub(pos, pos) == "]" then return arr, pos + 1 end
  while true do
    local v
    v, pos = parse_value(s, pos)
    arr[#arr + 1] = v
    pos = skip_ws(s, pos)
    local c = s:sub(pos, pos)
    if c == "]" then return arr, pos + 1 end
    if c ~= "," then decode_error(s, pos, "expected ',' or ']'") end
    pos = skip_ws(s, pos + 1)
  end
end

local function parse_object(s, pos)
  local obj = {}
  pos = skip_ws(s, pos + 1)
  if s:sub(pos, pos) == "}" then return obj, pos + 1 end
  while true do
    pos = skip_ws(s, pos)
    if s:sub(pos, pos) ~= '"' then decode_error(s, pos, "expected string key") end
    local key
    key, pos = parse_string(s, pos)
    pos = skip_ws(s, pos)
    if s:sub(pos, pos) ~= ":" then decode_error(s, pos, "expected ':'") end
    local v
    v, pos = parse_value(s, skip_ws(s, pos + 1))
    obj[key] = v
    pos = skip_ws(s, pos)
    local c = s:sub(pos, pos)
    if c == "}" then return obj, pos + 1 end
    if c ~= "," then decode_error(s, pos, "expected ',' or '}'") end
    pos = pos + 1
  end
end

parse_value = function(s, pos)
  pos = skip_ws(s, pos)
  local c = s:sub(pos, pos)
  if c == '"' then return parse_string(s, pos) end
  if c == "{" then return parse_object(s, pos) end
  if c == "[" then return parse_array(s, pos) end
  if c == "-" or c:match("%d") then return parse_number(s, pos) end
  if s:sub(pos, pos + 3) == "true" then return true, pos + 4 end
  if s:sub(pos, pos + 4) == "false" then return false, pos + 5 end
  if s:sub(pos, pos + 3) == "null" then return nil, pos + 4 end
  decode_error(s, pos, "unexpected character '" .. c .. "'")
end

function json.decode(s)
  if type(s) ~= "string" then error("json: expected string", 2) end
  local v, pos = parse_value(s, 1)
  return v
end
`

// prelude defines the Lua-facing capability surface on top of the single
// native primitive __af_op. Everything here is policy, written in Lua,
// exactly where the design says it should live.
const prelude = `
-- agentflow prelude (phase 1)
-- Native primitive: __af_op(request_json) -> (response_json, ok)

local function op(req)
  local resp, ok = __af_op(json.encode(req))
  if not ok then
    -- failures cross the bridge as JSON strings; unwrap for readable errors,
    -- and raise with level 0 so bubbles don't carry prelude line numbers
    local msg = resp
    local pok, v = pcall(json.decode, resp)
    if pok and type(v) == "string" then msg = v end
    error(msg, 0)
  end
  return json.decode(resp)
end

af = { op = op }

session = {}
function session.inbox() return op({ type = "inbox" }) end
function session.send(text) op({ type = "send", text = text }) end
-- session.push sends to an explicit channel/recipient without an active
-- inbound message (proactive/background egress). Requires the channel.push
-- capability. reply_to is the channel-specific recipient (e.g. telegram
-- chat id).
function session.push(channel, reply_to, text)
  return op({ type = "session.push", channel = channel, reply_to = reply_to, text = text })
end
-- session.push_user sends to a user by identity UUID. Channel-agnostic: pass
-- the UUID from msg.from ("user:<uuid>" -> strip the prefix) of a prior turn.
-- Requires the channel.push capability and the identity layer to be enabled.
function session.push_user(uuid, text)
  return op({ type = "session.push_user", address = "user:" .. uuid, text = text })
end
-- session.exit terminates this session cleanly: the loop is not restarted,
-- timers/requests are reaped, and the parent (if any) receives agent.died.
-- The calling coroutine is never resumed.
function session.exit() op({ type = "session.exit" }) end

log = {}
local function mklog(level)
  return function(msg) op({ type = "log", level = level, msg = tostring(msg) }) end
end
log.debug = mklog("debug")
log.info  = mklog("info")
log.warn  = mklog("warn")
log.error = mklog("error")

time = {}
function time.sleep(seconds) op({ type = "sleep", seconds = seconds }) end

agent = {}
function agent.info()
  return op({ type = "agent.info" })
end
function agent.send(address, payload)
  return op({ type = "agent.send", address = address, payload = payload or {} })
end
function agent.request(address, payload, timeout)
  return op({ type = "agent.request", address = address, payload = payload or {}, timeout = timeout or 30 })
end
function agent.reply(request_id, payload)
  return op({ type = "agent.reply", request = request_id, payload = payload or {} })
end
function agent.list()
  return op({ type = "agent.list" })
end
function agent.spawn(profile, spec)
  return op({ type = "agent.spawn", profile = profile, spec = spec or {} })
end

scheduler = {}
function scheduler.every(interval, opts)
  opts = opts or {}
  return op({ type = "scheduler.every", interval = interval })
end
function scheduler.after(delay, opts)
  opts = opts or {}
  return op({ type = "scheduler.after", delay = delay })
end
function scheduler.cron(expr, opts)
  return op({ type = "scheduler.cron", cron = expr })
end
function scheduler.cancel(timer_id)
  return op({ type = "scheduler.cancel", timer_id = timer_id })
end

store = {}
function store.put(table, key, value, opts)
  opts = opts or {}
  return op({ type = "store.put", table = table, key = key, value = value, ttl = opts.ttl })
end
function store.get(table, key)
  return op({ type = "store.get", table = table, key = key })
end
function store.query(table, query)
  query = query or {}
  query.table = table
  return op({ type = "store.query", query = query })
end
function store.delete(table, key)
  return op({ type = "store.delete", table = table, key = key })
end

memory = {}
function memory.write(record)
  local targets = memory_routing_table(record)
  for _, t in ipairs(targets) do
    store.put(t.store, t.key, t.value, { ttl = t.ttl })
  end
  return true
end
function memory.recall(query, opts)
  return memory_recall_handler(query, opts)
end

tools = {}
function tools.list()
  return op({ type = "tools.list" })
end
function tools.run(name, args, opts)
  local req = { type = "tools.run", tool = name, args = args or {} }
  if opts then
    if opts.confirmed then req.confirmed = true; req.confirm_id = opts.confirm_id end
  end
  return op(req)
end

shell = {}
function shell.spawn(opts)
  opts = opts or {}
  return op({ type = "shell.spawn",
    image      = opts.image,
    workdir    = opts.workdir,
    env        = opts.env,
    network    = opts.network,
    mem_limit  = opts.mem_limit,
    cpu_limit  = opts.cpu_limit,
    provider   = opts.provider,
    host       = opts.host,
    user       = opts.user,
    password   = opts.password,
    key_file   = opts.key_file,
    shell_opts = opts.shell_opts,
  })
end
function shell.exec(handle_id, command)
  return op({ type = "shell.exec", shell_handle = handle_id, cmd = command })
end
function shell.write(handle_id, filepath, content)
  return op({ type = "shell.write", shell_handle = handle_id, path = filepath, content = content })
end
function shell.destroy(handle_id)
  return op({ type = "shell.destroy", shell_handle = handle_id })
end

local function with_opts(req, opts)
  if opts then for k, v in pairs(opts) do req[k] = v end end
  return req
end

llm = {}
function llm.chat(messages, opts)
  return op(with_opts({ type = "llm.chat", messages = messages }, opts))
end

-- llm.stream returns an iterator: for delta in llm.stream(msgs) do ... end
function llm.stream(messages, opts)
  local h = op(with_opts({ type = "llm.stream.open", messages = messages }, opts))
  local done = false
  return function()
    if done then return nil end
    local r = op({ type = "llm.stream.next", stream = h.id })
    if r.done then done = true; return nil end
    return r.delta
  end
end
`

// LoadBase installs the json library and the prelude into the state.
func (s *State) LoadBase() error {
	if err := s.Eval("@json", jsonLib); err != nil {
		return err
	}
	return s.Eval("@prelude", prelude)
}
