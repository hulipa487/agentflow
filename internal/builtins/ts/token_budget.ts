// af-prelude-version: 2
// plugin:token_budget — the default prompt.budget handler.
//
// Loaded into every session state before the loop plugin (as the global
// `token_budget`). Approximates tokens as ceil(chars/4).
//
// Contract: token_budget(messages, budget) -> messages
//   - a history that fits comes back as it is (the exact array reference)
//   - the system message (messages[0] when role == "system") is always kept
//   - the most recent message (or whole newest tool exchange) is always kept
//   - oldest history is dropped from the front until it fits
//   - a tool exchange is kept or dropped as one atomic unit

interface BudgetThinkingBlock {
  thinking?: string;
  data?: string;
  [k: string]: unknown;
}

interface BudgetToolCall {
  name?: string;
  arguments?: unknown;
  args?: unknown;
  [k: string]: unknown;
}

interface BudgetMessage {
  role: string;
  content?: unknown;
  thinking_blocks?: BudgetThinkingBlock[];
  tool_calls?: BudgetToolCall[];
  [k: string]: unknown;
}

function token_budget(messages: BudgetMessage[], budget?: number): BudgetMessage[] {
  const limit = budget !== undefined && budget !== null ? budget : 6000;

  const estTokens = (content: unknown): number => {
    if (typeof content !== "string") return 0;
    return Math.ceil(content.length / 4);
  };

  const estChars = (v: unknown, depth: number): number => {
    if (typeof v === "string") return v.length;
    if (typeof v === "number" || typeof v === "boolean") return String(v).length;
    if (typeof v !== "object" || v === null || depth > 8) return 0;
    const obj = v as Record<string, unknown>;
    let n = 2;
    for (const k of Object.keys(obj)) {
      n += k.length + estChars(obj[k], depth + 1) + 4;
    }
    return n;
  };

  const estMsg = (m: BudgetMessage): number => {
    let n = estTokens(m.content) + 4;
    if (Array.isArray(m.thinking_blocks)) {
      for (const b of m.thinking_blocks) {
        if (b && typeof b === "object") {
          n += estTokens(b.thinking) + estTokens(b.data);
        }
      }
    }
    if (Array.isArray(m.tool_calls)) {
      for (const c of m.tool_calls) {
        if (c && typeof c === "object") {
          const args = c.arguments !== undefined && c.arguments !== null ? c.arguments : c.args;
          n += Math.ceil((estChars(c.name, 0) + estChars(args, 0)) / 4) + 4;
        }
      }
    }
    return n;
  };

  let totalCost = 0;
  for (let i = 0; i < messages.length; i++) {
    totalCost += estMsg(messages[i]);
  }
  if (totalCost <= limit) return messages;

  const kept: BudgetMessage[] = [];
  let tailStart = 0;
  let used = 0;
  if (messages.length > 0 && messages[0] && messages[0].role === "system") {
    kept.push(messages[0]);
    used = estMsg(messages[0]);
    tailStart = 1;
  }

  const asksForTools = (m: BudgetMessage): boolean =>
    m.role === "assistant" && Array.isArray(m.tool_calls) && m.tool_calls.length > 0;

  interface TrimUnit {
    messages: BudgetMessage[];
    cost: number;
  }
  const units: TrimUnit[] = [];
  let i = tailStart;
  while (i < messages.length) {
    const unit: TrimUnit = { messages: [messages[i]], cost: estMsg(messages[i]) };
    let j = i + 1;
    if (asksForTools(messages[i])) {
      while (j < messages.length && messages[j] && messages[j].role === "tool") {
        unit.messages.push(messages[j]);
        unit.cost += estMsg(messages[j]);
        j++;
      }
    }
    units.push(unit);
    i = j;
  }

  let first = units.length;
  for (let idx = units.length - 1; idx >= 0; idx--) {
    if (idx < units.length - 1 && used + units[idx].cost > limit) {
      break;
    }
    used += units[idx].cost;
    first = idx;
  }
  for (let idx = first; idx < units.length; idx++) {
    for (const m of units[idx].messages) {
      kept.push(m);
    }
  }
  return kept;
}
