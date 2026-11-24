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
	directive := fmt.Sprintf("// af-prelude-version: %d\n", vm.PreludeVersion)
	return []Template{
		{
			Name:        "echo",
			Title:       "Echo",
			Description: "No model needed: replies with the text it received. The smallest runnable agent.",
			Loop: directive + `function loop(): void {
  while (true) {
    const msg = session.inbox();
    session.send("echo: " + (msg.text || ""));
  }
}
`,
		},
		{
			Name:        "llm-chat",
			Title:       "LLM chat",
			Description: "One model call per message with a system prompt. Needs a configured model.",
			Loop: directive + `function loop(): void {
  while (true) {
    const msg = session.inbox();
    const reply = llm.chat([
      { role: "system", content: "You are a helpful assistant." },
      { role: "user", content: msg.text || "" },
    ]);
    session.send(reply.text || "");
  }
}
`,
		},
		{
			Name:        "tool-loop",
			Title:       "Tool loop",
			Description: "Offers the agent's enabled tools to the model and runs the first requested one.",
			Loop: directive + `function loop(): void {
  while (true) {
    const msg = session.inbox();
    const reply = llm.chat([
      { role: "system", content: "Use the available tools when they help." },
      { role: "user", content: msg.text || "" },
    ], { tools: tools.list() });

    const calls = reply.tool_calls || [];
    if (calls.length > 0) {
      const call = calls[0];
      const result = tools.run(call.name, call.args || {});
      session.send("tool " + call.name + " -> " + JSON.stringify(result));
    } else {
      session.send(reply.text || "");
    }
  }
}
`,
		},
		{
			Name:        "stateless",
			Title:       "One-shot",
			Description: "Answers one message and exits. Useful for testing session lifecycle behaviour.",
			Loop: directive + `function loop(): void {
  const msg = session.inbox();
  session.send("handled: " + (msg.text || ""));
  session.exit();
}
`,
		},
		{
			Name:        "group-chat",
			Title:       "Group chat with memory",
			Description: "For QQ or Telegram groups: names each speaker, keeps the chat's recent exchanges (session.state capability) and recalls older ones (a memory profile with a dialogue store).",
			Loop: directive + `const CONTEXT_TURNS = 10;

interface ContextTurn {
  who: string;
  text: string;
  reply: string;
}

// Recent context lives in session.state, which needs the session.state
// capability; without it the bot still answers, just without context.
function loadContext(): ContextTurn[] {
  try {
    const ctx = session.state.get("context");
    if (Array.isArray(ctx)) return ctx as ContextTurn[];
  } catch (_) {}
  return [];
}

function saveContext(ctx: ContextTurn[]): void {
  while (ctx.length > CONTEXT_TURNS) ctx.shift();
  try {
    session.state.set("context", ctx);
  } catch (_) {}
}

function loop(): void {
  while (true) {
    const msg = session.inbox();
    const p = (msg.payload || {}) as Record<string, unknown>;
    const who = String(p.name || p.username || msg.from || "member");
    const chat = String(p.chat_id || msg.reply_to || "chat");
    const text = msg.text || "";
    if (/\S/.test(text)) {
      const info = agent.info();
      const ctx = loadContext();
      const messages: Array<{ role: string; content: string }> = [
        { role: "system", content: info.instructions || "You are a friendly member of this group chat. Each message starts with the speaker's name." },
      ];
      // Older exchanges from this chat that match the message (memory profile).
      const older: string[] = [];
      for (const hit of memory.recall({ store: "dialogue", kind: "text", text: text, k: 8 }) || []) {
        const v = hit && (hit as { value?: Record<string, unknown> }).value;
        if (v && typeof v === "object" && v.chat === chat && older.length < 4) {
          older.push("- " + String(v.who) + ": " + String(v.text) + "\n  you: " + String(v.reply));
        }
      }
      if (older.length > 0) {
        messages.push({ role: "system", content: "Earlier in this chat:\n" + older.join("\n") });
      }
      for (const e of ctx) {
        messages.push({ role: "user", content: e.who + ": " + e.text });
        messages.push({ role: "assistant", content: e.reply });
      }
      messages.push({ role: "user", content: who + ": " + text });
      const reply = llm.chat(token_budget(messages, info.history_budget || 6000));
      const answer = reply.text || "";
      session.send(answer);
      memory.write({ type: "turn", id: msg.id, chat: chat, who: who, text: text, reply: answer });
      ctx.push({ who: who, text: text, reply: answer });
      saveContext(ctx);
    }
  }
}
`,
		},
		{
			Name:        "rag",
			Title:       "Knowledge base (RAG)",
			Description: "Answers from passages recalled by meaning and cites them; /learn adds passages for editors. Needs a memory profile with a vector store named knowledge, embed_model and recall: plugin:semantic.",
			Loop: directive + `const TOP_K = 4;

function passagesFor(question: string): Array<{ text: string }> {
  const out: Array<{ text: string }> = [];
  for (const hit of memory.recall({ store: "knowledge", text: question, k: TOP_K }) || []) {
    const v = hit && (hit as { value?: { text?: string } }).value;
    if (v && typeof v === "object" && v.text) {
      out.push({ text: v.text });
    }
  }
  return out;
}

function loop(): void {
  while (true) {
    const msg = session.inbox();
    const text = msg.text || "";
    const learnMatch = /^\s*\/learn\s+([\s\S]+)$/.exec(text);
    if (learnMatch) {
      const cfg = agent.config() || {};
      const editors = (((cfg.extras || {}) as Record<string, unknown>).editors || []) as string[];
      if (editors.includes(msg.from)) {
        let n = 0;
        for (const rawPara of learnMatch[1].split(/\n\s*\n/)) {
          const para = rawPara.trim();
          if (para) {
            n++;
            memory.write({
              targets: [{ store: "knowledge", key: "doc:" + os.time() + ":" + n, value: { text: para } }],
            });
          }
        }
        session.send("Learned " + n + " passage(s).");
      } else {
        session.send("Only editors can teach me.");
      }
    } else if (/\S/.test(text)) {
      const passages = passagesFor(text);
      if (passages.length === 0) {
        session.send("I don't have anything on that in my knowledge base yet.");
      } else {
        const numbered = passages.map((p, i) => "[" + (i + 1) + "] " + p.text);
        const reply = llm.chat([
          { role: "system", content: "Answer only from the numbered passages and cite them like [1]. If they do not contain the answer, say you don't know." },
          { role: "system", content: "Passages:\n\n" + numbered.join("\n\n") },
          { role: "user", content: text },
        ]);
        session.send(reply.text || "");
      }
    }
  }
}
`,
		},
		{
			Name:        "single-turn-api",
			Title:       "Single-turn API",
			Description: "For the openai channel: answers each chat/completions request's last message with the agent's own instructions.",
			Loop: directive + `function loop(): void {
  while (true) {
    const msg = session.inbox();
    const info = agent.info();
    const reply = llm.chat([
      { role: "system", content: info.instructions || "You are a helpful assistant." },
      { role: "user", content: msg.text || "" },
    ]);
    session.reply({ text: reply.text || "", finish_reason: reply.finish_reason, usage: reply.usage });
  }
}
`,
		},
		{
			Name:        "openai-passthrough",
			Title:       "OpenAI pass-through",
			Description: "For the openai channel: forwards the client's messages, tools and sampling parameters to the agent's model unchanged.",
			Loop: directive + `function loop(): void {
  while (true) {
    session.inbox();
    const reply = llm.forward();
    session.reply(reply);
  }
}
`,
		},
	}
}
