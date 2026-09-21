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
-- session.state is a small durable key/value store for this session alone: a
-- cursor, a step count, the last thing said. It survives a restart, and in a
-- fleet it follows the session to whichever instance owns it next. Needs the
-- session.state capability. Keys may contain letters, digits and . _ - :
-- (128 chars); values are any JSON-encodable Lua value, up to 64 KiB, and 256
-- keys per session. get returns nil for a key that was never set.
session.state = {}
function session.state.set(key, value)
  return op({ type = "session.state.set", key = key, value = value })
end
function session.state.get(key)
  return op({ type = "session.state.get", key = key })
end
function session.state.delete(key)
  return op({ type = "session.state.delete", key = key })
end
function session.state.list()
  return op({ type = "session.state.list" })
end
-- session.send(text, opts?) replies to the active inbound message. opts may
-- carry attachments = array of part tables ({type="image", mime=..., handle=...})
-- — typically msg.attachments forwarded from the inbound message. Channels
-- that cannot deliver media fail the op (honest, never a silent drop).
function session.send(text, opts)
  local req = { type = "send", text = text }
  if opts and opts.attachments then req.attachments = opts.attachments end
  op(req)
end
-- session.push sends to an explicit channel/recipient without an active
-- inbound message (proactive/background egress). Requires the channel.push
-- capability. reply_to is the channel-specific recipient (e.g. telegram
-- chat id). opts.attachments as in session.send.
function session.push(channel, reply_to, text, opts)
  local req = { type = "session.push", channel = channel, reply_to = reply_to, text = text }
  if opts and opts.attachments then req.attachments = opts.attachments end
  return op(req)
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
-- agent.info() -> this session's own identity: name, session_id, address,
-- model, instructions, history_budget, memory bindings, shell, skills and
-- capabilities. Two fields follow the *person* being served rather than the
-- agent: a profile may name a model and may add a layer of instructions, and
-- when it does, model is that model (with agent_model carrying what the agent
-- would otherwise have used) and instructions is the agent's instructions with
-- the person's layer appended as its own paragraph — instructions_append
-- carries that layer alone. A loop that reads these two fields honours a
-- per-user override without knowing it exists; one that wants to branch on the
-- preference can read agent_model and instructions_append.
function agent.info()
  return op({ type = "agent.info" })
end
-- agent.config() -> this agent's own profile: name, model, the instructions
-- path, goal{type, success_signal, max_turns, on_goal_met} (when the profile
-- declares one under extras.goal), and the remaining extras (a pm options
-- block, a workflow name, ...). Secret references render as opaque markers
-- ({env="VAR"} / {cred="service"}) — resolved values never appear here.
function agent.config()
  return op({ type = "agent.config" })
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
  return op({ type = "store.put", table = table, key = key, value = value, ttl = opts.ttl, vector = opts.vector })
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
-- store.scopes(table) -> { "user:<uuid>", ... } — the user scopes present in
-- a table, sorted. Maintenance provenance only (system/scheduler sessions,
-- e.g. the nightly distiller); channel and agent-hop sessions are denied —
-- scope enumeration must never be reachable from a user's own turn.
function store.scopes(table)
  return op({ type = "store.scopes", table = table })
end

-- The user surface: read-only facts about the person this turn belongs to.
-- A loop learns a name and a channel, never an email or a secret — credential
-- values are only ever reached as the opaque auth={service=...} reference the
-- engine resolves.
user = {}
-- user.current() -> {ok, registered, id, display_name, identities}. A turn with
-- no registered user (engine work, or a handle nobody has linked) reports
-- registered=false, so a loop can greet a guest differently.
function user.current()
  return op({ type = "user.current" })
end
-- user.usage() -> {ok, input, output, cached, reasoning, calls, failed,
-- billable, used, limit, remaining, unlimited, in_flight} for the current user,
-- so a loop can warn before the quota bites.
function user.usage()
  return op({ type = "user.usage" })
end
-- user.has_credential(service) -> {ok, service, present}: whether the user holds
-- a credential. The value is never exposed; only the engine can use it.
function user.has_credential(service)
  return op({ type = "user.has_credential", service = service })
end
-- user.get(id) -> one profile, and user.list() -> every profile. Maintenance
-- provenance only (system/scheduler): a user's own turn cannot read another
-- account.
function user.get(id)
  return op({ type = "user.get", user_id = id })
end
function user.list()
  return op({ type = "user.list" })
end

memory = {}
function memory.write(record)
  local targets = memory_routing_table(record)
  local info = agent.info()
  local mem = info.memory or {}
  local embed_model = mem.embed_model
  local stores = mem.stores or {}
  for _, t in ipairs(targets) do
    local opts = { ttl = t.ttl }
    -- Embed only when the profile names an embed_model AND the target
    -- store's backend can hold vectors; other stores get plain puts.
    if embed_model and embed_model ~= "" then
      local can_vector = false
      local s = stores[t.store]
      if s and s.features then
        for _, f in ipairs(s.features) do
          if f == "vector" then can_vector = true; break end
        end
      end
      if can_vector then
        local text = t.value
        if type(text) == "table" then text = text.text or json.encode(text) end
        -- Multimodal records: attachments ride alongside the text as one
        -- merged embedding (Jina content group), so recall by text finds the
        -- media too. Requires an omni embedding model (e.g.
        -- jina-embeddings-v5-omni-small); text-only models error honestly.
        local atts = record.attachments
        if atts and #atts > 0 then
          local parts = {}
          if type(text) == "string" and #text > 0 then
            table.insert(parts, text)
          end
          for _, att in ipairs(atts) do table.insert(parts, att) end
          local r = llm.embed(parts, { model = embed_model, merged = true })
          if r and r.vectors and r.vectors[1] then
            opts.vector = r.vectors[1]
          end
        elseif type(text) == "string" and #text > 0 then
          local r = llm.embed({ text }, { model = embed_model })
          if r and r.vectors and r.vectors[1] then
            opts.vector = r.vectors[1]
          end
        end
      end
    end
    store.put(t.store, t.key, t.value, opts)
  end
  return true
end
function memory.recall(query, opts)
  -- The memory profile selects the recall handler; plugin:semantic is
  -- handled by its support chunk, everything else uses the default.
  local info = agent.info()
  local recall = info.memory and info.memory.recall
  if recall == "plugin:semantic" and memory_semantic_recall_handler then
    return memory_semantic_recall_handler(query, opts)
  end
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

-- Lua-declared tools: a loop (or a support chunk) registers its own
-- model-visible tools with tool.def. Declared tools merge into tools.list and
-- intercept tools.run; the handler runs synchronously on the loop coroutine,
-- so it may itself call any op (llm.chat, memory.*, ...).
--
-- Visibility follows the same rule as Go-registered tools: the agent's skills
-- list, or tools.policy.default when skills is empty. A declared tool this
-- agent cannot see is left out of tools.list — which is what lets one shared
-- loop directory carry tools only some agents get. That is a behavior change
-- for a deployment with tools.policy.default: none and no skills: declared
-- tools used to be listed unconditionally and now are not. The filter is
-- applied before shadowing, so a hidden declared tool does not suppress a
-- visible Go tool of the same name.
--
-- tools.run applies the same rule: a declared name this agent cannot see is
-- not dispatchable and falls through to the Go path, which answers
-- not-available like it does for any other invisible tool. Registered tools
-- were always refused at dispatch, and tools.run is the model-facing path — the
-- one a hallucinated or injected tool call travels — so the two must agree. A
-- loop that wants its own handler can call the function directly; it does not
-- need tools.run to reach a tool its model is not shown.
--
-- Declared tools are the loop's own code: the Go tools policy (forbidden,
-- needs_confirm, permission: forbidden) does not gate them.
--
-- tools.policy.overrides may target a declared tool by name: its description
-- (literal or {prompt: key} from the prompts registry) replaces the spec's,
-- and params.<param>.description refines one declared parameter. The spec's
-- own description and params are the fallback when no override names it, and
-- an override can never add or remove a list entry. Overrides are resolved
-- when the list is built, not at boot, so editing a file:-backed prompt is
-- reflected on the next tools.list().
tool = {}
local lua_tools = {}
local go_tools_list = tools.list
local go_tools_run = tools.run

function tool.def(spec)
  assert(type(spec) == "table", "tool.def: spec must be a table")
  assert(type(spec.name) == "string" and #spec.name > 0, "tool.def: spec.name must be a non-empty string")
  assert(type(spec.handler) == "function", "tool.def: spec.handler must be a function")
  if spec.params ~= nil then
    assert(type(spec.params) == "table", "tool.def: spec.params must be a JSON-schema table")
  end
  lua_tools[spec.name] = spec
end

-- tool_override_params returns the declared schema with any overridden param
-- description merged in. The spec's own table is never mutated: it belongs to
-- the loop and is reused on every turn. A param the schema does not declare is
-- skipped, matching the Go registry's rule.
local function tool_override_params(params, ovparams)
  local base = params or { type = "object", properties = {} }
  if ovparams == nil then
    return base
  end
  local out = {}
  for k, v in pairs(base) do out[k] = v end
  local props = {}
  for k, v in pairs(base.properties or {}) do props[k] = v end
  for pname, po in pairs(ovparams) do
    local declared = props[pname]
    if po.description and type(declared) == "table" then
      local merged = {}
      for k, v in pairs(declared) do merged[k] = v end
      merged.description = po.description
      props[pname] = merged
    end
  end
  out.properties = props
  return out
end

-- tool_declared asks the engine which of these declared names this agent can
-- see, and for the config overrides targeting them. Both tools.list and
-- tools.run go through it, so the surface the model is shown and the surface
-- it can invoke are decided by one answer.
local function tool_declared(names)
  local res = op({ type = "tools.declared", tool_names = names }) or {}
  return res.visible or {}, res.overrides or {}
end

function tools.list()
  -- The full declared set goes over the wire either way: a declared-but-hidden
  -- tool still counts as declared, so an override aimed at it is not reported
  -- as naming nothing. Resolved per call against the live prompt registry, so
  -- a reloaded file:-backed prompt shows up here without a session restart.
  local visible, overrides = {}, {}
  if next(lua_tools) ~= nil then
    local names = {}
    for name in pairs(lua_tools) do names[#names + 1] = name end
    visible, overrides = tool_declared(names)
  end

  local out = {}
  for _, def in ipairs(go_tools_list()) do
    local f = def["function"]
    -- A visible declared tool shadows a Go tool of the same name. One this
    -- agent cannot see must not suppress the Go tool.
    if not (f and visible[f.name]) then
      out[#out + 1] = def
    end
  end
  for name, spec in pairs(lua_tools) do
    if visible[name] then
      local ov = overrides[name]
      out[#out + 1] = {
        type = "function",
        ["function"] = {
          name = name,
          description = (ov and ov.description) or spec.description or "",
          parameters = tool_override_params(spec.params, ov and ov.params),
        },
      }
    end
  end
  return out
end

function tools.run(name, args, opts)
  local spec = lua_tools[name]
  if spec == nil then
    return go_tools_run(name, args, opts)
  end
  -- A declared tool this agent cannot see is not dispatchable either: it is
  -- absent from tools.list, so a call naming it can only come from a
  -- hallucinated or injected tool call. Registered tools are already refused
  -- at dispatch; this makes the two surfaces agree. Fall through to the Go
  -- path so the answer is the same not-available result.
  local visible = tool_declared({ name })
  if not visible[name] then
    return go_tools_run(name, args, opts)
  end
  local ok, res = pcall(spec.handler, args or {})
  if not ok then
    return { ok = false, tool = name, error = tostring(res) }
  end
  if res == nil then
    res = { ok = true, tool = name }
  end
  return res
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

-- http issues a single HTTP request and returns the full response. No
-- streaming, no retry; a loop that wants retry wraps it. Secrets go in
-- headers via os.env so they never sit in Lua source.
http = {}
function http.request(opts)
  opts = opts or {}
  return op({
    type        = "http.request",
    method      = opts.method or "GET",
    url         = opts.url,
    headers     = opts.headers,
    body        = opts.body,
    query       = opts.query,
    json        = opts.json,
    timeout     = opts.timeout,
    auth        = opts.auth,   -- {service=...}: a stored credential, resolved by Go
  })
end
function http.get(url, opts)
  opts = opts or {}; opts.method = "GET"; opts.url = url; return http.request(opts)
end
function http.post(url, body, opts)
  opts = opts or {}; opts.method = "POST"; opts.url = url; opts.body = body; return http.request(opts)
end

-- os: the sandbox's minimal process surface. os.env reads a process
-- environment variable at call time (for API keys and other secrets that must
-- not be hardcoded in Lua source); it returns "" if unset. os.time/os.date are
-- the wall clock, UTC only (see below). No other os.* exists — there is no
-- io, no filesystem, no process control.
os = {}
function os.env(name) return op({ type = "os.env", name = name }) end

-- Wall clock. Everything here is UTC: the engine has no notion of a local
-- timezone, so the fields are the same on every host. The native primitive is
-- __af_now() (Unix seconds, from the process clock).
local SECS_PER_DAY = 86400

-- days-since-epoch -> y, m, d (Howard Hinnant's civil_from_days).
local function civil_from_days(z)
  z = z + 719468
  local era = math.floor(z / 146097)
  local doe = z - era * 146097
  local yoe = math.floor((doe - math.floor(doe / 1460) + math.floor(doe / 36524) - math.floor(doe / 146096)) / 365)
  local y = yoe + era * 400
  local doy = doe - (365 * yoe + math.floor(yoe / 4) - math.floor(yoe / 100))
  local mp = math.floor((5 * doy + 2) / 153)
  local d = doy - math.floor((153 * mp + 2) / 5) + 1
  local m = mp + (mp < 10 and 3 or -9)
  if m <= 2 then y = y + 1 end
  return y, m, d
end

local function days_from_civil(y, m, d)
  if m <= 2 then y = y - 1 end
  local era = math.floor(y / 400)
  local yoe = y - era * 400
  local mp = (m + 9) % 12
  local doy = math.floor((153 * mp + 2) / 5) + d - 1
  local doe = yoe * 365 + math.floor(yoe / 4) - math.floor(yoe / 100) + doy
  return era * 146097 + doe - 719468
end

local WDAY = { "Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday" }
local MONTH = { "January", "February", "March", "April", "May", "June",
                "July", "August", "September", "October", "November", "December" }

-- The Lua os.date table for a UTC instant: year/month/day/hour/min/sec, wday
-- (1 = Sunday .. 7 = Saturday), yday (1-366), isdst (always false).
local function date_fields(t)
  local days = math.floor(t / SECS_PER_DAY)
  local secs = t - days * SECS_PER_DAY
  local y, m, d = civil_from_days(days)
  return {
    year = y, month = m, day = d,
    hour = math.floor(secs / 3600),
    min = math.floor((secs % 3600) / 60),
    sec = secs % 60,
    wday = (days + 4) % 7 + 1,
    yday = days - days_from_civil(y, 1, 1) + 1,
    isdst = false,
  }
end

local function pad2(n) return string.format("%02d", n) end

-- strftime subset: %Y %y %m %d %e %H %I %M %S %p %A %a %B %b %j %w %u %Z %%.
-- An unknown directive is left verbatim (a literal "%" then the character).
local function strftime(f, t)
  local dt = date_fields(t)
  return (f:gsub("%%([%a%%])", function(c)
    if c == "Y" then return string.format("%04d", dt.year) end
    if c == "y" then return pad2(dt.year % 100) end
    if c == "m" then return pad2(dt.month) end
    if c == "d" then return pad2(dt.day) end
    if c == "e" then return string.format("%2d", dt.day) end
    if c == "H" then return pad2(dt.hour) end
    if c == "I" then return pad2(((dt.hour + 11) % 12) + 1) end
    if c == "M" then return pad2(dt.min) end
    if c == "S" then return pad2(dt.sec) end
    if c == "p" then return dt.hour < 12 and "AM" or "PM" end
    if c == "A" then return WDAY[dt.wday] end
    if c == "a" then return string.sub(WDAY[dt.wday], 1, 3) end
    if c == "B" then return MONTH[dt.month] end
    if c == "b" then return string.sub(MONTH[dt.month], 1, 3) end
    if c == "j" then return string.format("%03d", dt.yday) end
    if c == "w" then return tostring(dt.wday - 1) end
    if c == "u" then return tostring(dt.wday == 1 and 7 or dt.wday - 1) end
    if c == "Z" then return "UTC" end
    if c == "%" then return "%" end
    return "%" .. c
  end))
end

-- os.time() -> Unix seconds, no arguments. (Real Lua's os.time(table) is not
-- implemented: a table would silently mean "now" here, which is worse than an
-- error. Convert with os.date/os.time arithmetic instead.)
function os.time(...)
  if select("#", ...) > 0 then
    error("os.time: takes no arguments (UTC Unix seconds)", 2)
  end
  return __af_now()
end

-- os.date([format,] time) -> a UTC date table, or a formatted string when
-- the format is a strftime string ("*t"/"!*t"/nil all return the table; a
-- leading "!" is accepted and is a no-op because the clock is always UTC).
-- The time argument is Unix seconds and defaults to now.
function os.date(format, t)
  if type(format) == "number" and t == nil then
    t, format = format, nil
  end
  if t == nil then t = __af_now() end
  if type(t) ~= "number" then
    error("os.date: time must be a number (Unix seconds)", 2)
  end
  if format == nil or format == "" or format == "*t" or format == "!*t" then
    return date_fields(t)
  end
  if type(format) ~= "string" then
    error("os.date: format must be a string", 2)
  end
  if string.sub(format, 1, 1) == "!" then format = string.sub(format, 2) end
  if format == "*t" then return date_fields(t) end
  return strftime(format, t)
end

-- runtime.config surface: deployment configuration as read-only Lua data.
runtime = {}
-- runtime.triggers() -> { ok, triggers = [ { name, kind, run_on_boot, target =
-- {profile}, payload, and the schedule fields (event={channel,match} |
-- cron | every) }, ... ] } — the merged triggers/*.yaml (or top-level
-- triggers:) list, snapshotted at boot. This replaces baking CRON_TASKS /
-- ROUTES tables into Lua source. every:/cron: entries are also fired by the
-- engine itself; reading them here is unchanged and still safe (the shape is
-- stable), so a loop can use the schedule data for its own bookkeeping.
function runtime.triggers()
  return op({ type = "runtime.triggers" })
end

-- credential.get(name) -> { ok, name, value } — the stored engine-wide
-- secret, ONLY when the agent's profile lists the name under credentials:[]
-- (empty/missing list = denied, error says so). Every access is logged and
-- counted; the value is never journaled (op responses are not journal
-- records) and should never be echoed into session.send.
credential = {}
function credential.get(name)
  return op({ type = "credential.get", cred_name = name })
end

-- mail: IMAP fetch and SMTP send, in-process via the net.mail runtime cap.
-- Like http.request, auth={service=...} names a stored credential resolved by
-- Go; the password never crosses the Lua bridge. Host/port/username come from
-- the loop (typically via os.env) and never sit in Lua source.
mail = {}
-- mail.imap_fetch{host, port, user, mailbox, unseen, limit, auth} ->
--   {ok, mailbox, count, messages:[{seq,uid,flags,subject,from,to,date,body}]}
function mail.imap_fetch(opts)
  opts = opts or {}
  return op({
    type     = "mail.imap.fetch",
    mail_host = opts.host,
    mail_port = opts.port,
    mail_user = opts.user,
    mailbox   = opts.mailbox,
    unseen    = opts.unseen,
    limit     = opts.limit,
    auth      = opts.auth,   -- {service=...}: stored IMAP password
  })
end
-- mail.smtp_send{host, port, user, from, to, subject, text_body, auth} ->
--   {ok, sent}
function mail.smtp_send(opts)
  opts = opts or {}
  return op({
    type      = "mail.smtp.send",
    mail_host = opts.host,
    mail_port = opts.port,
    mail_user = opts.user,
    mail_from = opts.from,
    mail_to   = opts.to,     -- array of recipient addresses
    subject   = opts.subject,
    text_body = opts.text_body or opts.body,
    auth      = opts.auth,   -- {service=...}: stored SMTP password
  })
end

local function with_opts(req, opts)
  if opts then for k, v in pairs(opts) do req[k] = v end end
  return req
end

llm = {}
-- llm.chat(messages, opts): messages are {role, content} tables, or
-- {role, parts = {...}} for multimodal turns. Parts are tables:
--   {type="text", text="..."} or
--   {type="image"|"audio"|"video"|"file", mime="...", handle="media:...",
--    name="..."} — media parts typically come from msg.attachments and the
--   runtime resolves the handle to bytes; data (base64) and url also work.
-- Providers that cannot take a part type fail the call with a clear error.
-- opts.thinking overrides the model's default thinking level for this call:
-- "off" | "low" | "medium" | "high" | "xhigh" | "max" — one vocabulary,
-- mapped per provider (Anthropic budget, OpenAI effort, Gemini budget).
-- The reply carries reasoning the provider sent: reply.thinking (text),
-- reply.thinking_blocks (raw blocks), usage.reasoning (token count). Pass
-- the blocks back on an assistant turn (thinking_blocks = reply.thinking_blocks)
-- to continue Anthropic tool loops with thinking enabled.
function llm.chat(messages, opts)
  return op(with_opts({ type = "llm.chat", messages = messages }, opts))
end

-- llm.embed(inputs, opts) -> { vectors = { {..}, .. }, usage = { input = n } }
-- inputs may be a single string, an array of strings, or a mixed array of
-- strings and part tables ({type="image", handle="media:...", url=...}) for
-- multimodal embeddings (Jina jina-embeddings-v5-omni-*: text+image+video+
-- audio+pdf in one vector space). Strings normalize to text parts.
-- opts: model, task (retrieval.query | retrieval.passage | text-matching |
-- clustering | classification), dimensions (Matryoshka truncation),
-- merged = true (fold all inputs into ONE embedding).
-- Text-only batches serialize as plain strings on the wire — vanilla
-- OpenAI/Ollama/vLLM endpoints see the exact historical request.
function llm.embed(inputs, opts)
  local arr = type(inputs) == "table" and inputs or { inputs }
  local parts = {}
  for _, item in ipairs(arr) do
    if type(item) == "string" then
      table.insert(parts, { type = "text", text = item })
    elseif type(item) == "table" then
      table.insert(parts, item)
    end
  end
  return op(with_opts({ type = "llm.embed", inputs = parts }, opts))
end

-- llm.rerank(query, documents, opts) -> { results = { { index = i, score = s }, .. } }
-- index is 0-based into documents. opts.model names a provider="rerank"
-- models: entry; opts.top_n trims the ranked list.
function llm.rerank(query, documents, opts)
  return op(with_opts({ type = "llm.rerank", text = query, documents = documents }, opts))
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

files = {}
-- User-scoped file store (cap: files). Scope is engine-resolved from the
-- turn's identity — a loop can never name another user's scope. Bytes never
-- cross the bridge: put takes utf-8 content or {data=base64, mime=...};
-- read returns the handle + metadata ({path, handle, size, mime, ts,
-- revision}), materialization is engine-side.
-- files.put(project, path, content|{data=...,mime=...}, {mime=...}?) -> {ok, entry}
function files.put(project, path, content, opts)
  opts = opts or {}
  local req = { type = "files.put", project = project, path = path }
  if type(content) == "table" then
    req.data = content.data
    req.handle = content.handle
    req.mime = content.mime or opts.mime
  else
    req.content = content
    req.mime = opts.mime
  end
  return op(req)
end
-- files.read(project, path) -> {ok, entry}
function files.read(project, path)
  return op({ type = "files.read", project = project, path = path })
end
-- files.list(project, prefix?) -> {ok, entries}
function files.list(project, prefix)
  return op({ type = "files.list", project = project, path = prefix or "" })
end
-- files.projects() -> {ok, projects}: the project names in the caller's own
-- scope. Scope is engine-resolved, so a loop can only ever see its own; there
-- is no way to name someone else's.
function files.projects()
  return op({ type = "files.projects" })
end
-- files.delete(project, path) -> {ok=true}
function files.delete(project, path)
  return op({ type = "files.delete", project = project, path = path })
end
-- files.commit(project, {ref="main", message=...}?) -> {ok, commit}. A commit
-- snapshots the whole working tree (content-addressed: unchanged files are
-- shared) and moves the named ref; the parent chain is the history.
function files.commit(project, opts)
  opts = opts or {}
  return op({ type = "files.commit", project = project, ref = opts.ref, commit_msg = opts.message })
end
-- files.checkout(project, ref|commit_id) -> {ok, manifest={commit, files}}
function files.checkout(project, ref)
  return op({ type = "files.checkout", project = project, ref = ref })
end
-- Per-session scratch space, TTL'd (files.scratch_ttl). Keyed by the calling
-- session — other sessions cannot see it.
files.scratch = {}
function files.scratch.put(name, content, opts)
  opts = opts or {}
  local req = { type = "files.scratch.put", path = name }
  if type(content) == "table" then
    req.data = content.data
    req.handle = content.handle
    req.mime = content.mime or opts.mime
  else
    req.content = content
    req.mime = opts.mime
  end
  return op(req)
end
function files.scratch.read(name)
  return op({ type = "files.scratch.read", path = name })
end
function files.scratch.list()
  return op({ type = "files.scratch.list" })
end
`

// LoadBase installs the json library and the prelude into the state.
func (s *State) LoadBase() error {
	if err := s.Eval("@json", jsonLib); err != nil {
		return err
	}
	return s.Eval("@prelude", prelude)
}
