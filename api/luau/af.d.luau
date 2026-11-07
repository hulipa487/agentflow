---@meta
-- Editor stubs for the AgentFlow Lua API (LuaLS completion and hover). Never
-- executed. The prelude in internal/vm/prelude.go is the source of truth and
-- its version (af.version) is the compatibility contract; every chunk must
-- declare it in its leading comment block:
--
--   -- af-prelude-version: 1
--
-- The canonical copy lives in the runtime repo at api/luau/af.d.luau; Studio's
-- docs/luau/af.d.luau is synced from it with `make docs-sync`.

---@class MediaPart
---@field type string "text" | "image" | "audio" | "video" | "file"
---@field text? string
---@field mime? string
---@field data? string base64
---@field url? string
---@field handle? string "media:<sha256>" blob handle
---@field name? string

---@class Provenance
---@field kind string "channel" | "agent" | "scheduler" | "system"
---@field principal string
---@field parent? string
---@field request_id? string use this, never a payload field, for agent.reply
---@field user_uuid? string the tenant; nil = unknown, "" = known to have none
---@field membership_uuid? string set when the turn acts in a group context

---@class Message
---@field id string
---@field type string "user" | "timer" | "agent" | "confirm" | "system" | ...
---@field from string
---@field to? string
---@field text? string
---@field channel? string
---@field reply_to? string
---@field attachments? MediaPart[]
---@field payload? table
---@field provenance? Provenance
---@field ts? integer

---@class ToolCall
---@field id? string
---@field name string
---@field args? table

---@class Usage
---@field input integer
---@field output integer
---@field cached integer
---@field cache_write integer
---@field reasoning integer

---@class LLMReply
---@field text? string
---@field thinking? string
---@field thinking_blocks? table[] pass back on the assistant turn to continue Anthropic tool loops
---@field tool_calls? ToolCall[]
---@field usage? Usage

---@class ChatMessage
---@field role string "system" | "user" | "assistant" | "tool"
---@field content? string
---@field parts? MediaPart[]
---@field tool_calls? ToolCall[]
---@field tool_call_id? string
---@field tool_result? any
---@field thinking_blocks? table[]

---@class ChatOpts
---@field model? string a key of `models:`; default is the agent's model
---@field temperature? number
---@field max_tokens? integer
---@field thinking? string "off" | "low" | "medium" | "high" | "xhigh" | "max"
---@field tools? table[] {name, description, parameters} or tools.list() entries
---@field tool_choice? string

---@class MemoryStoreBinding
---@field backend string
---@field table string
---@field features string[]
---@field locality "deployment" | "region" | "instance" how far the store's data reaches: durable across the deployment, shared within the region, or lost with the process

---@class AgentInfo
---@field name string
---@field session_id string
---@field address string
---@field model string
---@field instructions? string base instructions plus the user's layer, if any
---@field history_budget? integer
---@field skills? string[]
---@field capabilities? string[]
---@field memory? { stores?: table<string, MemoryStoreBinding>, recall?: string, embed_model?: string }
---@field prompts? table<string, string>
---@field extras? table

---@class TimerRef
---@field id string

---@class af
---@field version integer the prelude API version this core implements
---@field op fun(req: table): any raw op call; prefer the typed helpers
af = {}

-- session -------------------------------------------------------------------

session = {}

--- Wait for the next inbound message. Parks the session.
---@return Message
function session.inbox() end

--- Reply to the active inbound message.
---@param text string
---@param opts? { attachments?: MediaPart[] }
function session.send(text, opts) end

--- Proactive send to a channel recipient. Capability: channel.push.
---@param channel string
---@param reply_to string
---@param text string
---@param opts? { attachments?: MediaPart[] }
function session.push(channel, reply_to, text, opts) end

--- Proactive send to a user by identity UUID. Capability: channel.push.
---@param uuid string
---@param text string
function session.push_user(uuid, text) end

--- End this session permanently; the loop is never resumed.
function session.exit() end

session.state = {}

--- Capability: session.state. Values up to 64 KiB, 256 keys per session.
---@param key string
---@param value any
function session.state.set(key, value) end

---@param key string
---@return any value nil when never set
function session.state.get(key) end

---@param key string
function session.state.delete(key) end

---@return table<string, any>
function session.state.list() end

-- log / time / os --------------------------------------------------------------

log = {}
---@param msg any
function log.debug(msg) end
---@param msg any
function log.info(msg) end
---@param msg any
function log.warn(msg) end
---@param msg any
function log.error(msg) end

time = {}
--- Park the session for a duration (idle, not busy-waiting).
---@param seconds number
function time.sleep(seconds) end

os = {}
---@param name string
---@return string?
function os.env(name) end

--- UTC Unix seconds. Takes no arguments.
---@return integer
function os.time() end

--- A UTC date table ("*t" or nil) or a strftime-formatted string.
---@param format? string
---@param t? integer Unix seconds, default now
---@return string|table
function os.date(format, t) end

-- agent ---------------------------------------------------------------------

agent = {}

---@return AgentInfo
function agent.info() end

--- The agent's own profile as data. Secrets appear as {env=...} / {cred=...}.
---@return { name: string, model: string, prompts?: table, goal?: table, extras?: table }
function agent.config() end

--- Capability: agent.send. Subject to can_contact.
---@param address string "agent:<name>" | "session:<id>"
---@param payload? table
function agent.send(address, payload) end

--- Capability: agent.request. Waits for agent.reply.
---@param address string
---@param payload? table
---@param timeout? number seconds, default 30
---@return Message
function agent.request(address, payload, timeout) end

---@param request_id string msg.provenance.request_id
---@param payload? table
function agent.reply(request_id, payload) end

---@return { session_id: string, agent: string, parent_id?: string }[]
function agent.list() end

--- Capability: agent.spawn. Starts a child from a profiles.agent template.
---@param profile string
---@param spec? table accepted but ignored
---@return { address: string, session_id: string, agent: string }
function agent.spawn(profile, spec) end

-- scheduler -----------------------------------------------------------------

scheduler = {}

---@param interval number seconds
---@param opts? table
---@return TimerRef
function scheduler.every(interval, opts) end

---@param delay number seconds
---@param opts? table
---@return TimerRef
function scheduler.after(delay, opts) end

--- Cron subset: */N minutes or fixed minute/hour; the rest must be *.
---@param expr string
---@param opts? table
---@return TimerRef
function scheduler.cron(expr, opts) end

---@param timer_id string
function scheduler.cancel(timer_id) end

-- store / memory ------------------------------------------------------------

store = {}

--- Capability: memory.
---@param table string a store name from the agent's memory profile
---@param key string
---@param value any
---@param opts? { ttl?: number, vector?: number[] }
function store.put(table, key, value, opts) end

---@param table string
---@param key string
---@return any
function store.get(table, key) end

---@param table string
---@param query? table
---@return table[]
function store.query(table, query) end

---@param table string
---@param key string
function store.delete(table, key) end

--- Maintenance turns only (system/scheduler).
---@param table string
---@return string[]
function store.scopes(table) end

memory = {}

--- Route a record through the memory profile's write plugins (embedding when
--- the profile has embed_model and the store supports vectors).
---@param record table
---@return boolean
function memory.write(record) end

--- Recall through the profile's recall plugin (plugin:recency, plugin:semantic, ...).
---@param query table e.g. { store = "dialogue", kind = "text", text = "...", k = 5 }
---@param opts? table
---@return { key: string, value: any, score?: number }[]
function memory.recall(query, opts) end

-- user ------------------------------------------------------------------------

user = {}

---@return { ok: boolean, registered: boolean, id?: string, display_name?: string, identities?: table[] }
function user.current() end

---@return { ok: boolean, input: integer, output: integer, cached: integer, reasoning: integer, calls: integer, failed: integer, billable: integer, used: integer, limit: integer, remaining: integer, unlimited: boolean, in_flight: integer }
function user.usage() end

--- Whether the user holds a credential; the value is never exposed.
---@param service string
---@return { ok: boolean, service: string, present: boolean }
function user.has_credential(service) end

--- Maintenance turns only.
---@param id string
---@return table
function user.get(id) end

--- Maintenance turns only.
---@return table[]
function user.list() end

-- llm -----------------------------------------------------------------------

llm = {}

--- Capability: llm.chat.
---@param messages ChatMessage[]
---@param opts? ChatOpts
---@return LLMReply
function llm.chat(messages, opts) end

--- Token stream. Call next_delta() directly in a while loop (a generic `for`
--- cannot yield across the bridge) and call close() on early exit.
---@param messages ChatMessage[]
---@param opts? ChatOpts
---@return fun(): string? next_delta
---@return fun() close
function llm.stream(messages, opts) end

---@param inputs string|string[]|(string|MediaPart)[]
---@param opts? { model?: string, task?: string, dimensions?: integer, merged?: boolean }
---@return { vectors: number[][], usage?: Usage }
function llm.embed(inputs, opts) end

--- Result indexes are zero-based.
---@param query string
---@param documents string[]
---@param opts? { model?: string, top_n?: integer }
---@return { results: { index: integer, score: number }[] }
function llm.rerank(query, documents, opts) end

-- tools -----------------------------------------------------------------------

tools = {}

--- Capability: tools. Builtin, declarative HTTP, MCP and visible tool.def tools.
---@return table[]
function tools.list() end

---@param name string
---@param args? table
---@param opts? { confirmed?: boolean, confirm_id?: string }
---@return any
function tools.run(name, args, opts) end

tool = {}

--- Declare a tool implemented in Lua. It becomes visible when the agent's
--- skills (or tools.policy) expose its name.
---@param spec { name: string, description?: string, params?: table, handler: fun(args: table): any }
function tool.def(spec) end

-- shell -----------------------------------------------------------------------

shell = {}

--- Capability: shell.exec.
---@param opts? { provider?: string, image?: string, workdir?: string, env?: table, network?: string, mem_limit?: string, cpu_limit?: string, host?: string, user?: string, password?: string, key_file?: string, volumes?: table, project?: string, ref?: string, scratch_mount?: string, shell_opts?: table }
---@return table handle
function shell.spawn(opts) end

---@param handle_id string
---@param command string
---@return { stdout: string, stderr: string, exit_code: integer }
function shell.exec(handle_id, command) end

---@param handle_id string
---@param filepath string
---@param content string
function shell.write(handle_id, filepath, content) end

---@param handle_id string
function shell.destroy(handle_id) end

-- http ------------------------------------------------------------------------

http = {}

--- Capability: net.http. Outbound policy enforced in the dialer (netguard).
---@param opts { url: string, method?: string, headers?: table<string,string>, body?: string, json?: table, query?: table<string,string>, auth?: { service: string }, timeout?: number, save_to?: string }
---@return { ok: boolean, status: integer, headers: table, body: string, truncated?: boolean, saved?: table }
function http.request(opts) end

---@param url string
---@param opts? table
function http.get(url, opts) end

---@param url string
---@param body? string
---@param opts? table
function http.post(url, body, opts) end

-- mail ------------------------------------------------------------------------

mail = {}

--- Capability: net.mail. auth names a stored credential; the password never reaches Lua.
---@param opts { host: string, port?: integer, user: string, mailbox?: string, unseen?: boolean, limit?: integer, auth: { service: string } }
---@return { ok: boolean, mailbox: string, count: integer, messages: table[] }
function mail.imap_fetch(opts) end

---@param opts { host: string, port?: integer, user: string, from: string, to: string[], subject: string, text_body?: string, body?: string, auth: { service: string } }
---@return { ok: boolean, sent: boolean }
function mail.smtp_send(opts) end

-- files -----------------------------------------------------------------------

files = {}

--- Capability: files. Scoped to the user (or agent:<name> without identity).
---@param project string
---@param path string
---@param content string|{ data?: string, handle?: string, mime?: string }
---@param opts? { mime?: string }
function files.put(project, path, content, opts) end

---@param project string
---@param path string
function files.read(project, path) end

---@param project string
---@param prefix? string
function files.list(project, prefix) end

---@return table[]
function files.projects() end

---@param project string
---@param path string
function files.delete(project, path) end

--- Snapshot the project.
---@param project string
---@param opts? { message?: string, ref?: string }
function files.commit(project, opts) end

---@param project string
---@param ref string
function files.checkout(project, ref) end

files.scratch = {}

--- Per-session scratch files that expire (files.scratch_ttl).
---@param name string
---@param content string|{ data?: string, handle?: string, mime?: string }
---@param opts? { mime?: string }
function files.scratch.put(name, content, opts) end

---@param name string
function files.scratch.read(name) end

---@return table[]
function files.scratch.list() end

-- credential / runtime / json ----------------------------------------------------

credential = {}

--- Returns a raw secret named in the agent's credentials: list. Restricted to
--- engine-fired turns when runtime.credentials.engine_wide is "maintenance".
---@param name string
---@return any
function credential.get(name) end

runtime = {}

--- The deployment's triggers: as data.
---@return { ok: boolean, triggers: table[] }
function runtime.triggers() end

json = {}
---@param value any
---@return string
function json.encode(value) end
---@param text string
---@return any
function json.decode(text) end

-- support chunks (loaded into every session before the loop) -------------------

--- Trim messages to a token budget, keeping the system prompt.
---@param messages ChatMessage[]
---@param budget integer
---@return ChatMessage[]
function token_budget(messages, budget) end
