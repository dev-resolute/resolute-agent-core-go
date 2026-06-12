# 6. Reverse the multi-runner-Agent invariant: Agent becomes single-runner and mutable

Date: 2026-05-28
Status: Accepted

## Context

`pi-core-agent-go` v0.1.x shipped with a deliberately Go-idiomatic deviation from upstream Pi: the `*Agent` is **immutable after `NewAgent`** and goroutine-safe across many concurrent `*Run`s. One Agent constructed at process start serves N concurrent requests, each carrying its own per-invocation state on `*Run`. This shape was justified by Go's server-handler deployment model and documented in `CONTEXT.md` under the term "Multi-runner Agent".

Upstream Pi went the opposite direction. From v0.31.0 onward, the `AgentHarness` IS the session: it holds `model`, `systemPrompt`, `messages`, `resources` as mutable fields, exposes `setModel(...)` / `setTools(...)` / `setSystemPrompt(...)` / `setResources(...)` setters, and is single-runner (one in-flight prompt per harness). This shape lets long-running conversations swap model, refresh resources, or hot-reload skills mid-session without recreating the harness.

When reviewing upstream's v0.76.0 release for portable changes, the cumulative gap between the two shapes had become large enough that further selective porting was producing API drift and divergent semantics on a per-feature basis (Q3 explored three reconciliation options: (a) full upstream shape, (b) mutation on `*Run`, (c) two-track API).

## Decision

Reverse the multi-runner-Agent invariant. Adopt upstream's single-runner, mutable harness shape directly:

- `*Agent` becomes **mutable**. All runtime config (`model`, `tools`, `systemPrompt`, `thinkingLevel`, `skills`, stream options) lives on `*Agent` as mutex-guarded fields.
- `*Agent` becomes **single-runner**. One in-flight prompt per Agent. Concurrent users get N Agents (one per session), not one Agent shared across N Runs.
- The Agent exposes upstream-shaped setters: `SetModel`, `SetTools`, `SetSystemPrompt`, `SetThinkingLevel`, `SetSkills`. Each mutates `*Agent` immediately; the change takes effect on the next turn snapshot.
- Each provider request is built from a **turn snapshot** taken at turn start — concurrent setters during an in-flight turn affect the next turn, not the current one. This is the upstream rule, ported verbatim.
- **`*Run` is dissolved.** All methods previously on `*Run` (`Stop`, `Steer`, `FollowUp`, `State`, `Transcript`) move to `*Agent`. `Agent.Prompt(ctx, msg)` returns an `EventStream` (the existing struct with `Events` + `Done` channels) — no separate handle. The terminal value delivered on `EventStream.Done` is renamed `RunResult` → `PromptResult` to match the operation that produced it. Sentinel errors lose the `Run` prefix per CS-2 (no stutter inside package `pi`): `ErrRunCancelled` → `ErrPromptCancelled`, `ErrRunStopped` → `ErrAgentStopped`.

## Why

- **API-surface compatibility with upstream.** Porting future fixes (CHANGELOG entries past v0.76.0) becomes mechanical once setter names and snapshot semantics match. The compounding cost of selective porting is gone.
- **Live mutation is a first-class need we cannot model in the immutable shape.** Mid-conversation model swap, system-prompt edit, and resource hot-reload are real Go use cases (operator dashboards, chat-app deployments) that the immutable Agent forced into Agent-rebuild workarounds.
- **Skills upstream live on the harness.** Porting them into our previous shape required inventing a new home for them or duplicating them per `*Run`. Putting them on `*Agent` matches upstream and matches the "user's session" mental model. (Per Q5 we port `Skill` and the auto-render-into-system-prompt behavior; we explicitly do not port `PromptTemplate` — it's an app-layer slash-command UI concept, framework adds no value — nor `ExecutionEnv` — `os.ReadFile` suffices.)
- **The Go-server objection is weaker than it looked.** N Agents for N concurrent users is the standard chat-app shape. The original concern (allocation overhead) is dominated by per-conversation transcript and provider-state allocations that already exist. A handful of mutex-guarded fields per Agent is not the bottleneck.
- **Cross-user isolation moves from type-system enforcement to per-Agent ownership.** Worth the trade because callers can use one of several standard ownership patterns (per-request Agent in a handler, Agent-per-session in a map, per-connection Agent in a long-lived WebSocket) — all clearer than reasoning about shared-immutable-Agent + per-Run state.

## Consequences

- **`v0.1.x` API breaks.** Existing callers using `agent.Run(ctx, opts)` get a new shape. v0.2.0 ships as a breaking release; CHANGELOG documents the migration path explicitly.
- **`AgentConfig` shrinks.** Fields like `SystemPrompt`, `DefaultModel`, `DefaultThinking` move from "agent-wide defaults" to "initial values of mutable Agent state". `Tools` becomes the initial tool set, mutable thereafter via `SetTools`.
- **`*Run` is dissolved.** Methods become methods on `*Agent`. `Agent.Prompt(...)` returns the existing `EventStream` struct. Type renames: `RunResult` → `PromptResult`, `RunState` → `AgentState`, `RunPhase` → `AgentPhase`, `RunOpts` → `PromptOpts`, `ErrRunCancelled` → `ErrPromptCancelled`, `ErrRunStopped` → `ErrAgentStopped`. The `Run` name leaves the public surface entirely.
- **Mutex everywhere on the read path.** Every turn-snapshot construction takes a read lock on the Agent's config. Snapshot construction must be cheap (shallow copy of slices/maps; no deep copy). Benchmarks expected to be unchanged; flagged for verification.
- **`Steer` and `FollowUp` get simpler semantics.** Only one in-flight prompt per Agent, so "which run does this Steer apply to" disappears as a question. The buffer/queue mechanics stay; the addressing collapses.
- **Multi-tenant server users carry the per-session lifecycle complexity.** A chat server holding 1000 concurrent conversations now holds 1000 `*Agent`s, plus the responsibility of evicting them on session end. This is the standard pattern — but it does need a `Close()` or context-tied cleanup story.
- **ADR-0003 (compaction) and ADR-0004 (cancellation) keep their content.** Compaction operates on the Agent's session; cancellation rules stay the same per-Run/per-Agent shape.
- **CONTEXT.md changes.** The "Multi-runner Agent" entry is replaced with a "Single-runner Agent" entry. The `Run` entry, `Session ID lifecycle`, `Goroutine-safety contract`, `Steer`, `FollowUp` entries all need follow-up rewrites once Q4 settles `*Run`'s fate.
- **Test surface changes.** `agenttest.NewAgent(t, opts)` becomes the per-test fixture, not a shared package-level Agent. Tests that previously exercised concurrent Runs against one Agent are rewritten to exercise one Agent each — the concurrency property they tested (cross-Run isolation) is now enforced by construction.
