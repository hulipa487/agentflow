# Master plan: OpenAI-compatible channel and session lifecycle

Status: approved direction, not started. Written 2026-11-17 against `main` at
`267c1c7` (agentflow) and `0912419` (agentflow-studio).

You are picking up implementation work. Read sections 1 to 4 fully before
writing code. Section 5 is the work, in order. Section 6 is the exact contracts
(HTTP, Lua, config). Section 7 lists what to ask the owner about.

---

## 1. Goal

Two outcomes, delivered together because the first needs the second:

1. **Plug-and-play OpenAI compatibility.** Clients that speak the OpenAI Chat
   Completions API work against AgentFlow with only a base URL and an API key:
   agent frameworks and bots (OpenClaw, AstrBot), coding agents (Roo Code,
   Cline), chat frontends (Open WebUI, LibreChat) and plain SDK code.
2. **A session model that fits stateless traffic.** Sessions keep their job of
   naming a conversation and ordering its turns, but their Lua memory becomes
   disposable, and a request that needs no conversation gets a session that
   costs almost nothing.

### The design in one paragraph

The OpenAI endpoint is a **channel**: a stateless protocol adapter. It
authenticates, parses, correlates the reply and speaks SSE. It makes no
decision about meaning. **Lua decides**: the route handler picks the session
(one per request, or one per conversation), and the loop decides what the
request is (forward it to a model, enrich it, answer it as an agent). The
default loop is a faithful pass-through, so a client that is itself an agent
(its own system prompt, its own tools) gets exactly a model. Statefulness, when
wanted, lives in Lua code that uses the stores.

### Non-goals

- No raw proxy that bypasses the engine. Every model call goes through the
  metered `llm.*` path (keys, quotas, usage ledger, safety).
- No Anthropic Messages endpoint, no Responses API, no `/v1/embeddings`, no
  legacy `/v1/completions` in this plan.
- No change to the regional ownership model (leases, inbox, fencing). A
  per-turn-lock redesign was considered and deferred.
- No prelude version bump. Every Lua API change here is additive. The owner
  has decided additive changes keep `PreludeVersion` at 1. Do not change it.

---

## 2. Ground rules for this repo

Build and test:

- `make` builds the vendored Luau static library and the binary. `go build
  ./...`, `go vet ./...` and `go test ./...` must pass before every commit.
- The Luau VM is cgo. You cannot cross-vet for Windows or macOS locally; CI
  does that. CI runs ubuntu, macos and windows.
- **Never build a unique id from the clock alone.** Windows reads time in
  100 ns steps and macOS in microseconds; that already broke CI once
  (`internal/core/supervisor/scheduler_adapter.go`, see `timerFireSeq`). Use a
  counter or a random id.
- Postgres integration tests run only with `AGENTFLOW_TEST_POSTGRES` set.
  Write them, gate them the same way, and say in your report if they did not
  run.

Git:

- Work on a branch off `main`. Commit only when the owner asks. Never push
  unless asked.
- The agentflow working tree has CRLF line endings in many files while the
  index is LF, so `git status` shows hundreds of files as modified. That is
  noise, not your changes. Use `git diff --ignore-cr-at-eol --stat` to see real
  changes. Commit LF content only: for each changed file, `tr -d '\r' < f |
  git hash-object -w --stdin`, then `git update-index --add --cacheinfo
  <mode>,<hash>,<f>`. Do not `git add -A`.
- Leave `third_party/luau` (a dirty submodule) out of every commit.
- Author identity: `git -c user.name=oscar -c user.email=oscar@local commit`.

Docs and contracts:

- User documentation lives in the **agentflow-studio** repo under `docs/`, and
  is embedded in the Studio app. A new page must also be added to the manifest
  in `agentflow-studio/internal/docs/docs.go` or the viewer cannot open it (a
  test, `TestEveryDocIsReachable`, enforces this).
- The Lua type stub is `agentflow/api/luau/af.d.luau`. After editing it run
  `make docs-sync` in agentflow-studio, which copies it into Studio.
- The op catalog Studio's editor uses comes from
  `internal/core/studio/catalog.go`. New Lua ops go there too.
- There is one HTTP listener (`gateway.listen`). The admin plane owns
  `/admin/`, `/healthz`, `/readyz`, `/metrics`; the engine owns `/health` and
  `/v1/users/`. See `reservedChannelPath` in `internal/config/channels.go`.

Style: the codebase explains *why* in comments, at length. Match it. Every new
rule that refuses a config must have an error message that says what to do.

---

## 3. How things work today (read these first)

| Topic | Where | What to know |
|---|---|---|
| Channel drivers | `internal/drivers/webhook/webhook.go` | The model to copy. `handle` parks the HTTP request on a `pending` map keyed by a request id, `submit` hands a `router.Inbound` to the router, `Deliver` resolves the waiting request. First reply wins. |
| Driver interface | `internal/core/gateway/gateway.go` | `Driver{Name(); Deliver(replyTo, text, attachments)}`. Text and media only. `Registry.Send` is what a session's egress calls. |
| Routing | `internal/core/router/router.go`, `internal/builtins/lua/per_chat.lua` | The router is itself a Lua loop in one service state. It receives `{message, agent}` as JSON and answers with a `deliver {agent, key, message}` op. **The whole message crosses the Lua bridge twice here**, which matters for large requests. |
| Delivery | `internal/core/supervisor/supervisor.go` | `Deliver` goes through the hub when one is installed, else `DeliverLocal`. `ensureSession` is the one place actors are created; the per-tenant session cap is applied there. `onActorExit` retires a session id (`retired` map) unless the lease was lost. |
| Session actor | `internal/core/session/actor.go` | `Run` restarts `runOnce` on crash. The `inbox` op (in `dispatchInline`) is where a turn ends: it emits a `turn.end` trace event, then blocks in `waitInbox`. `egress` runs the text safety filter, calls `gw.Send`, journals. |
| Message shape | `session.Message` in `actor.go` | `ID, Type, From, Text, Channel, ReplyTo, Payload, Attachments, Provenance, Ts`. `Provenance.UserUUID` is the only authoritative identity; never trust `Payload` for it. |
| Ownership | `internal/core/sessionhub/sessionhub.go`, `internal/core/lease`, `internal/core/inbox` | One lease per session. `Route` claims an unowned session locally, or queues the message for the owner. The hub is installed in **every** topology today, so `single` also writes lease rows. |
| Model calls | `internal/drivers/llm/llm.go`, `internal/core/caps/llm.go` | `Manager.Chat` and `StreamOpen/StreamNext/StreamClose`. A stream carries **text deltas only**; tool calls, usage and thinking arrive once, on the terminal `Frame`. `caps/llm.go` wraps the handlers with metering and quota holds (search for `metered`). `llm.Opts` has `Temperature, MaxTokens, Thinking, Tools, ToolChoice` (`ToolChoice` is `""`, `auto` or `none` only). |
| Turn capture | `internal/core/studio/broker.go` | The Studio playground registers a synthetic channel, stamps provenance itself (`Chat`, around the `prov :=` line), collects every reply of a turn and finishes when `SessionBusy` reports idle. |
| Identity | `internal/core/identity/identity.go` | `TokenUser(token) (userID, ok)` resolves a per-user bearer token. `IssueToken`, `RevokeToken` exist. The users API is `internal/core/users`. |
| Session state | `internal/core/caps/sessionstate.go` | `session.state.*` is durable per-session key/value (64 KiB per value, 256 keys), behind the `session.state` capability. |

Three facts drive the design:

1. **Idle sessions are never unloaded.** There is no idle timeout. Every
   conversation keeps a Luau state and a goroutine until the process exits.
2. **Lua variables are already not durable.** Hot reload, failover and a lost
   lease all restart the loop from the top. A correct loop cannot rely on its
   locals. Durable state is `session.state.*`, `memory.*`, `store.*`.
3. **A reply reaches only the instance that owns the session.** `Deliver`
   looks in that process's `pending` map. In a regional cell a request that
   lands on instance A for a session owned by B never gets its answer. The
   sync webhook has this limit today.

---

## 4. Decisions already made

These are fixed. Do not reopen them; build to them.

- **D1. Channel type `openai`**, package `internal/drivers/chatapi` (the name
  avoids a clash with the model client in `internal/drivers/llm/openai.go`).
- **D2. `model` in a request names an agent.** `GET /v1/models` lists the
  agents the channel exposes.
- **D3. The default behaviour is pass-through.** Client `messages`, `tools`,
  `tool_choice` and sampling parameters are forwarded to the agent's model, and
  the model's text and tool calls are returned unchanged. Client tools are
  never executed by AgentFlow.
- **D4. Session kind is chosen by the Lua route handler**, not by channel
  config. Three kinds: `ephemeral` (one turn), keyed (today's behaviour),
  persistent (daemons, unchanged).
- **D5. Ephemeral sessions take no lease, use no inbox, and are not retired.**
  Their key is unique per request, so two owners are impossible.
- **D6. Large requests stay out of Lua by default.** The message that crosses
  the router and enters the loop carries metadata and a handle. The loop can
  forward the request without materialising it, or ask for the full table.
- **D7. Real streaming in the first release**: text deltas are relayed as they
  arrive. Tool calls are sent as one complete chunk at the end, which the
  streaming protocol allows (clients concatenate `arguments` fragments; one
  fragment is valid).
- **D8. Identity comes from the bearer token**, stamped into
  `Provenance.UserUUID` by the channel. The request's `user` field is passed
  to the loop as untrusted metadata.
- **D9. Safety wins over streaming.** If the agent's safety profile can drop
  egress, the engine buffers the answer, checks it, and sends it whole.
  Live relay happens only when nothing can veto the text afterwards.
- **D10. Lua locals are a cache.** This becomes a documented contract, which
  is what allows unloading idle sessions.

---

## 5. Work, in order

Each phase ends with the build, vet and full tests passing, and with its own
tests. Do the phases in order; later ones depend on earlier ones.

### Phase 0: measure and capture

**0.1 Spawn benchmark.** Add `BenchmarkSessionSpawn` in
`internal/core/supervisor` (or `session`): time from `DeliverLocal` of the
first message to the echo loop's reply, with a stub gateway. Add
`BenchmarkRouterHop` for one message through the Lua router with a 1 KiB and a
1 MiB payload. Record the numbers in the PR description. They decide 4.3.

**0.2 Fixture recorder.** Add `cmd/agentflow/testdata/chatapi/` and a small
dev tool, `internal/drivers/chatapi/cmd/record` (a `main` package, not shipped
in the binary): an HTTP reverse proxy that forwards to a real
OpenAI-compatible upstream and writes each request and response pair to a
file, with the `Authorization` header and any key-looking string redacted.

**0.3 Capture real traffic.** You cannot run the target clients in CI. Ask the
owner to run the recorder against each client they care about (OpenClaw,
AstrBot, Roo Code, Cline, Open WebUI) and commit the captures as fixtures. If
captures are not available when you reach phase 3, hand-write fixtures from
the OpenAI API reference for: plain chat, streaming chat, a request with
`tools`, a follow-up carrying a `tool` role result, an image part, and
`stream_options.include_usage`. Mark hand-written fixtures as such in the file
name. **Do not claim compatibility with a client that has no captured
fixture.**

Acceptance: benchmarks run; recorder works against any OpenAI-compatible
server; fixtures directory exists with at least the hand-written set.

### Phase 1: session kinds

**1.1 Router contract.** In `internal/core/router/router.go`, add an optional
`kind` field to the `deliver` op (`"ephemeral"` or empty). Document it in the
router op contract comment at the top of `per_chat.lua`. Unknown values are
refused with a logged warning, not ignored.

**1.2 Ephemeral delivery.** Add `Supervisor.DeliverEphemeral(agent, key, msg)`
in `internal/core/supervisor/supervisor.go`:

- It never calls the hub. It calls the same `ensureSession` path with an
  `ephemeral` flag on the actor.
- The actor exits cleanly the **second** time its loop asks for the inbox:
  the first `inbox` returns the message, the second ends the session. Implement
  it in the `inbox` case of `dispatchInline`: when `a.Ephemeral` and a turn has
  already been served, return the same "stop" result `session.exit` produces.
- `onActorExit` must **not** add an ephemeral id to `retired` (that map would
  grow by one entry per request forever) and must not touch `leaseLost`.
- A crash in an ephemeral session ends it. Do not restart-loop it: `Run` must
  return after one `runOnce` when `a.Ephemeral`.
- Timers, shells, children and pending requests are reaped on exit exactly as
  for any session (the existing code in `onActorExit` already does this).

**1.3 Caps.** Ephemeral sessions are counted separately from the per-tenant
session cap. Add `runtime.limits.ephemeral_per_tenant` (int, default 16, 0
disables). When exceeded, `DeliverEphemeral` returns a typed error
(`ErrTenantBusy`) that the channel maps to HTTP 429. Add a metric
`agentflow_ephemeral_sessions` (gauge-style counter pair like
`agentflow_sessions_active`) and `agentflow_ephemeral_cap_drops`.

**1.4 Journaling mode.** Add a per-channel `journal` setting:
`full` (default, today's behaviour), `metadata` (ids, sizes, user, timings, no
text or attachments) and `off`. Apply it in both places messages are
journaled: the inbound `journalSink.RecordMessage` calls in
`cmd/agentflow/main.go`, and `Actor.egress` (via the channel name, through a
lookup the supervisor gives the actor). `metadata` must never write message
text, not even truncated.

**1.5 Tests.**

- An ephemeral session serves one turn and is gone; the supervisor's `sessions`
  and `retired` maps are unchanged in size after 1,000 requests.
- No lease row and no inbox row is written for an ephemeral session (assert on
  the store).
- The cap returns `ErrTenantBusy` and recovers when a session ends.
- A crashing ephemeral loop does not restart.
- `journal: metadata` writes no text (search the journal table for the body).

Acceptance: `per_chat.lua` unchanged in behaviour for existing channels; all
existing tests pass.

### Phase 2: reply and streaming primitives

The gateway can only deliver text. The channel needs structured replies,
deltas, and a reliable end-of-turn signal.

**2.1 Gateway capabilities.** In `internal/core/gateway/gateway.go` add
optional interfaces and matching `Registry` methods. A driver that does not
implement one gets the stated fallback:

```go
// Reply is a finished assistant message.
type Reply struct {
    Text         string
    ToolCalls    []ToolCall   // id, name, arguments as a JSON string
    FinishReason string       // "stop" | "tool_calls" | "length" | "content_filter"
    Usage        *Usage       // nil when unknown
    Reasoning    string       // optional, only sent when the channel allows it
}

type ReplyDriver interface { DeliverReply(replyTo string, r Reply) error }
// fallback: Deliver(replyTo, r.Text, nil); an error if r.ToolCalls is non-empty.

type DeltaDriver interface { DeliverDelta(replyTo string, text string) error }
// fallback: buffer in the registry per replyTo and flush on EndTurn.

type TurnDriver interface { EndTurn(replyTo string, err error) }
// fallback: no-op.
```

**2.2 End of turn.** In `Actor.dispatchInline`, where the `inbox` case emits
`turn.end`, call `a.gw.EndTurn(current.Channel, current.ReplyTo, nil)`. Also
call it with a non-nil error when a loop crashes mid-turn (`runOnce` returning
crashed while a current message exists) and when the session is cancelled
mid-turn. This replaces polling `SessionBusy`; leave the Studio broker as it
is.

**2.3 New session ops.** Add to the actor's op table and to
`internal/vm/prelude.go` (builders), `api/luau/af.d.luau` (types) and
`internal/core/studio/catalog.go` (catalog):

- `session.reply(table)`: op `reply`. Fields `text`, `tool_calls`,
  `finish_reason`, `usage`, `reasoning`. Runs `text` through the same egress
  safety and journal path as `send` (refactor `egress` so both share it), then
  `gw.SendReply`. At most one `reply` per turn; a second is an error returned
  to Lua.
- `session.send_delta(text)`: op `send.delta`. Delivers a text fragment via
  `gw.SendDelta`. Subject to D9: if the agent's safety dispatcher can drop
  egress, the engine accumulates deltas and releases them only at end of turn
  after the check. Expose whether that is the case to Go callers as
  `Actor.streamsLive()`.

`session.send(text)` keeps working on every channel. On the `openai` channel
each `send` appends to the turn's content (see 3.4).

**2.4 Model options.** Extend `llm.Opts` in `internal/drivers/llm/llm.go`,
and each provider serializer, with:

- `ToolChoice`: also `required` and a named function (`{name = "x"}` from Lua;
  represent as `ToolChoice: "function:x"` or a small struct, your call, but keep
  the existing three string values working).
- `ParallelToolCalls *bool`, `TopP *float64`, `Stop []string`, `Seed *int64`,
  `ResponseFormat map[string]any`.
- Providers that have no equivalent drop the field silently, except
  `ResponseFormat` and a named `ToolChoice`, which return a clear error rather
  than being ignored.

Every option needs a wire test per provider in the style of
`provider_wire_test.go`.

**2.5 `llm.forward`.** A new metered op in `internal/core/caps/llm.go`, built
on the **same metered stream path** as `llm.stream.open` (quota hold, usage
ledger, `agentflow_llm_*` metrics, user keys). It must not call the provider
directly.

Lua: `llm.forward(opts?) -> reply`.

- With no arguments it forwards the current turn's request (see D6 and 3.3)
  to the agent's model.
- `opts.model` overrides the model. `opts.messages` replaces the messages.
  `opts.prepend` and `opts.append` add messages before and after the client's.
  `opts.tools` adds tool definitions to the client's. `opts.params` overrides
  sampling parameters. `opts.max_tokens_cap` clamps the client's value.
- If the client asked for a stream and `Actor.streamsLive()` is true, each
  text delta is relayed with `gw.SendDelta` as it arrives.
- It returns `{ text, tool_calls, finish_reason, usage, thinking,
  thinking_blocks }`. It does **not** finish the HTTP response;
  `session.reply(reply)` does. If the loop ends the turn without calling
  `reply`, `EndTurn` finalises with what was relayed (`finish_reason = "stop"`).
- Outside a turn that has a forwardable request, with no `opts.messages`, it
  returns an error to Lua.

**2.6 Tests.** Fallbacks for each optional interface; `reply` twice in a turn
errors; deltas are held back when the safety profile can drop; `llm.forward`
writes one usage ledger row and releases its quota hold on success, on
provider error and on client cancel; `EndTurn` fires on normal end, crash and
cancel.

### Phase 3: the channel

**3.1 Config.** In `internal/config/config.go` (`Channel` struct) and
`internal/config/channels.go` (`validateChannelList`):

```yaml
gateway:
  channels:
    - name: api
      type: openai
      path: /v1/                  # serves <path>chat/completions and <path>models
      agents: [coder, helper]     # exposed as model ids (or `agent:` for one)
      auth: user_token            # user_token | key | none
      keys: ["${COMPAT_API_KEY}"] # auth: key only; secret references
      timeout: 600s               # whole-request limit
      max_request_bytes: 33554432
      journal: metadata           # full | metadata | off (default metadata for this type)
      expose_reasoning: false     # send model reasoning as reasoning_content
```

Validation rules, each with a message that says what to do:

- `path` ends with `/`. `/v1/` is allowed for this type even though
  `/v1/users/` is reserved: the driver registers the two exact routes, not the
  subtree. Extend `reservedChannelPath` accordingly and add a collision check
  between this channel's two routes and every other channel path.
- `agents` non-empty and every name is a configured agent. `agent` and
  `agents` together is an error.
- `auth: user_token` requires `runtime.identity.enabled`.
- `auth: none` is refused unless `gateway.listen` is loopback (reuse
  `loopbackListen` semantics; move the helper somewhere config can call).
- `auth: key` requires at least one key.
- A model whose `base_url` resolves to this instance's own listener and path
  is refused (an agent would call itself forever).

Wire construction in `cmd/agentflow/main.go` next to the other
`case "webhook":` channel cases. Add the type to `agentflow init`'s comments,
to `agentflow doctor` (PASS/WARN lines for auth mode and exposed agents) and
to the Studio channel form (`agentflow-studio/internal/ui`, the Channels
screen, and the validation route it calls).

**3.2 Request handling** (`internal/drivers/chatapi`).

- `POST <path>chat/completions`, `GET <path>models`, `GET <path>models/{id}`.
  Anything else under the path: 404 in OpenAI's error envelope.
- Authenticate first. `user_token`: `identity.TokenUser`; unknown token is 401.
  `key`: constant-time compare against the resolved keys.
- Parse with `max_request_bytes`. Reject `n > 1` (400). Accept and ignore
  unknown fields.
- Resolve `model` to an exposed agent, else 404 `model_not_found`.
- Build the session message:
  - `Type: "user"`, `Channel`, `ReplyTo: <request id>`,
    `Text`: the text of the last `user` message (empty if the last message is
    a `tool` result).
  - `From`: `"user:<channel>:<user uuid or key label>"`.
  - `Provenance`: `Kind: "channel"`, `UserUUID` set from the token for
    `user_token`. For `key` and `none`, leave identity to the normal
    resolution path; read `Actor.stampIdentity` and `actingIdentityOf` first.
  - `Payload` (small, see 6.2) with `request_ref`.
- Store the parsed request in a per-driver registry keyed by `request_ref`,
  removed when the turn ends or times out. This is D6: the body does not
  cross the router.
- Honour `Idempotency-Key`: derive the message id from it (hash, not the raw
  header), so a retried request is the same message. For ephemeral sessions a
  repeat while the first is still running attaches to the running request; a
  repeat after it finished is a new turn.
- Client disconnect cancels the request context; for an ephemeral session call
  `StopSession` so no more tokens are spent.

**3.3 Request access from Lua.** Two ops, added like the ones in 2.3:

- `openai.request()`: returns the full request as a Lua table (`messages`,
  `tools`, `tool_choice`, `params`, `metadata`, `user`). Refuses with a clear
  error above 4 MiB of JSON; the loop should forward instead of materialising.
- `llm.forward` (2.5) reads the same registry entry directly in Go.

Both resolve the entry from the current message's `request_ref`. A ref that is
not in this process's registry (a keyed session that moved instances) returns
an error naming the limitation; see 5.4.

**3.4 Reply assembly.**

- Non-streaming: wait for `DeliverReply` or `EndTurn`. Text from `send` calls
  is concatenated in order; a `reply` supplies the rest. Answer with one
  `chat.completion` object.
- Streaming: send the role chunk immediately, then an SSE comment line every
  15 s while idle (keeps proxies from closing the connection), text chunks on
  `DeliverDelta`, then on `DeliverReply` or `EndTurn`: one chunk with any
  remaining text, one chunk with the complete `tool_calls`, a chunk with
  `finish_reason`, a usage chunk if `stream_options.include_usage` was set, and
  `data: [DONE]`.
- If the engine buffered because of D9, the whole text goes out as a single
  content chunk. This is valid and clients handle it.
- `EndTurn` with an error before any output: an HTTP error (non-streaming) or
  an SSE `error` event (streaming). After output began: finish the stream with
  `finish_reason: "stop"` and log the error; do not send a malformed stream.
- A turn that produced nothing returns an empty assistant message, not a
  timeout.

**3.5 Built-in Lua.**

- `per_chat.lua`: when `msg.payload.api == "openai.chat"`, deliver with
  `kind = "ephemeral"` and `key = "req:" .. msg.payload.request_id`, unless
  `msg.payload.conversation_id` is set, in which case deliver keyed with
  `"conv:" .. channel .. ":" .. conversation_id`. Keep every other branch as it
  is.
- New `internal/builtins/lua/openai_passthrough.lua` (`plugin:openai_passthrough`):

  ```lua
  -- af-prelude-version: 1
  function loop()
    while true do
      local msg = session.inbox()
      local reply = llm.forward()
      session.reply(reply)
    end
  end
  ```

  Register it in `internal/builtins/builtins.go` and add it to the prelude
  contract test list.
- Add `examples/loops/openai_memory.lua`: a pass-through that prepends a
  system message built from `memory.recall` on a user-scoped facts store, to
  show the "exceptional" side.

**3.6 Tests.**

- Replay every fixture from phase 0 through an `httptest` server with a stub
  model, comparing the response shape field by field (ignore ids and
  timestamps).
- Streaming: chunk order, heartbeat, `[DONE]`, usage chunk only when asked.
- Tool round trip: request with `tools` returns `tool_calls`; the follow-up
  with a `tool` message reaches the model with the right `tool_call_id`.
- Auth: missing, wrong and revoked tokens; `auth: none` refused off loopback.
- Errors: every row of the table in 6.1.
- Isolation: two users' concurrent requests never see each other's request
  registry entries or replies.
- Client disconnect stops the session (assert no further model calls).
- A live end-to-end run: `agentflow run` with a stub model server, then curl a
  streaming request. Put the script in the PR description.

**3.7 Docs.** New page `agentflow-studio/docs/openai-api.md` (and its manifest
entry): what it is, the config block, the pass-through default, how to write a
loop against it, the tables from section 6, and an honest compatibility list
(only clients with captured fixtures). Update `configuration.md` (channel
type), `loops-reference.md` (new ops), `agentflow/README.md` (one bullet under
channels) and `agentflow.example.yaml` (a commented channel).

Acceptance for the plug-and-play milestone: with only `base_url` and an API
key configured, the OpenAI Python SDK's streaming and tool-calling examples
run unmodified against an agent using `plugin:openai_passthrough`.

### Phase 4: session lifecycle

**4.1 Unload idle sessions.**

- Config `runtime.sessions.idle_unload` (duration; see open question Q2 for
  the default). 0 disables.
- Track last activity on the actor (set when `waitInbox` returns a message and
  when a turn ends).
- A janitor in the supervisor (one goroutine, every minute) unloads a session
  when all hold: idle longer than the setting, not busy, empty mailbox, not a
  persistent agent, not ephemeral, and it owns no live resource. "Owns no live
  resource" needs four cheap queries; add them: scheduler timers by owner
  (`internal/core/scheduler`, add `OwnerPending(owner) int`), shells by
  session (`shellMgr`), live children, and pending `agent.request`s
  (`s.requests`).
- Unload is a new `EndReason` (`EndUnloaded`). `onActorExit` must not retire
  the id for it, and must release the session's lease: add
  `Hub.ReleaseSession(key)` that expires that one lease (same mechanism as
  `Release`, one key).
- The next message recreates the session through `ensureSession` and the loop
  starts from the top.
- Document D10 in `loops.md`: locals are a cache; use `session.state.*`.
- Tests: unloads after the idle time; does not unload with a pending timer,
  shell, child or request; the next message works and sees `session.state`
  values written before the unload; the lease is released; a session id is not
  retired by an unload; metric `agentflow_sessions_unloaded`.

**4.2 No session leases in `single`.** In `cmd/agentflow/main.go`, when the
topology is `single` and `-force-single` was not passed, do not install the
hub (`sup.SetHub`). The supervisor already supports a nil hub. Check and fix
every other use of `hub` in `main.go` for that case: `hub.Drain`, `SetOnLost`,
`memMgr.SetGenerationSource` (leave unset), `shellMgr.Sweep`'s liveness
function (give it one backed by the supervisor's session map),
`FailoverPersistent`, and `hub.Release` at shutdown. With `-force-single`
keep the hub: two processes may share the store, and the lease is the only
thing keeping a session from running twice. Tests: a `single` run writes no
`session:` lease rows; `-force-single` still does; the existing single-tier
tests pass.

**4.3 Cheaper activation, only if the numbers say so.** Use the phase 0
benchmark. If creating a session and reaching the first `inbox` costs more
than 2 ms on the CI Linux runner, do the first of these that gets it under
1 ms, measuring after each:

1. Cache each loop's compiled bytecode per (path, mtime, size) so a spawn
   loads bytecode instead of compiling source. Look at the C shim behind
   `internal/vm/vm.go` (`afvm_*`) for where compile and load are joined.
2. Keep a small pool (default 8) of **fresh** Luau states with the prelude
   and support chunks loaded. A state that has run any loop code is destroyed,
   never returned to the pool. Refill in the background. Hot reload of a
   support chunk must drain the pool.

If the cost is already under 2 ms, record the number and skip 4.3.

### Phase 5: stateful helpers and the regional gap

Do these only after phases 1 to 4 are merged.

**5.1 History fingerprint.** A route handler needs to recognise a conversation
that carries no id. Add a router-side op `chat.fingerprint` that returns, for
the current request, a list of chained hashes: `h[i] = sha256(h[i-1] || role
|| normalised content)`. With `router.state`, a handler can map the prefix
hash to a session key, detect a regenerate (prefix matches an earlier
checkpoint) or a branch (prefix matches, then diverges). Ship it as a helper
plus an example handler, `examples/routes/openai_conversations.lua`. Do not
make it the default route.

**5.2 `plugin:openai_agent`.** A loop for chat-style clients that want an
assistant instead of a model: it uses the agent's own instructions and
server-side tools, treats the last user message as the turn, ignores client
`tools`, and answers with `session.reply`. Document when to choose it.

**5.3 Reasoning replay example.** `examples/loops/openai_thinking.lua`: after
`llm.forward`, store `reply.thinking_blocks` in a user-scoped store keyed by
tool call id with a TTL; on the next request, reattach them to the matching
assistant message before forwarding. This is what makes Anthropic models with
thinking enabled usable from OpenAI-format clients across tool rounds.

**5.4 Cross-instance replies.** Fix fact 3 from section 3 for keyed sessions
in a regional cell, for this channel and the sync webhook:

- Make `ReplyTo` carry the origin instance (`<owner>/<id>`).
- Add a shared table, `channel_replies` (origin, reply id, seq, kind, body,
  created), in the runtime store. A driver asked to deliver a reply whose
  origin is another instance writes a row instead of failing.
- Each instance drains rows addressed to it on the hub's poll and delivers
  them locally. Sweep rows older than the channel timeout.
- For this channel, a keyed session on another instance also needs the request
  body: when the hub queues a message for another owner, inline the request
  JSON into the message payload (bounded by `max_request_bytes`), and make
  `openai.request()` and `llm.forward` read it from there when the local
  registry has no entry.
- Until 5.4 lands, `agentflow validate` must warn when an `openai` or sync
  `webhook` channel is used with `runtime.topology: regional` and a route that
  can produce keyed sessions. Ephemeral sessions are unaffected, because the
  receiving instance always owns them.

**5.5 Optional `turn(msg)` entry point.** A loop file that defines `turn`
instead of `loop` is called once per message with no state kept between calls.
Implement it as a prelude-provided `loop` that calls `turn`. Additive.

---

## 6. Contracts

### 6.1 HTTP

Request fields and what happens to them:

| Field | Handling |
|---|---|
| `model` | Agent name. Unknown: 404 `model_not_found`. |
| `messages` | Kept in the request registry. Roles `system`, `developer`, `user`, `assistant`, `tool`. Content parts: `text`, `image_url` (data URLs ingested through the channel's media policy; remote URLs passed to providers that accept them, else 400). |
| `stream`, `stream_options.include_usage` | Supported. |
| `tools`, `tool_choice`, `parallel_tool_calls` | Forwarded. Never executed here. |
| `temperature`, `top_p`, `max_tokens`, `max_completion_tokens`, `stop`, `seed`, `response_format` | Forwarded where the provider supports them (2.4). |
| `n` | Only 1. Otherwise 400. |
| `user`, `metadata` | Passed to Lua as untrusted metadata. `metadata.conversation_id` or the `X-AgentFlow-Conversation` header sets `payload.conversation_id`. |
| `Idempotency-Key` header | See 3.2. |
| anything else | Accepted and ignored. |

Response: the standard `chat.completion` and `chat.completion.chunk` objects.
`usage` maps `Input` to `prompt_tokens`, `Output` to `completion_tokens`,
`Cached` to `prompt_tokens_details.cached_tokens`, `Reasoning` to
`completion_tokens_details.reasoning_tokens`. Tool call `arguments` is the JSON
encoding of `ToolCall.Args`.

Errors use OpenAI's envelope `{"error": {"message", "type", "code"}}`:

| Situation | Status | `code` |
|---|---|---|
| Missing or bad credentials | 401 | `invalid_api_key` |
| Unknown model | 404 | `model_not_found` |
| Bad JSON, `n > 1`, unsupported content part | 400 | `invalid_request_error` |
| Request too large | 413 | `request_too_large` |
| User quota exhausted | 429 | `insufficient_quota` |
| Ephemeral cap or session cap reached | 429 | `rate_limit_exceeded`, with `Retry-After` |
| Input blocked by safety | 400 | `content_filter` |
| Output blocked by safety | 200 | `finish_reason: "content_filter"` |
| Upstream model error | 502 | `upstream_error` (do not leak the upstream body) |
| Turn timeout | 504 | `timeout` |
| Draining (readiness false) | 503 | `server_unavailable` |

### 6.2 What the loop receives

`msg.payload` on this channel (small on purpose):

```lua
{
  api = "openai.chat",
  request_id = "…", request_ref = "…",
  model = "coder", stream = true,
  message_count = 12, has_tools = true, bytes = 48211,
  conversation_id = nil,          -- from header or metadata, untrusted
  user = "…", metadata = { … },   -- untrusted
}
```

### 6.3 Lua API added

| Call | Purpose |
|---|---|
| `openai.request()` | The full request as a table. Size-capped. |
| `llm.forward(opts?)` | Forward the turn's request to a model, relaying deltas. Returns the reply. |
| `session.reply(t)` | Finish the turn with a structured assistant message. |
| `session.send_delta(text)` | Send a text fragment of the answer. |
| router `deliver { …, kind = "ephemeral" }` | One-turn session. |
| router `chat.fingerprint` (phase 5) | Chained history hashes. |

All additive. `PreludeVersion` stays 1.

---

## 7. Open questions for the owner

Proceed with the stated default; raise the question in your first report.

- **Q1. Client system prompts.** Default: forwarded as sent (pass-through).
  `plugin:openai_agent` ignores them. Alternative: a channel setting to strip
  them.
- **Q2. Default for `idle_unload`.** Default: `30m`. It changes behaviour for
  existing deployments (a loop loses its locals after 30 idle minutes), which
  is allowed by D10 but is visible. Alternative: `0` (off) for one release.
- **Q3. Default `journal` for this channel type.** Default: `metadata`,
  because coding contexts are large and private. Alternative: `full`, like
  every other channel.
- **Q4. `expose_reasoning`.** Default: off. Some clients display
  `reasoning_content`; turning it on leaks model reasoning to the caller.
- **Q5. Which clients must have captured fixtures before release.** Needed
  for phase 0.3.

---

## 8. Definition of done

- Phases 1 to 4 merged, each with tests, docs and passing CI on all three
  platforms.
- The OpenAI Python SDK examples for streaming and tool calling run unmodified
  against `plugin:openai_passthrough`.
- Every client named as compatible in the docs has a captured fixture replayed
  in the test suite.
- A `single` deployment writes no session lease rows; an idle session is
  unloaded and comes back with its `session.state` intact.
- 1,000 ephemeral requests leave no growth in the supervisor's maps, the lease
  table, the inbox table or (with `journal: metadata`) message text in the
  journal.
- Your final report says what was not verified (Postgres paths, real clients
  without fixtures).
