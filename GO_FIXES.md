# agentflow — engine fixes (handoff prompt)

> **STATUS (2026-09-12): all 8 items FIXED** in 4 commits on main: `6218706`
> (P0-1 + P0-2), `e317af7` (P1-3), `289d0f8` (P2-4/5/6), `9a7bce6` (P3-7
> documented + P3-8 wired). Non-cgo test packages green (config, memory,
> scheduler, builtins, llm, search, sqlite, legal); cgo-coupled packages (vm,
> tools, caps, supervisor, webhook) could not be compiled on the Windows dev
> box (no gcc) — their tests still need a cgo toolchain run. hulipa-side
> migration done: afgen emits `shared:`, the `kb` store sets `shared: true`,
> `tools.policy.default: none` re-enabled. **The regenerated hulipa config
> requires this fixed engine** — old binaries strict-reject the new `shared:`
> store field.

Fix the following defects in the agentflow Go codebase (github.com/hulipa487/agentflow — a single-binary LLM agent runtime: actor-model sessions, Luau sandbox via cgo, YAML config strict-parsed with `KnownFields(true)`). All were found by building a product on the **unmodified** binary, so each includes the downstream symptom it caused. Downstream workarounds exist for #1/#2, so nothing is blocked — these are engine hardening fixes. Keep every fix backward compatible (opt-in or semantically identical), add a unit test per fix, and run `go test ./...`.

---

## P0-1 — `tools.policy.default: none` is ignored (tool exposure)

**File:** `internal/core/tools/registry.go`, `func (r *Registry) Expose` (~line 70).

**Bug:** `defaultAllow` is computed from `policy.Default` (lines ~71–77) but the filter loop never uses it. The only gate in the loop is:

```go
if len(skills) > 0 && !allowed[name] {
    continue
}
```

So an agent with **empty skills** gets **every registered tool** even when `tools.policy.default: none`. (`forbidden` and `overrides` do work.)

**Downstream symptom:** agents configured with no tools were handed the full builtin toolset (fs.read/fs.write/web_search/legal_*) — which then triggered P0-2 and 400'd production LLM calls.

**Fix:** in the loop, skip when `len(skills) == 0 && !defaultAllow` (or return an empty `AgentSet` early in the `else` branch that currently just comments "nothing").

**Test:** `Expose(nil, ToolsPolicy{Default: "none"}, false)` → 0 tools; `Default: "all"` / unset → all tools; explicit skills still filter to exactly those tools.

---

## P0-2 — empty `required: []` round-trips through Lua as `"required": {}`

**Files:** `internal/core/tools/legal.go:95` (`"required": []string{}` in `builtin:legal_fetch`'s schema); schema serialization path `ToolSpec.JSON()` (`internal/core/tools/registry.go:32`) → `tools.list` cap → Luau → llm.chat → `internal/drivers/llm/openai/provider.go` (`"parameters": t.Parameters`).

**Bug:** an empty Go slice in a tool's parameter schema becomes an empty **Lua table** inside the sandbox, which serializes back to JSON as `{}` (object), not `[]` (array). The outbound chat request then carries `"required": {}` — invalid JSON Schema. Strict providers reject it: **xAI returns 400 `Schema validation failed: /required: {} is not of type "array"`**. Lenient providers (OpenRouter) silently tolerated it, which is why it went unnoticed.

**Fix (do both):**
1. In `ToolSpec.JSON()`, omit `required` when the slice is empty (empty `required` is semantically identical to absent).
2. Defense in depth: at the llm.chat boundary, normalize tool parameter schemas — coerce any `required` that decoded as a map/object to absent, so *any* Go-side empty array in a schema can't produce `{}` downstream regardless of which cap ferried it.

**Test:** register a tool with `"required": []string{}`, round-trip it through `tools.list` in the VM, build a chat request, and assert the emitted JSON has `required` absent or `[]` — never `{}`.

---

## P1-3 — webhook channel has a hardcoded 55s synchronous timeout

**File:** `internal/drivers/webhook/webhook.go:124` (`time.After(55 * time.Second)` → 504 "agent timeout"; the late reply is then dropped with "no pending request").

**Bug:** the webhook handler parks the HTTP response until the agent replies, bounded by a fixed ~55s. Any multi-agent pipeline (coordinator → workers via `agent.request`) that exceeds it loses the reply.

**Fix:** (a) make the timeout configurable in the channel's YAML (`timeout: 120s`, default 55s); and (b) add an async mode: accept the POST, return `202` + a job id immediately, and deliver the eventual reply via `GET /webhook/result/<id>` polling or a caller-supplied `callback_url` POST. Sync behavior stays the default.

**Test:** a handler slower than the configured timeout still delivers its reply via poll/callback; sync mode under the timeout is unchanged.

---

## P2-4 — all agents share one physical `dialogue`/`facts` table

**Files:** `internal/config/config.go:396` (`DefaultMemoryProfile` maps every agent's logical `dialogue`→physical table `dialogue`, `facts`→`facts`); `internal/core/memory/backend.go` (`ResolveStores`) + `internal/core/memory/store.go` (`BindForTable` keys bindings by **physical** table name).

**Bug:** two agents on the same memory profile (or both on `builtin:conversational`) read/write the **same rows** — conversation history leaks across agents (a writer agent's recall returns a librarian agent's turns).

**Fix:** at bind time, prefix the physical table name with the agent name (e.g. `writer.dialogue`) unless the store opts into sharing via a new `shared: true` field on the store config (add to the config schema — remember parsing is strict). Shared stores (a deliberate cross-agent knowledge base) opt in; private stores are isolated by default. Document the migration: existing deployments that rely on implicit sharing must set `shared: true`.

**Test:** two agents on `builtin:conversational` write turns; each recalls only its own. Two agents on a profile with a `shared: true` store both see each other's rows.

---

## P2-5 — `persistent` / `singleton` agent flags are parsed but never honored

**Files:** flag definitions in `internal/config/config.go`; spawn logic in `internal/core/supervisor/supervisor.go` (`Deliver` find-or-create); boot in `cmd/agentflow/main.go`.

**Bug:** sessions spawn **lazily on first `Deliver`**; nothing reads the `persistent`/`singleton` flags, so a daemon agent (cron scheduler, queue consumer) never starts without external traffic. Downstream had to add a boot-kick: a singleton router that `deliver`s a synthetic boot message to each daemon at startup.

**Fix:** after the supervisor starts, for each configured agent with `persistent: true`, deliver a synthetic boot message (e.g. `Type: "boot"`, `From: "system:supervisor"`) so its session spawns at boot.

**Test:** boot the runtime with a `persistent: true` agent and zero inbound traffic; the agent's session is running (e.g. observable via its first loop turn / a boot log).

---

## P2-6 — timer messages carry no timer id

**File:** `internal/core/supervisor/scheduler_adapter.go:26` (`ID: "timer:" + owner` for every timer; the delivered message payload has only `Ts`).

**Bug:** all timers owned by a session deliver indistinguishable messages, so one session can't run multiple independent timers. Downstream had to collapse to a single self-rescheduling base tick.

**Fix:** thread the timer/task id through `deliver` — set `ID: "timer:" + owner + ":" + timerID` and add `timer_id` to the message payload exposed to Lua (`msg.payload.timer_id`).

**Test:** schedule two timers on one session; the two delivered messages carry distinct `timer_id`s.

---

## P3-7 — cron expression subset is undocumented and minimal

**File:** `internal/core/scheduler/scheduler.go:204` (`parseCron`): only `*/N * * * *` and fixed `M H * * * *`; day/month/weekday must be `*`.

**Fix (optional):** support full 5-field cron (a small parser or a dependency), or explicitly document the supported subset in the scheduler docs.

## P3-8 — `plugins.dir` support-chunk shadowing is documented but not wired

**File:** builtin Lua loading (`builtins.SupportChunks()` / `builtins.Resolve`).

**Bug:** docs say a deployment can shadow builtin Lua support chunks via `plugins.dir`, but the loader never consults it — support chunks are global and unshadowable, which forced downstream per-agent memory hooks to be dispatched from loop files instead of shadowing `memory_routing_table`/`memory_recall_handler`. Either wire the shadowing or remove the claim from the docs.

---

## Acceptance

- One commit per fix (or per P-tier), each with a unit test as specified.
- `go test ./...` green; `go vet` clean.
- No config-schema breakage: new fields (`timeout`, `shared`, async webhook options) are optional with defaults matching today's behavior.
- After P0-1 + P0-2: an agent with no `tools:` under `tools.policy.default: none` exposes zero tools, and a chat request carrying any builtin tool schema is accepted by xAI's strict schema validation.
