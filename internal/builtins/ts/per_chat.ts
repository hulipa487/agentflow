// af-prelude-version: 2
// plugin:per_chat — the default gateway.route handler.
//
// Runs in the router's service state (singleton), not in a session. One
// session per (agent, channel, chat, identity): two users on the same channel
// never share history, one user's two chats don't either, and a chat acting
// in a group context is a different session from the same chat acting
// personally.

interface RouteMessage {
  id?: string;
  channel?: string;
  from?: string;
  text?: string;
  payload?: {
    chat_id?: string | number;
    api?: string;
    conversation_id?: string;
    request_id?: string;
    [k: string]: unknown;
  };
  [k: string]: unknown;
}

interface RouteInboxItem {
  agent: string;
  message: RouteMessage;
}

function loop(): void {
  const chatOf = (msg: RouteMessage): string => {
    if (msg.payload && msg.payload.chat_id !== undefined && msg.payload.chat_id !== null) {
      return String(msg.payload.chat_id);
    }
    return String(msg.from || "?");
  };

  const modeKey = (msg: RouteMessage): string =>
    `mode:${msg.channel || "?"}:${chatOf(msg)}`;

  const deliver = (item: RouteInboxItem, key: string, group?: string, kind?: string): void => {
    const req: Record<string, unknown> = {
      type: "deliver",
      agent: item.agent,
      key: key,
      message: item.message,
    };
    if (group) req.identity_group = group;
    if (kind) req.kind = kind;
    af.op(req);
  };

  while (true) {
    const item = session.inbox() as unknown as RouteInboxItem;
    const msg = item.message;
    const channel = msg.channel || "?";
    const base = `${channel}:${chatOf(msg)}`;

    // OpenAI-compatible traffic is stateless by default (ephemeral), or
    // keyed by conversation_id when opted in.
    if (msg.payload && msg.payload.api === "openai.chat") {
      const conv = msg.payload.conversation_id;
      if (conv !== undefined && conv !== null && conv !== "") {
        deliver(item, `conv:${channel}:${String(conv)}`);
      } else {
        deliver(item, `req:${String(msg.payload.request_id)}`, undefined, "ephemeral");
      }
    } else {
      const mk = modeKey(msg);
      let group: string | undefined;

      // Toggle commands, resolved before routing so the message itself lands
      // in the context it selects.
      if (msg.text) {
        const match = /^\/group\s+([A-Za-z0-9-]+)$/.exec(msg.text);
        if (match) {
          af.op({ type: "router.state.set", key: mk, value: match[1] });
          group = match[1];
        } else if (msg.text === "/personal") {
          af.op({ type: "router.state.delete", key: mk });
        }
      }

      if (group === undefined) {
        const mode = af.op({ type: "router.state.get", key: mk });
        if (mode !== undefined && mode !== null && mode !== "") {
          group = String(mode);
        }
      }

      if (group) {
        deliver(item, `${base}:${group}`, group);
      } else {
        deliver(item, base);
      }
    }
  }
}
