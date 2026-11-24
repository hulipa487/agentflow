// AgentFlow TypeScript Scripting API (v2: Goja Loops, External APIs & Safety Filters)

export interface AuthRef {
  service: string;
}

export interface HTTPRequest {
  method?: string;
  url: string;
  headers?: Record<string, string>;
  query_params?: Record<string, string>;
  body?: string;
  json?: unknown;
  /** Host-injected stored credential service name (never exposed to scripts). */
  auth_service?: string;
  auth?: AuthRef;
  timeout_ms?: number;
  response_encoding?: "text" | "base64";
}

export interface HTTPResponse {
  ok: boolean;
  status: number;
  headers: Record<string, string>;
  body: string;
  body_base64?: string;
  truncated?: boolean;
}

// ---- External API Extensions (TypeScript/Goja & Wasm/wazero) ---------------

export interface ExtLogger {
  debug(msg: string): void;
  info(msg: string): void;
  warn(msg: string): void;
  error(msg: string): void;
}

export interface ExtHTTPClient {
  request(req: HTTPRequest): HTTPResponse;
}

/**
 * Capability-scoped host object handed as the 2nd argument to External API actions.
 */
export interface ExtHost {
  readonly log: ExtLogger;
  readonly http: ExtHTTPClient;
}

export type ExtAction<TArgs = Record<string, unknown>, TResult = unknown> = (
  args: TArgs,
  host: ExtHost,
) => TResult;

// ---- Safety Script Filters ---------------------------------------------------

export interface SafetyIngressInput {
  message: string;
  source: "channel" | "agent" | "scheduler" | "system" | string;
  session_id: string;
  annotations: Record<string, string>;
}

export interface SafetyIngressResult {
  text?: string;
  drop?: boolean;
  skip?: boolean;
  refusal?: string;
  reason?: string;
  /** Override the model used for this turn (e.g. "strong-model"). */
  redirect?: string;
  model?: string;
  directive?: string;
  directives?: string[];
  annotations?: Record<string, string>;
}

export interface SafetyEgressInput {
  text: string;
  source: string;
  session_id: string;
  annotations: Record<string, string>;
}

export interface SafetyEgressResult {
  text?: string;
  drop?: boolean;
  fallback?: string;
  reason?: string;
  annotations?: Record<string, string>;
}

// ---- Loop & Host Capabilities ------------------------------------------------

export interface ChatMessage {
  role: "system" | "user" | "assistant" | "tool";
  content?: string;
  parts?: Array<Record<string, unknown>>;
  tool_calls?: Array<ToolCallSpec>;
  tool_call_id?: string;
  tool_result?: unknown;
  thinking_blocks?: Array<Record<string, unknown>>;
}

export interface ToolCallSpec {
  id?: string;
  name: string;
  args?: Record<string, unknown>;
  arguments?: string;
}

export interface ChatResponse {
  text: string;
  thinking?: string;
  thinking_blocks?: Array<Record<string, unknown>>;
  tool_calls?: ToolCallSpec[];
  finish_reason: string;
  usage?: {
    input: number;
    output: number;
    cached?: number;
    cache_write?: number;
    reasoning?: number;
  };
  media?: Array<Record<string, unknown>>;
}

export interface APIDescriptor {
  name: string;
  version?: string;
  description?: string;
  runtime: "script" | "wasm";
  actions: Record<
    string,
    {
      description?: string;
      expose_as_tool?: boolean;
      needs_confirm?: boolean;
      parameters?: Record<string, unknown>;
    }
  >;
}

export interface BatchAPICall {
  api: string;
  action: string;
  args?: Record<string, unknown>;
}

export interface BatchAPIResult {
  ok: boolean;
  data?: unknown;
  error?: string;
}

export interface ExtAPINamespace {
  list(): APIDescriptor[];
  call<TResult = unknown>(
    api: string,
    action: string,
    args?: Record<string, unknown>,
  ): TResult;
  batch(calls: BatchAPICall[]): BatchAPIResult[];
  readonly [pkg: string]: Record<string, (args?: Record<string, unknown>) => unknown>;
}

export interface AgentFlowGlobals {
  readonly api: ExtAPINamespace;
  readonly session: {
    inbox(): Record<string, unknown>;
    send(text: string, opts?: Record<string, unknown>): boolean;
    reply(text: string, opts?: Record<string, unknown>): boolean;
    send_delta(text: string): boolean;
    exit(): void;
  };
  readonly llm: {
    chat(messages: ChatMessage[], opts?: Record<string, unknown>): ChatResponse;
    stream(messages: ChatMessage[], opts?: Record<string, unknown>): Iterable<string>;
    embed(inputs: Array<string | Record<string, unknown>>, opts?: Record<string, unknown>): Record<string, unknown>;
    rerank(query: string, documents: string[], opts?: Record<string, unknown>): Record<string, unknown>;
  };
  readonly tools: {
    list(): Array<Record<string, unknown>>;
    run(name: string, args?: Record<string, unknown>): unknown;
  };
  readonly http: {
    request(req: HTTPRequest): HTTPResponse;
  };
}
