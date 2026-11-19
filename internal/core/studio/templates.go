package studio

import (
	"fmt"

	"agentflow/internal/vm"
)

// Template is a starter loop the builder offers. Every template carries the
// current prelude version, so a loop created from one loads without an edit.
type Template struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Loop        string `json:"loop"`
}

// Templates returns the starter loops. They are deliberately small and use
// only documented prelude calls; the playground is where a new agent is
// exercised before it is persisted.
func Templates() []Template {
	directive := fmt.Sprintf("-- af-prelude-version: %d\n", vm.PreludeVersion)
	return []Template{
		{
			Name:        "echo",
			Title:       "Echo",
			Description: "No model needed: replies with the text it received. The smallest runnable agent.",
			Loop: directive + `function loop()
  while true do
    local msg = session.inbox()
    session.send("echo: " .. (msg.text or ""))
  end
end
`,
		},
		{
			Name:        "llm-chat",
			Title:       "LLM chat",
			Description: "One model call per message with a system prompt. Needs a configured model.",
			Loop: directive + `function loop()
  while true do
    local msg = session.inbox()
    local reply = llm.chat({
      { role = "system", content = "You are a helpful assistant." },
      { role = "user", content = msg.text or "" },
    })
    session.send(reply.text or "")
  end
end
`,
		},
		{
			Name:        "tool-loop",
			Title:       "Tool loop",
			Description: "Offers the agent's enabled tools to the model and runs the first requested one.",
			Loop: directive + `function loop()
  while true do
    local msg = session.inbox()
    local reply = llm.chat({
      { role = "system", content = "Use the available tools when they help." },
      { role = "user", content = msg.text or "" },
    }, { tools = tools.list() })

    local calls = reply.tool_calls or {}
    if #calls > 0 then
      local call = calls[1]
      local result = tools.run(call.name, call.args or {})
      session.send("tool " .. call.name .. " -> " .. tostring(result))
    else
      session.send(reply.text or "")
    end
  end
end
`,
		},
		{
			Name:        "stateless",
			Title:       "One-shot",
			Description: "Answers one message and exits. Useful for testing session lifecycle behaviour.",
			Loop: directive + `function loop()
  local msg = session.inbox()
  session.send("handled: " .. (msg.text or ""))
  session.exit()
end
`,
		},
		{
			Name:        "group-chat",
			Title:       "Group chat with memory",
			Description: "For QQ or Telegram groups: names each speaker, keeps the chat's recent exchanges (session.state capability) and recalls older ones (a memory profile with a dialogue store). Full scenario: examples/qq-group-chat.",
			Loop: directive + `local CONTEXT_TURNS = 10

-- Recent context lives in session.state, which needs the session.state
-- capability; without it the bot still answers, just without context.
local function load_context()
  local ok, ctx = pcall(session.state.get, "context")
  if ok and type(ctx) == "table" then return ctx end
  return {}
end

local function save_context(ctx)
  while #ctx > CONTEXT_TURNS do table.remove(ctx, 1) end
  pcall(session.state.set, "context", ctx)
end

function loop()
  while true do
    local msg = session.inbox()
    local p = msg.payload or {}
    local who = tostring(p.name or p.username or msg.from or "member")
    local chat = tostring(p.chat_id or msg.reply_to or "chat")
    local text = msg.text or ""
    if text:match("%S") then
      local info = agent.info()
      local ctx = load_context()
      local messages = {
        { role = "system", content = info.instructions or "You are a friendly member of this group chat. Each message starts with the speaker's name." },
      }
      -- Older exchanges from this chat that match the message (memory profile).
      local older = {}
      for _, hit in ipairs(memory.recall({ store = "dialogue", kind = "text", text = text, k = 8 }) or {}) do
        local v = hit.value
        if type(v) == "table" and v.chat == chat and #older < 4 then
          table.insert(older, "- " .. tostring(v.who) .. ": " .. tostring(v.text) .. "\n  you: " .. tostring(v.reply))
        end
      end
      if #older > 0 then
        table.insert(messages, { role = "system", content = "Earlier in this chat:\n" .. table.concat(older, "\n") })
      end
      for _, e in ipairs(ctx) do
        table.insert(messages, { role = "user", content = e.who .. ": " .. e.text })
        table.insert(messages, { role = "assistant", content = e.reply })
      end
      table.insert(messages, { role = "user", content = who .. ": " .. text })
      local reply = llm.chat(token_budget(messages, info.history_budget or 6000))
      local answer = reply.text or ""
      session.send(answer)
      memory.write({ type = "turn", id = msg.id, chat = chat, who = who, text = text, reply = answer })
      table.insert(ctx, { who = who, text = text, reply = answer })
      save_context(ctx)
    end
  end
end
`,
		},
		{
			Name:        "rag",
			Title:       "Knowledge base (RAG)",
			Description: "Answers from passages recalled by meaning and cites them; /learn adds passages for editors. Needs a memory profile with a vector store named knowledge, embed_model and recall: plugin:semantic. Full scenario: examples/telegram-rag.",
			Loop: directive + `local TOP_K = 4

local function passages_for(question)
  local out = {}
  for _, hit in ipairs(memory.recall({ store = "knowledge", text = question, k = TOP_K }) or {}) do
    if type(hit.value) == "table" and hit.value.text then
      table.insert(out, hit.value)
    end
  end
  return out
end

function loop()
  while true do
    local msg = session.inbox()
    local text = msg.text or ""
    local doc = text:match("^%s*/learn%s+(.+)$")
    if doc then
      -- Who may teach the bot is policy: here, senders listed in the agent's
      -- extras.editors.
      local editors = ((agent.config() or {}).extras or {}).editors or {}
      local allowed = false
      for _, who in ipairs(editors) do
        if who == msg.from then allowed = true end
      end
      if allowed then
        local n = 0
        for para in (doc .. "\n\n"):gmatch("(.-)\n%s*\n") do
          if para:match("%S") then
            n = n + 1
            memory.write({ targets = { { store = "knowledge", key = "doc:" .. os.time() .. ":" .. n, value = { text = para } } } })
          end
        end
        session.send("Learned " .. n .. " passage(s).")
      else
        session.send("Only editors can teach me.")
      end
    elseif text:match("%S") then
      local passages = passages_for(text)
      if #passages == 0 then
        session.send("I don't have anything on that in my knowledge base yet.")
      else
        local numbered = {}
        for i, p in ipairs(passages) do
          table.insert(numbered, "[" .. i .. "] " .. p.text)
        end
        local reply = llm.chat({
          { role = "system", content = "Answer only from the numbered passages and cite them like [1]. If they do not contain the answer, say you don't know." },
          { role = "system", content = "Passages:\n\n" .. table.concat(numbered, "\n\n") },
          { role = "user", content = text },
        })
        session.send(reply.text or "")
      end
    end
  end
end
`,
		},
		{
			Name:        "single-turn-api",
			Title:       "Single-turn API",
			Description: "For the openai channel: answers each chat/completions request's last message with the agent's own instructions. Full scenario: examples/chat-completions.",
			Loop: directive + `function loop()
  while true do
    local msg = session.inbox()
    local info = agent.info()
    local reply = llm.chat({
      { role = "system", content = info.instructions or "You are a helpful assistant." },
      { role = "user", content = msg.text or "" },
    })
    session.reply({ text = reply.text or "", finish_reason = reply.finish_reason, usage = reply.usage })
  end
end
`,
		},
	}
}
