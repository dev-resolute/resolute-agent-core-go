# Changelog

## [0.2.0] - 2026-05-28

This release adopts upstream Pi's single-runner, mutable-Agent shape (ADR-0006) and
ports a set of v0.76.0 capabilities and correctness fixes. It is a breaking release;
because there are no external consumers yet, it is a clean bump with no deprecation
cycle. Migration is mechanical — see the recipe below.

### Breaking Changes

- **`*Run` is dissolved.** `Agent.Run(...)` is replaced by `Agent.Prompt(ctx, msg)`,
  which returns the `EventStream` struct directly. All methods formerly on `*Run`
  move to `*Agent`.
- **Agent is now single-runner and mutable.** One `Agent` represents one conversation
  and runs at most one prompt at a time. Runtime config (model, tools, system prompt,
  thinking level, skills) is mutable via setters; changes take effect on the next turn.
  Multi-tenant servers now hold N Agents for N concurrent sessions instead of sharing
  one immutable Agent across N Runs.
- **Type renames:**
  - `RunResult` → `PromptResult`
  - `RunState` → `AgentState`
  - `RunPhase` → `AgentPhase`
  - `RunOpts` → `PromptOpts`
  - `ErrRunCancelled` → `ErrPromptCancelled`
  - `ErrRunStopped` → `ErrAgentStopped`
- **New sentinel:** `ErrAgentBusy` — returned by `Prompt` when a prompt is already in flight.
- **`AgentConfig` fields are now initial values of mutable state**, not immutable defaults:
  `DefaultModel`, `DefaultThinking`, `SystemPrompt`, `Tools` seed the Agent's starting
  state and are thereafter changed via setters.

### Migration Recipe

**1. `Agent.Run` → `Agent.Prompt`; drop the `*Run` handle.**

```go
// before (v0.1.x)
run, err := agent.Run(ctx, pi.RunOpts{Prompt: pi.NewText("user", "hi")})
for ev := range run.Events() { ... }
result := <-run.Done()

// after (v0.2.0)
stream, err := agent.Prompt(ctx, pi.NewText("user", "hi"), pi.PromptOpts{})
for ev := range stream.Events { ... }
result := <-stream.Done
```

**2. Move control methods from the run handle to the Agent.**

```go
// before                          // after
run.Stop()                          agent.Stop()
run.Steer(ctx, m)                   agent.Steer(ctx, m)
run.FollowUp(ctx, m)                agent.FollowUp(ctx, m)
run.State()                         agent.State()
run.Transcript()                    agent.Transcript()
```

**3. Apply type renames** (`RunResult`→`PromptResult`, `RunState`→`AgentState`,
`RunPhase`→`AgentPhase`, `RunOpts`→`PromptOpts`). A search-and-replace covers most of it:

```
RunResult        → PromptResult
RunState         → AgentState
RunPhase         → AgentPhase
RunOpts          → PromptOpts
ErrRunCancelled  → ErrPromptCancelled
ErrRunStopped    → ErrAgentStopped
```

`PromptOpts` no longer carries `Prompt` — the message is the second argument to
`Agent.Prompt(ctx, msg, opts)`, with `opts PromptOpts` third. Other former
`RunOpts` fields (`SessionID`, `Model`, `SystemPrompt`) remain on `PromptOpts`
for per-call overrides, or are set on the Agent via setters before the prompt.

**4. One Agent per conversation, not one Agent shared across requests.**

```go
// before: shared immutable Agent at boot, Run per request
var agent = pi.NewAgent(cfg)            // process-wide
func handle(r) { run, _ := agent.Run(...) }

// after: construct an Agent per session/request
func handle(r) {
    agent, _ := pi.NewAgent(cfg)        // per request/session
    defer agent.Close()
    stream, _ := agent.Prompt(r.Context(), msg, pi.PromptOpts{})
}
```

**5. Update error matching to the renamed sentinels.**

```go
// before
if errors.Is(result.Err, pi.ErrRunStopped) { ... }
// after
if errors.Is(result.Err, pi.ErrAgentStopped) { ... }
```

**6. Reconfigure mid-conversation with setters instead of rebuilding the Agent.**

```go
// new in v0.2.0 — no v0.1.x equivalent (you had to rebuild)
agent.SetModel("gemini/gemini-2.5-flash")
agent.SetThinkingLevel(pi.ThinkingLow)
agent.SetSkills(updatedSkills)
```

### Added

- **Single-runner mutable Agent** with `SetModel`, `SetTools`, `SetSystemPrompt`,
  `SetThinkingLevel`, `SetSkills`; turn-snapshot semantics (setters affect the next
  turn, never the in-flight turn). (ADR-0006)
- **Skills.** `Skill` struct auto-rendered into the system prompt as an XML index of
  name + description + filePath; the model fetches full content on demand via a
  user-supplied content-reader tool. `DisableModelInvocation` hides a skill from the
  model-visible list. `Agent.SetSkills(...)` hot-reloads.
- **`piskills` package** — opt-in `Load(dir)` helper that walks `SKILL.md` files,
  parses YAML frontmatter, honors ignore files, and returns skills plus diagnostics.
- **`ShouldStopAfterTurn`** — graceful exit after a completed turn before polling
  queues or starting another LLM call. (upstream 0.72.0)
- **`ToolResult.Terminate`** — skip the automatic follow-up LLM call when every
  finalized tool result in a batch opts in. (upstream 0.69.0)
- **`Tool.PrepareArguments`** — transform raw tool-call arguments before schema
  validation; a compatibility shim for resumed sessions with outdated tool schemas.
  (upstream 0.64.0)
- **`Agent.Context()`** — exposes the in-flight prompt's `context.Context` so tools
  and hooks can forward cancellation into nested async work. (upstream 0.63.2)
- **`AgentConfig.ThinkingBudgets`** — per-`ThinkingLevel` token-budget overrides,
  forwarded to the provider. (upstream 0.38.0; requires pi-llm-go v0.2.0)
- **Session-id forwarding** — the conversation's session id is forwarded to the
  provider for session-keyed caching. (upstream 0.37.3; requires pi-llm-go v0.2.0)
- **`Transport` preference** wired through to the provider, defaulting to auto;
  websocket reserved for a future provider. (upstream 0.52.12/0.72.1; requires
  pi-llm-go v0.2.0)

### Fixed

Each fix landed with the corresponding upstream test ported to Go as a permanent
`_v076_test.go` regression fixture.

- Tool-call preflight stops preparing sibling tool calls after the prompt is aborted.
  (upstream 0.75.4)
- Steering waits until the current assistant message's tool-call batch fully finishes
  instead of skipping pending tool calls. (upstream 0.58.4)
- `AfterToolCall` hook errors produce an error tool result instead of aborting the
  batch. (upstream 0.67.67)
- Parallel tool execution emits tool-end events as each tool finalizes while still
  persisting tool-result messages in assistant source order. (upstream 0.68.1)
- Queued steering/follow-up messages resume correctly when a resumed session ends in
  an assistant message, preserving one-at-a-time ordering. (upstream 0.52.7)
- `Prompt` rejects with `ErrAgentBusy` when a prompt is already streaming. (upstream 0.32.0)
