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
	}
}
