# ADR-0007 — Config-update notifications are hook fields, not harness events

**Status:** Accepted (2026-06-12)
**Context repos:** `pi-core-agent-go`

## Context

Upstream pi-agent-core notifies observers when the harness's runtime config mutates:
`model_update`, `thinking_level_update` (renamed from `*_select` in 0.77.0), and
`tools_update` (new in 0.77.0, emitted by `setTools`/`setActiveTools`). Upstream can
deliver these because its harness owns a **persistent event subscription** that outlives
any single prompt.

Our port has no such surface. `EventStream` is per-Prompt: it is created by
`Agent.Prompt(...)` and closes when that Prompt completes. Setters (`SetModel`,
`SetThinkingLevel`, `SetTools`, `SetActiveTools`, `SetSystemPrompt`, `SetSkills`) are
typically called **between** Prompts, when no event channel exists at all.

## Decision

Config-update notifications are delivered through the existing `Hooks` struct: a new
optional field `OnConfigUpdate(ConfigUpdateCtx)` fires synchronously inside each setter,
regardless of whether a Prompt is in flight. `ConfigUpdateCtx` is a concrete per-hook
context struct (per our existing hook convention) describing what changed, with old and
new values.

The upstream event names (`model_update`, `thinking_level_update`, `tools_update`) do
**not** exist as `AgentEvent` variants in the Go port.

## Alternatives considered

1. **Emit at next turn snapshot** — emit `ModelUpdateEvent` etc. on the next Prompt's
   `EventStream` when the snapshot picks up the change. Rejected: delivery is deferred
   until someone prompts; an observer cannot see a change while the Agent is idle, and
   the event's timing no longer corresponds to the mutation.
2. **Persistent `Agent.Subscribe()` channel** — closest to upstream's model. Rejected:
   introduces a new concurrency surface (channel lifetime, slow consumers, backpressure,
   close-on-what semantics) for marginal benefit; our `Hooks` idiom already exists and
   the framework's other extension points are hook fields.
3. **No notifications** — rely on `Agent.State()` polling and persisted
   `active_tools_change` session entries. Rejected: drops `tools_update` parity outright
   and leaves multi-observer servers (UI + audit log) without a push path.

## Consequences

- Hooks are set at construction via `AgentConfig.Hooks`; late-binding an observer
  requires planning at construction time, unlike upstream's subscribe-anytime model.
- The hook runs synchronously on the setter's calling goroutine and is subject to the
  goroutine-safety contract; a slow hook slows the setter, not the prompt loop.
- A future persistent subscription surface, if ever needed, can be layered on top by a
  user installing a hook that fans out to channels — the framework does not own that
  complexity.
- When diffing the Go port against upstream's event inventory, `model_update`,
  `thinking_level_update`, and `tools_update` are intentionally absent; this ADR is the
  record of why.
