package studio

import (
	"fmt"

	"agentflow/internal/vm"
)

// TemplateSetting describes one starter configuration field surfaced in
// Studio's Builder UI and stored inside the agent's extras map.
type TemplateSetting struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Description string   `json:"description,omitempty"`
	Type        string   `json:"type"` // bool | number | string | select
	Default     any      `json:"default"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Options     []string `json:"options,omitempty"`
}

// Template is a starter loop the builder offers. Every template carries the
// current prelude version, so a loop created from one loads without an edit.
type Template struct {
	Name         string            `json:"name"`
	Title        string            `json:"title"`
	Description  string            `json:"description"`
	Capabilities []string          `json:"capabilities,omitempty"`
	Memory       string            `json:"memory,omitempty"`
	Instructions string            `json:"instructions,omitempty"`
	Extras       map[string]any    `json:"extras,omitempty"`
	Settings     []TemplateSetting `json:"settings,omitempty"`
	Loop         string            `json:"loop"`
}

// Templates returns the starter loops. They are deliberately small and use
// only documented prelude calls; the playground is where a new agent is
// exercised before it is persisted.
func Templates() []Template {
	directive := fmt.Sprintf("// af-prelude-version: %d\n", vm.PreludeVersion)
	f64 := func(v float64) *float64 { return &v }
	return []Template{
		{
			Name:         "telegram-group-bot",
			Title:        "Telegram group bot",
			Description:  "Flagship group assistant: answers @-mentions, replies, or an optional trigger phrase, enforces cooldowns, keeps each group's context separate with explicit retention, and stays quiet otherwise.",
			Capabilities: []string{"llm.chat", "memory", "session.state"},
			Memory:       "conversational",
			Instructions: "You are a concise, helpful member of a Telegram group chat. Answer clearly, match the user's language, and admit when you are unsure instead of guessing.",
			Extras: map[string]any{
				"respond_on_mention": true,
				"respond_on_reply":   true,
				"respond_in_dm":      true,
				"trigger_phrase":     "",
				"cooldown_sec":       15,
				"store_all_messages": false,
				"retention_turns":    20,
			},
			Settings: []TemplateSetting{
				{
					Key:         "respond_on_mention",
					Label:       "Reply on @-mention",
					Description: "Answer when a group member @-mentions the bot or invokes a bot command.",
					Type:        "bool",
					Default:     true,
				},
				{
					Key:         "respond_on_reply",
					Label:       "Reply when replied to",
					Description: "Answer when a group member replies directly to one of the bot's messages.",
					Type:        "bool",
					Default:     true,
				},
				{
					Key:         "respond_in_dm",
					Label:       "Reply in direct messages",
					Description: "Answer private 1-on-1 messages sent directly to the bot.",
					Type:        "bool",
					Default:     true,
				},
				{
					Key:         "trigger_phrase",
					Label:       "Trigger phrase",
					Description: "Optional case-insensitive keyword or prefix (for example !bot or help:) that wakes the bot without an @-mention.",
					Type:        "string",
					Default:     "",
					Placeholder: "!bot",
				},
				{
					Key:         "cooldown_sec",
					Label:       "Group cooldown (seconds)",
					Description: "Minimum gap between bot replies in the same group. 0 disables cooldown.",
					Type:        "number",
					Default:     15,
					Min:         f64(0),
					Max:         f64(3600),
				},
				{
					Key:         "store_all_messages",
					Label:       "Remember ignored group messages",
					Description: "When off, only turns where the bot replied enter group context and memory.",
					Type:        "bool",
					Default:     false,
				},
				{
					Key:         "retention_turns",
					Label:       "Context window (turns)",
					Description: "How many recent turns per group stay in working context.",
					Type:        "number",
					Default:     20,
					Min:         f64(1),
					Max:         f64(200),
				},
			},
			Loop: directive + `interface GroupTurn {
  who: string;
  text: string;
  reply: string;
}

interface GroupSettings {
  respond_on_mention: boolean;
  respond_on_reply: boolean;
  respond_in_dm: boolean;
  trigger_phrase: string;
  cooldown_sec: number;
  store_all_messages: boolean;
  retention_turns: number;
}

function readBoolSetting(extras: Record<string, unknown>, key: string, fallback: boolean): boolean {
  return typeof extras[key] === "boolean" ? (extras[key] as boolean) : fallback;
}

function readNumSetting(extras: Record<string, unknown>, key: string, fallback: number, min: number): number {
  const val = Number(extras[key]);
  return Number.isFinite(val) && val >= min ? val : fallback;
}

function readSettings(): GroupSettings {
  const extras = ((agent.config() || {}).extras || {}) as Record<string, unknown>;
  return {
    respond_on_mention: readBoolSetting(extras, "respond_on_mention", true),
    respond_on_reply: readBoolSetting(extras, "respond_on_reply", true),
    respond_in_dm: readBoolSetting(extras, "respond_in_dm", true),
    trigger_phrase: typeof extras.trigger_phrase === "string" ? extras.trigger_phrase.trim() : "",
    cooldown_sec: readNumSetting(extras, "cooldown_sec", 15, 0),
    store_all_messages: readBoolSetting(extras, "store_all_messages", false),
    retention_turns: Math.max(1, Math.floor(readNumSetting(extras, "retention_turns", 20, 1))),
  };
}

function loadTurns(): GroupTurn[] {
  try {
    const v = session.state.get("turns");
    if (Array.isArray(v)) return v as GroupTurn[];
  } catch (_) {}
  return [];
}

function saveTurns(turns: GroupTurn[], limit: number): void {
  while (turns.length > limit) turns.shift();
  try {
    session.state.set("turns", turns);
  } catch (_) {}
}

function loadLastReplyMs(): number {
  try {
    const v = Number(session.state.get("last_reply_ms"));
    return Number.isFinite(v) ? v : 0;
  } catch (_) {}
  return 0;
}

function saveLastReplyMs(ts: number): void {
  try {
    session.state.set("last_reply_ms", ts);
  } catch (_) {}
}

function rememberTurn(chat: string, who: string, text: string, reply: string): void {
  try {
    memory.write({ type: "turn", chat: chat, who: who, text: text, reply: reply });
  } catch (_) {}
}

function matchesTrigger(text: string, phrase: string): boolean {
  if (!phrase) return false;
  return text.toLowerCase().indexOf(phrase.toLowerCase()) >= 0;
}

function shouldAnswer(
  msg: { text?: string; channel?: string; payload?: Record<string, unknown> },
  cfg: GroupSettings
): { ok: true; reason: string } | { ok: false; reason: string } {
  const p = (msg.payload || {}) as Record<string, unknown>;
  const chatType = String(p.chat_type || "");
  const isStudio = msg.channel === "studio";
  const isPrivate = isStudio || chatType === "private" || chatType === "";

  if (isPrivate) {
    if (!isStudio && !cfg.respond_in_dm) {
      return { ok: false, reason: "dm_disabled" };
    }
    return { ok: true, reason: isStudio ? "studio_test" : "private_chat" };
  }

  const text = msg.text || "";
  const mentioned = p.mentioned === true;
  const replyToBot = p.is_reply_to_bot === true;
  const triggered = matchesTrigger(text, cfg.trigger_phrase);
  const matched =
    (cfg.respond_on_mention && mentioned) ||
    (cfg.respond_on_reply && replyToBot) ||
    triggered;

  if (!matched) {
    return { ok: false, reason: "not_mentioned" };
  }
  if (cfg.cooldown_sec > 0) {
    const elapsed = Date.now() - loadLastReplyMs();
    if (elapsed >= 0 && elapsed < cfg.cooldown_sec * 1000) {
      return { ok: false, reason: "cooldown" };
    }
  }
  return { ok: true, reason: triggered ? "trigger_phrase" : replyToBot ? "reply_to_bot" : "mentioned" };
}

function buildMessages(
  msg: { text?: string; from?: string; payload?: Record<string, unknown> },
  cfg: GroupSettings,
  turns: GroupTurn[],
  instructions: string,
  historyBudget: number
): Array<{ role: string; content: string }> {
  const p = (msg.payload || {}) as Record<string, unknown>;
  const who = String(p.name || p.username || msg.from || "member");
  const chatTitle = String(p.chat_title || "");
  const replyToText = String(p.reply_to_text || "");
  const text = msg.text || "";

  let sys = instructions || "You are a concise, helpful member of a Telegram group chat.";
  if (chatTitle) {
    sys += "\nGroup: " + chatTitle;
  }

  // Task 2 customization hook: route support questions to a group-specific knowledge base here.
  // For example, match support keywords or p.chat_id and prepend memory.recall({ store: "knowledge", ... }) passages.

  const messages: Array<{ role: string; content: string }> = [{ role: "system", content: sys }];
  const start = Math.max(0, turns.length - cfg.retention_turns);
  for (let i = start; i < turns.length; i++) {
    const t = turns[i];
    messages.push({ role: "user", content: t.who + ": " + t.text });
    if (t.reply) {
      messages.push({ role: "assistant", content: t.reply });
    }
  }
  let userLine = who + ": " + text;
  if (replyToText) {
    userLine = "[Replying to: " + replyToText + "]\n" + userLine;
  }
  messages.push({ role: "user", content: userLine });
  return token_budget(messages, historyBudget || 6000);
}

function loop(): void {
  while (true) {
    const msg = session.inbox();
    const cfg = readSettings();
    const p = (msg.payload || {}) as Record<string, unknown>;
    const who = String(p.name || p.username || msg.from || "member");
    const chat = String(p.chat_id || msg.reply_to || "chat");
    const text = msg.text || "";
    const turns = loadTurns();

    if (!/\S/.test(text)) {
      session.skip("empty_message");
      continue;
    }

    const decision = shouldAnswer(msg, cfg);
    if (!decision.ok) {
      if (cfg.store_all_messages) {
        turns.push({ who: who, text: text, reply: "" });
        saveTurns(turns, cfg.retention_turns);
        rememberTurn(chat, who, text, "");
      }
      session.skip(decision.reason);
      continue;
    }

    const info = agent.info();
    const messages = buildMessages(msg, cfg, turns, info.instructions, info.history_budget);
    const reply = llm.chat(messages);
    const answer = reply.text || "";

    session.send(answer);
    saveLastReplyMs(Date.now());
    turns.push({ who: who, text: text, reply: answer });
    saveTurns(turns, cfg.retention_turns);
    rememberTurn(chat, who, text, answer);
  }
}
`,
		},
		{
			Name:         "qq-group-bot",
			Title:        "QQ group bot",
			Description:  "Flagship QQ group assistant: retains recent group context, answers @-mentions, quote-replies, bot names/aliases, or contextually relevant group messages ([NO_REPLY] otherwise), and enforces cooldowns.",
			Capabilities: []string{"llm.chat", "memory", "session.state"},
			Memory:       "conversational",
			Instructions: "You are a concise, helpful member of a QQ group chat. Match the user's language, address speakers clearly, and admit when you are unsure instead of guessing.",
			Extras: map[string]any{
				"respond_on_mention": true,
				"respond_on_reply":   true,
				"respond_in_c2c":     true,
				"respond_in_guild":   false,
				"bot_names":          "",
				"trigger_phrase":     "",
				"reply_on_relevant":  true,
				"cooldown_sec":       15,
				"store_all_messages": true,
				"retention_turns":    20,
			},
			Settings: []TemplateSetting{
				{
					Key:         "respond_on_mention",
					Label:       "Reply on @-mention",
					Description: "Answer when a QQ group member @-mentions the bot.",
					Type:        "bool",
					Default:     true,
				},
				{
					Key:         "respond_on_reply",
					Label:       "Reply when quoted",
					Description: "Answer when a group member quotes or replies to one of the bot's messages without @-tagging it.",
					Type:        "bool",
					Default:     true,
				},
				{
					Key:         "respond_in_c2c",
					Label:       "Reply in C2C private chats",
					Description: "Answer 1-on-1 private QQ messages sent directly to the bot.",
					Type:        "bool",
					Default:     true,
				},
				{
					Key:         "respond_in_guild",
					Label:       "Reply in QQ Guild / channel DMs",
					Description: "Answer @-mentions in QQ Guild channels and guild DMs.",
					Type:        "bool",
					Default:     false,
				},
				{
					Key:         "bot_names",
					Label:       "Bot names / aliases",
					Description: "Comma-separated names or nicknames that wake the bot in plain text without @ (used together with its QQ username when group_all is enabled).",
					Type:        "string",
					Default:     "",
					Placeholder: "Misaka, Astra",
				},
				{
					Key:         "trigger_phrase",
					Label:       "Trigger phrase",
					Description: "Optional case-insensitive keyword or prefix (for example !bot) that wakes the bot without an @-mention when group_all scene is granted.",
					Type:        "string",
					Default:     "",
					Placeholder: "!bot",
				},
				{
					Key:         "reply_on_relevant",
					Label:       "Reply to relevant group messages",
					Description: "When group_all is enabled, let the bot evaluate non-@ messages against recent group context and stay silent ([NO_REPLY]) when not relevant.",
					Type:        "bool",
					Default:     true,
				},
				{
					Key:         "cooldown_sec",
					Label:       "Group cooldown (seconds)",
					Description: "Minimum gap between bot replies in the same QQ group. 0 disables cooldown.",
					Type:        "number",
					Default:     15,
					Min:         f64(0),
					Max:         f64(3600),
				},
				{
					Key:         "store_all_messages",
					Label:       "Remember ignored group messages",
					Description: "When using group_all, retain ordinary group messages in working context even when the bot stays quiet.",
					Type:        "bool",
					Default:     true,
				},
				{
					Key:         "retention_turns",
					Label:       "Context window (turns)",
					Description: "How many recent turns per QQ group stay in working context.",
					Type:        "number",
					Default:     20,
					Min:         f64(1),
					Max:         f64(200),
				},
			},
			Loop: directive + `interface QQGroupTurn {
  who: string;
  text: string;
  reply: string;
  reply_to?: string;
}

interface QQGroupSettings {
  respond_on_mention: boolean;
  respond_on_reply: boolean;
  respond_in_c2c: boolean;
  respond_in_guild: boolean;
  bot_names: string;
  trigger_phrase: string;
  reply_on_relevant: boolean;
  cooldown_sec: number;
  store_all_messages: boolean;
  retention_turns: number;
}

function readQQBoolSetting(extras: Record<string, unknown>, key: string, fallback: boolean): boolean {
  return typeof extras[key] === "boolean" ? (extras[key] as boolean) : fallback;
}

function readQQNumSetting(extras: Record<string, unknown>, key: string, fallback: number, min: number): number {
  const val = Number(extras[key]);
  return Number.isFinite(val) && val >= min ? val : fallback;
}

function readQQSettings(): QQGroupSettings {
  const extras = ((agent.config() || {}).extras || {}) as Record<string, unknown>;
  return {
    respond_on_mention: readQQBoolSetting(extras, "respond_on_mention", true),
    respond_on_reply: readQQBoolSetting(extras, "respond_on_reply", true),
    respond_in_c2c: readQQBoolSetting(extras, "respond_in_c2c", true),
    respond_in_guild: readQQBoolSetting(extras, "respond_in_guild", false),
    bot_names: typeof extras.bot_names === "string" ? extras.bot_names.trim() : "",
    trigger_phrase: typeof extras.trigger_phrase === "string" ? extras.trigger_phrase.trim() : "",
    reply_on_relevant: readQQBoolSetting(extras, "reply_on_relevant", true),
    cooldown_sec: readQQNumSetting(extras, "cooldown_sec", 15, 0),
    store_all_messages: readQQBoolSetting(extras, "store_all_messages", true),
    retention_turns: Math.max(1, Math.floor(readQQNumSetting(extras, "retention_turns", 20, 1))),
  };
}

function loadQQTurns(): QQGroupTurn[] {
  try {
    const v = session.state.get("turns");
    if (Array.isArray(v)) return v as QQGroupTurn[];
  } catch (_) {}
  return [];
}

function saveQQTurns(turns: QQGroupTurn[], limit: number): void {
  while (turns.length > limit) turns.shift();
  try {
    session.state.set("turns", turns);
  } catch (_) {}
}

function loadQQLastReplyMs(): number {
  try {
    const v = Number(session.state.get("last_reply_ms"));
    return Number.isFinite(v) ? v : 0;
  } catch (_) {}
  return 0;
}

function saveQQLastReplyMs(ts: number): void {
  try {
    session.state.set("last_reply_ms", ts);
  } catch (_) {}
}

function rememberQQTurn(chat: string, who: string, text: string, reply: string): void {
  try {
    memory.write({ type: "turn", chat: chat, who: who, text: text, reply: reply });
  } catch (_) {}
}

function parseQQNames(raw: string, botUsername: string): string[] {
  const parts = (raw || "").split(/[,，、;；\n]/);
  const out: string[] = [];
  const seen: Record<string, boolean> = {};
  for (let i = 0; i < parts.length; i++) {
    const s = parts[i].trim();
    if (s && !seen[s.toLowerCase()]) {
      seen[s.toLowerCase()] = true;
      out.push(s);
    }
  }
  const uname = (botUsername || "").trim();
  if (uname && !seen[uname.toLowerCase()]) {
    out.push(uname);
  }
  return out;
}

function matchesAnyQQName(text: string, names: string[]): boolean {
  if (!text || names.length === 0) return false;
  const lower = text.toLowerCase();
  for (let i = 0; i < names.length; i++) {
    if (lower.indexOf(names[i].toLowerCase()) >= 0) return true;
  }
  return false;
}

function matchesQQTrigger(text: string, phrase: string): boolean {
  if (!phrase) return false;
  return text.toLowerCase().indexOf(phrase.toLowerCase()) >= 0;
}

function shouldAnswerQQ(
  msg: { text?: string; channel?: string; payload?: Record<string, unknown> },
  cfg: QQGroupSettings
): { mode: "direct" | "passive" | "skip"; reason: string } {
  if (msg.channel === "studio") {
    return { mode: "direct", reason: "studio_test" };
  }
  const p = (msg.payload || {}) as Record<string, unknown>;
  const scene = String(p.scene || "group");

  if (scene === "c2c") {
    if (!cfg.respond_in_c2c) {
      return { mode: "skip", reason: "c2c_disabled" };
    }
    return { mode: "direct", reason: "c2c_chat" };
  }
  if (scene === "guild" || scene === "dm") {
    if (!cfg.respond_in_guild) {
      return { mode: "skip", reason: "guild_disabled" };
    }
    return { mode: "direct", reason: scene };
  }

  const text = msg.text || "";
  const mentioned = p.mentioned === true;
  const replyToBot = p.is_reply_to_bot === true;
  const names = parseQQNames(cfg.bot_names, typeof p.bot_username === "string" ? p.bot_username : "");
  const named = matchesAnyQQName(text, names);
  const triggered = matchesQQTrigger(text, cfg.trigger_phrase);
  const direct =
    (cfg.respond_on_mention && mentioned) ||
    (cfg.respond_on_reply && replyToBot) ||
    named ||
    triggered;

  if (!direct) {
    if (!cfg.reply_on_relevant) {
      return { mode: "skip", reason: "not_mentioned" };
    }
    if (p.mentions_others === true) {
      return { mode: "skip", reason: "mentions_others" };
    }
  }

  if (cfg.cooldown_sec > 0) {
    const elapsed = Date.now() - loadQQLastReplyMs();
    if (elapsed >= 0 && elapsed < cfg.cooldown_sec * 1000) {
      return { mode: "skip", reason: "cooldown" };
    }
  }

  if (!direct) {
    return { mode: "passive", reason: "relevance_check" };
  }
  const reason = mentioned
    ? "mentioned"
    : replyToBot
    ? "reply_to_bot"
    : named
    ? "bot_name"
    : "trigger_phrase";
  return { mode: "direct", reason: reason };
}

function buildReplyToLabel(p: Record<string, unknown>): string {
  const rText = typeof p.reply_to_text === "string" ? p.reply_to_text.trim() : "";
  if (!rText) return "";
  let rAuthor = typeof p.reply_to_author === "string" ? p.reply_to_author.trim() : "";
  if (!rAuthor && p.is_reply_to_bot === true) {
    rAuthor = typeof p.bot_username === "string" && p.bot_username ? p.bot_username : "you";
  }
  return rAuthor ? rAuthor + ": " + rText : rText;
}

function formatQQUserLine(who: string, text: string, replyTo?: string): string {
  const line = who + ": " + text;
  return replyTo ? "[Replying to " + replyTo + "]\n" + line : line;
}

function pushQQUserMessage(messages: Array<{ role: string; content: string }>, content: string): void {
  if (!content) return;
  const n = messages.length;
  if (n > 0 && messages[n - 1].role === "user") {
    messages[n - 1].content += "\n" + content;
    return;
  }
  messages.push({ role: "user", content: content });
}

function buildQQMessages(
  msg: { text?: string; from?: string; payload?: Record<string, unknown> },
  cfg: QQGroupSettings,
  turns: QQGroupTurn[],
  instructions: string,
  historyBudget: number,
  passiveCheck: boolean
): Array<{ role: string; content: string }> {
  const p = (msg.payload || {}) as Record<string, unknown>;
  const who = String(p.name || p.username || p.member_openid || p.user_openid || msg.from || "member");
  const scene = String(p.scene || "group");
  const text = msg.text || "";

  let sys = instructions || "You are a concise, helpful member of a QQ group chat.";
  if (scene === "group" || scene === "guild") {
    sys += "\nConversation scene: " + scene + ".";
  }
  if (passiveCheck) {
    sys +=
      "\nPassive group observation mode: the latest message did not explicitly @-mention or quote you. " +
      "Read the recent group messages above to judge whether the speaker is addressing you, continuing your topic, or asking something in the group where a short reply from you is helpful and welcome. " +
      "If it is casual chatter between other members or you do not need to interject, reply with ONLY the exact token [NO_REPLY].";
  }

  // Task 2 customization hook: route support questions to a group-specific knowledge base here.
  // For example, match support keywords or p.chat_id and prepend memory.recall({ store: "knowledge", ... }) passages.

  const messages: Array<{ role: string; content: string }> = [{ role: "system", content: sys }];
  const start = Math.max(0, turns.length - cfg.retention_turns);
  for (let i = start; i < turns.length; i++) {
    const t = turns[i];
    pushQQUserMessage(messages, formatQQUserLine(t.who, t.text, t.reply_to));
    if (t.reply) {
      messages.push({ role: "assistant", content: t.reply });
    }
  }
  const replyTo = buildReplyToLabel(p);
  pushQQUserMessage(messages, formatQQUserLine(who, text, replyTo));
  return token_budget(messages, historyBudget || 6000);
}

function isQQNoReply(text: string): boolean {
  const trimmed = text.trim();
  return (
    trimmed === "" ||
    trimmed.indexOf("[NO_REPLY]") >= 0 ||
    /^NO_REPLY[.。!！]?$/i.test(trimmed)
  );
}

function loop(): void {
  while (true) {
    const msg = session.inbox();
    const cfg = readQQSettings();
    const p = (msg.payload || {}) as Record<string, unknown>;
    const who = String(p.name || p.username || p.member_openid || p.user_openid || msg.from || "member");
    const chat = String(p.chat_id || msg.reply_to || "chat");
    const text = msg.text || "";
    const replyTo = buildReplyToLabel(p);
    const turns = loadQQTurns();

    if (!/\S/.test(text)) {
      session.skip("empty_message");
      continue;
    }

    const decision = shouldAnswerQQ(msg, cfg);
    if (decision.mode === "skip") {
      if (cfg.store_all_messages) {
        const turn: QQGroupTurn = { who: who, text: text, reply: "" };
        if (replyTo) turn.reply_to = replyTo;
        turns.push(turn);
        saveQQTurns(turns, cfg.retention_turns);
        rememberQQTurn(chat, who, text, "");
      }
      session.skip(decision.reason);
      continue;
    }

    const passiveCheck = decision.mode === "passive";
    const info = agent.info();
    const messages = buildQQMessages(msg, cfg, turns, info.instructions, info.history_budget, passiveCheck);
    const reply = llm.chat(messages);
    const answer = reply.text || "";

    if (passiveCheck && isQQNoReply(answer)) {
      if (cfg.store_all_messages || cfg.reply_on_relevant) {
        const turn: QQGroupTurn = { who: who, text: text, reply: "" };
        if (replyTo) turn.reply_to = replyTo;
        turns.push(turn);
        saveQQTurns(turns, cfg.retention_turns);
        rememberQQTurn(chat, who, text, "");
      }
      session.skip("not_relevant");
      continue;
    }

    session.send(answer);
    saveQQLastReplyMs(Date.now());
    const turn: QQGroupTurn = { who: who, text: text, reply: answer };
    if (replyTo) turn.reply_to = replyTo;
    turns.push(turn);
    saveQQTurns(turns, cfg.retention_turns);
    rememberQQTurn(chat, who, text, answer);
  }
}
`,
		},
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
