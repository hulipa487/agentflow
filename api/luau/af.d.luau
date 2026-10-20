---@meta
-- Editor stubs for the AgentFlow Lua API. Never executed. The prelude version
-- in internal/vm is the compatibility contract; this file mirrors its surface
-- for LuaLS completion and hover.

---@class MediaPart
---@field type string "text" | "image" | "audio" | "video" | "file"
---@field text? string
---@field mime? string
---@field data? string
---@field url? string
---@field handle? string
---@field name? string

---@class Message
---@field id string
---@field type string
---@field from string
---@field text? string
---@field channel? string
---@field reply_to? string
---@field attachments? MediaPart[]
---@field payload? table
---@field provenance? { kind?: string, principal?: string, user_uuid?: string, membership_uuid?: string }

---@class ToolCall
---@field id? string
---@field name string
---@field args? table

---@class LLMReply
---@field text? string
---@field tool_calls? ToolCall[]
---@field thinking_blocks? table[]

---@class AgentInfo
---@field name string
---@field model string
---@field instructions? string
---@field skills? string[]
---@field capabilities? string[]
---@field prompts? table<string, string>
---@field extras? table

---@class SessionApi
session = {}

---@return Message
function session.inbox() end
---@param text string
---@param opts? { attachments?: MediaPart[] }
function session.send(text, opts) end
---@param channel string
---@param reply_to string
---@param text string
---@param opts? { attachments?: MediaPart[] }
function session.push(channel, reply_to, text, opts) end
---@param user_uuid string
---@param text string
function session.push_user(user_uuid, text) end
function session.exit() end

---@class SessionStateApi
session.state = {}
---@param key string
---@return any
function session.state.get(key) end
---@param key string
---@param value any
function session.state.set(key, value) end
---@param key string
function session.state.delete(key) end
---@return string[]
function session.state.list() end

---@class LLMApi
llm = {}
---@param messages table[]
---@param opts? { model?: string, tools?: table[], tool_choice?: string, temperature?: number, max_tokens?: number, thinking?: string }
---@return LLMReply
function llm.chat(messages, opts) end
---@param inputs MediaPart[]|string[]
---@param opts? table
---@return number[][]
function llm.embed(inputs, opts) end
---@param query string
---@param documents string[]
---@param opts? table
---@return table
function llm.rerank(query, documents, opts) end

---@class ToolsApi
tools = {}
---@return table[]
function tools.list() end
---@param name string
---@param args? table
---@return any
function tools.run(name, args) end
---@param spec { name: string, description?: string, params?: table, handler: fun(args: table): any }
function tool.def(spec) end

---@class AgentApi
agent = {}
---@return AgentInfo
function agent.info() end
---@return table
function agent.config() end
function agent.send(address, payload) end
function agent.request(address, payload, timeout) end
function agent.reply(request_id, payload) end
function agent.list() end
function agent.spawn(profile, spec) end

---@class StoreApi
store = {}
function store.put(...) end
function store.get(...) end
function store.query(...) end
function store.delete(...) end
function store.scopes(...) end

---@class MemoryApi
memory = {}
function memory.write(...) end
function memory.recall(...) end

---@class HttpApi
http = {}
---@param opts { url: string, method?: string, headers?: table<string,string>, body?: string, json?: table, query?: table<string,string>, auth?: { service: string }, timeout?: number, save_to?: string }
---@return table
function http.request(opts) end
function http.get(url, opts) end
function http.post(url, opts) end

---@class ShellApi
shell = {}
function shell.spawn(opts) end
function shell.exec(handle, command) end
function shell.write(handle, path, content) end
function shell.destroy(handle) end

---@class FilesApi
files = {}
function files.put(...) end
function files.read(...) end
function files.list(...) end
function files.delete(...) end
function files.commit(...) end
function files.checkout(...) end

---@class SchedulerApi
scheduler = {}
function scheduler.every(seconds) end
function scheduler.after(seconds) end
function scheduler.cron(expr) end
function scheduler.cancel(timer_id) end

---@class LogApi
log = {}
function log.debug(msg, fields) end
function log.info(msg, fields) end
function log.warn(msg, fields) end
function log.error(msg, fields) end

---@class TimeApi
time = {}
function time.sleep(seconds) end

---@class JsonApi
json = {}
function json.encode(value) end
function json.decode(text) end

---@class CredentialApi
credential = {}
function credential.get(name) end

---@class RuntimeApi
runtime = {}
function runtime.triggers() end
