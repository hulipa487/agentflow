// af-prelude-version: 2
// QQ group bot: shared group memory, context-aware proactive participation,
// Doubao web search and guarded page fetch.
//
// Settings live in agent.config().extras (see agentflow.yaml):
//   respond_on_mention, respond_on_reply, respond_in_c2c, respond_in_guild,
//   bot_names, trigger_phrase, reply_on_relevant, cooldown_sec,
//   proactive_cooldown_sec, proactive_eval_cooldown_sec, proactive_min_chars,
//   proactive_wait_turns, proactive_hourly_max, store_all_messages,
//   retention_turns, max_tool_rounds, timezone_offset_hours, recall_k.
//
// The QQ channel must include the `group_all` scene; otherwise the platform
// never delivers ordinary (non-@) group messages and the bot cannot join a
// conversation proactively.

const DEFAULT_HISTORY_BUDGET = 6000;
const DEFAULT_RETENTION_TURNS = 24;
const MAX_TOOL_RESULT_CHARS = 12000;
const MAX_TEXT_CHARS = 700;
const MAX_REPLY_CHARS = 1000;
const OLDER_TURNS = 4;
const HOUR_MS = 60 * 60 * 1000;

interface QQTurn {
  id?: string;
  who: string;
  text: string;
  reply: string;
  reply_to?: string;
}

interface QQSettings {
  respond_on_mention: boolean;
  respond_on_reply: boolean;
  respond_in_c2c: boolean;
  respond_in_guild: boolean;
  bot_names: string;
  trigger_phrase: string;
  reply_on_relevant: boolean;
  cooldown_sec: number;
  proactive_cooldown_sec: number;
  proactive_eval_cooldown_sec: number;
  proactive_min_chars: number;
  proactive_wait_turns: number;
  proactive_hourly_max: number;
  store_all_messages: boolean;
  retention_turns: number;
  max_tool_rounds: number;
  timezone_offset_hours: number;
  recall_k: number;
}

interface QQDecision {
  mode: "direct" | "passive" | "skip";
  reason: string;
}

interface QQInbound {
  id?: string;
  channel?: string;
  from?: string;
  text?: string;
  reply_to?: string;
  payload?: Record<string, unknown>;
}

const PASSIVE_GATE =
  "【这一轮要不要开口】\n" +
  "这条群消息没有 @ 你。先判断此刻插话是否有价值，再决定。\n" +
  "可以开口：有人在问群里任何人都能回答的问题；对方在延续你参与过的话题；有人分享了作品或进度，你能指出一个具体细节；一个事实性问题需要搜索才能答准。\n" +
  "不要开口：两个人在互相点名对话（尤其是消息里 @ 了别人）；只是闲聊、玩梗、发表情；你上一轮刚说完；你只能说“确实/哈哈/同上”这类没有信息量的话；话题已经结束。\n" +
  "如果决定不开口，只输出 [NO_REPLY]，不要解释，不要加标点。\n" +
  "如果决定开口，直接给出你要发的那句话，1-3 句，像群里自然接话；不要说明你在判断、不要说“我插一句”。";

function readBool(extras: Record<string, unknown>, key: string, fallback: boolean): boolean {
  return typeof extras[key] === "boolean" ? (extras[key] as boolean) : fallback;
}

function readNum(
  extras: Record<string, unknown>,
  key: string,
  fallback: number,
  min: number
): number {
  const value = Number(extras[key]);
  return Number.isFinite(value) && value >= min ? value : fallback;
}

function readSettings(): QQSettings {
  const extras = ((agent.config() || {}).extras || {}) as Record<string, unknown>;
  return {
    respond_on_mention: readBool(extras, "respond_on_mention", true),
    respond_on_reply: readBool(extras, "respond_on_reply", true),
    respond_in_c2c: readBool(extras, "respond_in_c2c", true),
    respond_in_guild: readBool(extras, "respond_in_guild", false),
    bot_names: typeof extras.bot_names === "string" ? extras.bot_names.trim() : "",
    trigger_phrase: typeof extras.trigger_phrase === "string" ? extras.trigger_phrase.trim() : "",
    reply_on_relevant: readBool(extras, "reply_on_relevant", true),
    cooldown_sec: readNum(extras, "cooldown_sec", 0, 0),
    proactive_cooldown_sec: readNum(extras, "proactive_cooldown_sec", 90, 0),
    proactive_eval_cooldown_sec: readNum(extras, "proactive_eval_cooldown_sec", 15, 0),
    proactive_min_chars: readNum(extras, "proactive_min_chars", 4, 0),
    proactive_wait_turns: readNum(extras, "proactive_wait_turns", 1, 0),
    proactive_hourly_max: readNum(extras, "proactive_hourly_max", 6, 1),
    store_all_messages: readBool(extras, "store_all_messages", true),
    retention_turns: Math.max(1, Math.floor(readNum(extras, "retention_turns", DEFAULT_RETENTION_TURNS, 1))),
    max_tool_rounds: Math.max(1, Math.min(8, Math.floor(readNum(extras, "max_tool_rounds", 4, 1)))),
    timezone_offset_hours: Math.min(14, readNum(extras, "timezone_offset_hours", 8, -12)),
    // Text recall uses this as a result limit; semantic recall uses it as the
    // vector top-k before the chat filter, so a shared agent-wide store with
    // several chats wants a larger value.
    recall_k: Math.max(1, Math.min(200, Math.floor(readNum(extras, "recall_k", OLDER_TURNS * 6, 1)))),
  };
}

function trimText(value: string, max: number): string {
  const text = String(value || "");
  return text.length > max ? text.slice(0, max) + "…" : text;
}

function loadTurns(): QQTurn[] {
  try {
    const value = session.state.get("turns");
    if (Array.isArray(value)) return value as QQTurn[];
  } catch (_) {}
  return [];
}

function saveTurns(turns: QQTurn[], limit: number): void {
  while (turns.length > limit) turns.shift();
  for (let i = 0; i < turns.length; i++) {
    turns[i].text = trimText(turns[i].text, MAX_TEXT_CHARS);
    turns[i].reply = trimText(turns[i].reply, MAX_REPLY_CHARS);
  }
  try {
    session.state.set("turns", turns);
    return;
  } catch (_) {}
  // session.state caps one value at 64 KiB; drop the oldest turns and retry
  // rather than silently losing the whole group memory.
  while (turns.length > 8) {
    turns.shift();
    try {
      session.state.set("turns", turns);
      return;
    } catch (_) {}
  }
  try {
    session.state.set("turns", []);
  } catch (_) {}
}

function loadNumber(key: string): number {
  try {
    const value = Number(session.state.get(key));
    return Number.isFinite(value) ? value : 0;
  } catch (_) {}
  return 0;
}

function saveNumber(key: string, value: number): void {
  try {
    session.state.set(key, value);
  } catch (_) {}
}

function loadNumbers(key: string): number[] {
  const out: number[] = [];
  try {
    const value = session.state.get(key);
    if (Array.isArray(value)) {
      for (let i = 0; i < value.length; i++) {
        const n = Number(value[i]);
        if (Number.isFinite(n)) out.push(n);
      }
    }
  } catch (_) {}
  return out;
}

function saveNumbers(key: string, values: number[]): void {
  try {
    session.state.set(key, values);
  } catch (_) {}
}

function pruneStamps(stamps: number[], now: number): number[] {
  const cutoff = now - HOUR_MS;
  const out: number[] = [];
  for (let i = 0; i < stamps.length; i++) {
    if (stamps[i] >= cutoff) out.push(stamps[i]);
  }
  return out;
}

function parseNames(raw: string, botUsername: string): string[] {
  const parts = (raw || "").split(/[,，、;；\n]/);
  const out: string[] = [];
  const seen: Record<string, boolean> = {};
  for (let i = 0; i < parts.length; i++) {
    const name = parts[i].trim();
    if (name && !seen[name.toLowerCase()]) {
      seen[name.toLowerCase()] = true;
      out.push(name);
    }
  }
  const username = (botUsername || "").trim();
  if (username && !seen[username.toLowerCase()]) {
    out.push(username);
  }
  return out;
}

function matchesAnyName(text: string, names: string[]): boolean {
  if (!text || names.length === 0) return false;
  const lower = text.toLowerCase();
  for (let i = 0; i < names.length; i++) {
    if (lower.indexOf(names[i].toLowerCase()) >= 0) return true;
  }
  return false;
}

function matchesTrigger(text: string, phrase: string): boolean {
  if (!phrase) return false;
  return text.toLowerCase().indexOf(phrase.toLowerCase()) >= 0;
}

function turnsSinceBotReply(turns: QQTurn[]): number {
  let count = 0;
  for (let i = turns.length - 1; i >= 0; i--) {
    if (turns[i].reply) break;
    count++;
  }
  return count;
}

function decide(msg: QQInbound, cfg: QQSettings, turns: QQTurn[], stamps: number[], now: number): QQDecision {
  const p = (msg.payload || {}) as Record<string, unknown>;
  if (msg.channel === "studio") {
    return { mode: "direct", reason: "studio_test" };
  }
  const scene = String(p.scene || "group");

  if (scene === "c2c") {
    return cfg.respond_in_c2c
      ? { mode: "direct", reason: "c2c_chat" }
      : { mode: "skip", reason: "c2c_disabled" };
  }
  if (scene === "guild" || scene === "dm") {
    return cfg.respond_in_guild
      ? { mode: "direct", reason: scene }
      : { mode: "skip", reason: "guild_disabled" };
  }
  if (scene !== "group") {
    return { mode: "skip", reason: "unsupported_scene" };
  }

  const text = String(msg.text || "");
  const mentioned = p.mentioned === true;
  const replyToBot = p.is_reply_to_bot === true;
  const names = parseNames(cfg.bot_names, typeof p.bot_username === "string" ? p.bot_username : "");
  const named = matchesAnyName(text, names);
  const triggered = matchesTrigger(text, cfg.trigger_phrase);
  const direct =
    (cfg.respond_on_mention && mentioned) ||
    (cfg.respond_on_reply && replyToBot) ||
    named ||
    triggered;

  if (direct) {
    if (cfg.cooldown_sec > 0) {
      const elapsed = now - loadNumber("last_reply_ms");
      if (elapsed >= 0 && elapsed < cfg.cooldown_sec * 1000) {
        return { mode: "skip", reason: "cooldown" };
      }
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

  if (!cfg.reply_on_relevant) {
    return { mode: "skip", reason: "not_mentioned" };
  }
  if (p.mentions_others === true) {
    return { mode: "skip", reason: "addressed_to_someone_else" };
  }
  if (text.trim().length < cfg.proactive_min_chars) {
    return { mode: "skip", reason: "too_short" };
  }
  if (cfg.proactive_cooldown_sec > 0) {
    const last = loadNumber("last_proactive_ms");
    if (last > 0 && now - last < cfg.proactive_cooldown_sec * 1000) {
      return { mode: "skip", reason: "proactive_cooldown" };
    }
  }
  if (cfg.proactive_eval_cooldown_sec > 0) {
    const lastEval = loadNumber("last_eval_ms");
    if (lastEval > 0 && now - lastEval < cfg.proactive_eval_cooldown_sec * 1000) {
      return { mode: "skip", reason: "eval_cooldown" };
    }
  }
  const recentStamps = pruneStamps(stamps, now);
  if (cfg.proactive_hourly_max > 0 && recentStamps.length >= cfg.proactive_hourly_max) {
    return { mode: "skip", reason: "proactive_hourly_cap" };
  }
  if (turnsSinceBotReply(turns) < cfg.proactive_wait_turns) {
    return { mode: "skip", reason: "bot_spoke_recently" };
  }
  return { mode: "passive", reason: "relevance_check" };
}

function buildReplyToLabel(p: Record<string, unknown>): string {
  const replyText = typeof p.reply_to_text === "string" ? p.reply_to_text.trim() : "";
  if (!replyText) return "";
  let author = typeof p.reply_to_author === "string" ? p.reply_to_author.trim() : "";
  if (!author && p.is_reply_to_bot === true) {
    author = typeof p.bot_username === "string" && p.bot_username ? p.bot_username : "you";
  }
  const label = author ? author + ": " + replyText : replyText;
  return trimText(label, 300);
}

function formatTurn(turn: QQTurn): string {
  const line = turn.who + ": " + turn.text;
  return turn.reply_to ? "[引用 " + turn.reply_to + "]\n" + line : line;
}

function currentLine(p: Record<string, unknown>, who: string, text: string): string {
  const quoted = buildReplyToLabel(p);
  const line = who + ": " + text;
  return quoted ? "[引用 " + quoted + "]\n" + line : line;
}

function pushUserMessage(messages: ChatMessage[], content: string): void {
  if (!content) return;
  const last = messages.length - 1;
  if (last >= 0 && messages[last].role === "user") {
    messages[last].content = String(messages[last].content || "") + "\n" + content;
    return;
  }
  messages.push({ role: "user", content: content });
}

function recallOlder(chat: string, text: string, recent: QQTurn[], k: number): QQTurn[] {
  if (!text || text.trim().length < 2) return [];
  let hits: unknown[] = [];
  try {
    // plugin:recency uses kind=text (FTS5); plugin:semantic ignores kind and
    // embeds `text` for a vector query. The same call serves both profiles.
    hits = memory.recall({ store: "dialogue", kind: "text", text: text, k: k }) || [];
  } catch (_) {
    return [];
  }
  const seen: Record<string, boolean> = {};
  for (let i = 0; i < recent.length; i++) {
    seen[recent[i].text] = true;
  }
  const out: QQTurn[] = [];
  for (let i = 0; i < hits.length; i++) {
    const hit = hits[i] as Record<string, unknown>;
    const value = (hit && hit.value) as Record<string, unknown>;
    if (!value || typeof value !== "object") continue;
    if (String(value.chat || "") !== chat) continue;
    const oldText = String(value.text || "").trim();
    if (!oldText || seen[oldText]) continue;
    seen[oldText] = true;
    out.push({
      who: String(value.who || "member"),
      text: trimText(oldText, MAX_TEXT_CHARS),
      reply: trimText(String(value.reply || ""), MAX_REPLY_CHARS),
    });
    if (out.length >= OLDER_TURNS) break;
  }
  return out;
}

function buildSystem(
  info: { instructions?: string; history_budget?: number },
  scene: string,
  passive: boolean,
  older: QQTurn[],
  timezoneOffsetHours: number
): string {
  let sys = String(info.instructions || "").trim();
  if (!sys) {
    sys = "You are a concise, helpful member of a QQ group chat.";
  }
  const offset = Number(timezoneOffsetHours);
  const zone = offset >= 0 ? "UTC+" + offset : "UTC" + offset;
  const localNow = os.date("!%Y-%m-%d %H:%M", os.time() + Math.round(offset * 3600));
  sys += "\n\n【当前场景】" + (scene === "group" ? "QQ 群聊" : scene) +
    "；当前时间（" + zone + "）：" + localNow + "。";
  if (older.length > 0) {
    const lines: string[] = [];
    for (let i = 0; i < older.length; i++) {
      const item = older[i];
      let block = "- " + item.who + ": " + item.text;
      if (item.reply) block += "\n  你当时回: " + item.reply;
      lines.push(block);
    }
    sys += "\n\n【更早的群聊记忆（按相关性召回，可能不属于当前话题；不要把它当成刚刚发生的事）】\n" +
      lines.join("\n");
  }
  if (passive) {
    sys += "\n\n" + PASSIVE_GATE;
  } else {
    sys += "\n\n【这一轮】对方在直接找你（@、引用、叫名字或触发词）。直接给出你的回答，不要输出 [NO_REPLY]。";
  }
  return sys;
}

function buildMessages(
  msg: QQInbound,
  cfg: QQSettings,
  turns: QQTurn[],
  older: QQTurn[],
  passive: boolean,
  info: { instructions?: string; history_budget?: number }
): ChatMessage[] {
  const p = (msg.payload || {}) as Record<string, unknown>;
  const who = String(p.name || p.username || p.member_openid || p.user_openid || msg.from || "member");
  const scene = String(p.scene || "group");
  const messages: ChatMessage[] = [
    { role: "system", content: buildSystem(info, scene, passive, older, cfg.timezone_offset_hours) },
  ];
  const start = Math.max(0, turns.length - cfg.retention_turns);
  for (let i = start; i < turns.length; i++) {
    const turn = turns[i];
    pushUserMessage(messages, formatTurn(turn));
    if (turn.reply) {
      messages.push({ role: "assistant", content: turn.reply });
    }
  }
  pushUserMessage(messages, currentLine(p, who, String(msg.text || "")));
  return token_budget(messages, info.history_budget || DEFAULT_HISTORY_BUDGET);
}

function availableTools(): unknown[] {
  try {
    const list = tools.list();
    if (Array.isArray(list)) return list;
  } catch (_) {}
  return [];
}

function chatOnce(
  messages: ChatMessage[],
  model: string,
  toolList: unknown[],
  noTools: boolean
): ChatResponse | null {
  const opts: Record<string, unknown> = {};
  if (model) opts.model = model;
  if (!noTools && toolList.length > 0) {
    opts.tools = toolList;
    opts.tool_choice = "auto";
  }
  try {
    return llm.chat(messages, opts);
  } catch (_) {
    // One retry covers a transient provider/stream error without turning the
    // whole session loop over to the supervisor restart.
    try {
      return llm.chat(messages, opts);
    } catch (_) {
      return null;
    }
  }
}

function runTurn(
  messages: ChatMessage[],
  model: string,
  cfg: QQSettings
): { ok: boolean; text: string } {
  const toolList = availableTools();
  for (let round = 0; round < cfg.max_tool_rounds; round++) {
    const reply = chatOnce(messages, model, toolList, false);
    if (!reply) return { ok: false, text: "" };
    const calls = reply.tool_calls || [];
    const text = reply.text || "";
    if (calls.length === 0) {
      return { ok: true, text: text };
    }
    messages.push({
      role: "assistant",
      content: text,
      tool_calls: calls,
      thinking_blocks: reply.thinking_blocks,
    });
    for (let i = 0; i < calls.length; i++) {
      const call = calls[i];
      let args: Record<string, unknown> = {};
      if (call.args && typeof call.args === "object") {
        args = call.args;
      } else if (call.arguments) {
        try {
          const parsed = JSON.parse(call.arguments);
          if (parsed && typeof parsed === "object") args = parsed;
        } catch (_) {}
      }
      let body: string;
      try {
        body = json.encode(tools.run(call.name, args));
      } catch (err) {
        body = json.encode({ ok: false, error: String(err) });
      }
      if (body.length > MAX_TOOL_RESULT_CHARS) {
        body = body.slice(0, MAX_TOOL_RESULT_CHARS) + "\n…(tool result truncated)";
      }
      messages.push({
        role: "tool",
        tool_call_id: String(call.id || "call_" + round + "_" + i),
        content: body,
      });
    }
  }
  // Tool budget exhausted: make one final pass with tools disabled so the
  // model has to answer from what it already has.
  const final = chatOnce(messages, model, toolList, true);
  return final ? { ok: true, text: final.text || "" } : { ok: false, text: "" };
}

function isNoReply(text: string): boolean {
  const upper = text.trim().toUpperCase();
  return upper === "" || upper === "NO_REPLY" || upper.indexOf("[NO_REPLY]") >= 0;
}

function rememberTurn(chat: string, id: string, who: string, text: string, reply: string): void {
  try {
    memory.write({
      type: "turn",
      id: id,
      chat: chat,
      who: who,
      text: trimText(text, MAX_TEXT_CHARS),
      reply: trimText(reply, MAX_REPLY_CHARS),
      ts: os.time(),
    });
  } catch (_) {}
}

function turnId(chat: string, messageId: string): string {
  const raw = messageId
    ? String(messageId)
    : "local-" + os.time() + "-" + Math.floor(Math.random() * 1000000);
  return chat + ":" + raw;
}

function loop(): void {
  while (true) {
    const msg = session.inbox() as unknown as QQInbound;
    const cfg = readSettings();
    const p = (msg.payload || {}) as Record<string, unknown>;
    const scene = String(p.scene || "group");
    const who = String(p.name || p.username || p.member_openid || p.user_openid || msg.from || "member");
    const chat = String(p.chat_id || msg.from || msg.reply_to || "chat");
    const text = String(msg.text || "");
    const turns = loadTurns();
    const stamps = loadNumbers("proactive_ms");
    const now = Date.now();

    if (!/\S/.test(text)) {
      session.skip("empty_message");
      continue;
    }

    const decision = decide(msg, cfg, turns, stamps, now);

    if (decision.mode === "skip") {
      if (cfg.store_all_messages) {
        const id = turnId(chat, msg.id || "");
        const turn: QQTurn = { id: id, who: who, text: text, reply: "" };
        const quoted = buildReplyToLabel(p);
        if (quoted) turn.reply_to = quoted;
        turns.push(turn);
        saveTurns(turns, cfg.retention_turns);
        rememberTurn(chat, id, who, text, "");
      }
      session.skip(decision.reason);
      continue;
    }

    const passive = decision.mode === "passive";
    if (passive) saveNumber("last_eval_ms", now);
    const info = agent.info();
    const recentForRecall = turns.slice(Math.max(0, turns.length - cfg.retention_turns));
    const older = recallOlder(chat, text, recentForRecall, cfg.recall_k);
    const messages = buildMessages(msg, cfg, turns, older, passive, info);
    const result = runTurn(messages, info.model || "", cfg);
    const answer = String(result.text || "").trim();

    if (!result.ok || !answer || (passive && isNoReply(answer))) {
      const id = turnId(chat, msg.id || "");
      if (cfg.store_all_messages || cfg.reply_on_relevant) {
        const turn: QQTurn = { id: id, who: who, text: text, reply: "" };
        const quoted = buildReplyToLabel(p);
        if (quoted) turn.reply_to = quoted;
        turns.push(turn);
        saveTurns(turns, cfg.retention_turns);
        rememberTurn(chat, id, who, text, "");
      }
      if (!result.ok && !passive) {
        // A direct question should not be left hanging when the model/stream
        // fails twice. This fallback needs no model.
        try {
          session.send("我这边刚刚断了一下，没接上。你再发一次？");
        } catch (_) {}
      } else {
        session.skip(passive && result.ok ? "not_relevant" : "model_unavailable");
      }
      continue;
    }

    let sent = true;
    try {
      sent = session.send(answer) !== false;
    } catch (_) {
      sent = false;
    }
    if (sent) {
      saveNumber("last_reply_ms", now);
      if (passive) {
        saveNumbers("proactive_ms", pruneStamps(stamps.concat([now]), now));
      }
    }
    const id = turnId(chat, msg.id || "");
    const turn: QQTurn = { id: id, who: who, text: text, reply: sent ? answer : "" };
    const quoted = buildReplyToLabel(p);
    if (quoted) turn.reply_to = quoted;
    turns.push(turn);
    saveTurns(turns, cfg.retention_turns);
    rememberTurn(chat, id, who, text, sent ? answer : "");
  }
}
