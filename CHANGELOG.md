# Changelog

## [Unreleased]

> **Live validation:** full agent tool loop confirmed against live `gemini-3.1-pro-preview`
> (`TestLiveGemini3AgentToolLoop`); without the round trip the auto-continued turn is rejected with
> `400 INVALID_ARGUMENT: Function call is missing a thought_signature`.

### Added

- **Gemini 3 thought-signature round trip.** Bumps `resolute-llm-go` v0.7.0 -> **v0.8.0** (which
  captures/replays Gemini 3 `thoughtSignature` on tool calls and fixes tool calls lost to chunk
  layout) and threads the signature through the agent transcript so multi-turn tool loops work on
  `gemini-3*` models: the prompt runner copies `ThoughtSignature` from `llm.ToolCallStartEvent`
  into the persisted tool_call message, and `DefaultConvertToLLM` replays it onto the rebuilt
  `llm.ToolCallContent`. New additive API: `NewToolCallWithSignature` (persists the signature in
  the tool_call body as `thought_signature`; nil-signature equivalent to `NewToolCall`) and
  `Message.ToolCallThoughtSignature()` (nil when absent -- pre-existing transcripts keep working).
  Custom `ConvertToLLM` implementations replaying tool calls to Gemini 3 must do the same.

## [0.5.0] - 2026-06-28

### Added

- **Six OpenAI-compatible providers reachable through the agent (AGENT-16).** Bumps the
  `resolute-llm-go` dependency v0.6.0 → **v0.7.0**, which adds xAI, Mistral, Qwen, and z.ai as named
  `openai-compat` instances alongside OpenAI and OpenCode Zen. No agent-core code change was needed —
  the provider registry (`AgentConfig.Providers`, resolution by `<provider>/<model>` ref) is already
  generic; this release is the dependency bump plus proof it carries the new targets. A registry test
  asserts Gemini and all four LLM-10 compat providers (distinct `Name`s) each resolve by ref, and
  `examples/providers` wires a seven-provider agent (Gemini + the six compat targets), each registered
  when its API key is present.

### Changed

- **`resolute-llm-go` v0.6.0 → v0.7.0** (the four new providers + `openaicompat.Config.Name`). Both
  repos are now public, so the dependency resolves over plain `go get` — the `GOPRIVATE` workaround
  noted in v0.4.0 is no longer required.

## [0.4.0] - 2026-06-27

### Changed

- **Module path changed to `github.com/dev-resolute/resolute-agent-core-go`** (was
  `github.com/resolute-sh/pi-core-agent-go`), part of the `resolute-sh`→`dev-resolute` rebrand
  (note the name flip: *core-agent* → *agent-core*). Update your import path:
  `go get github.com/dev-resolute/resolute-agent-core-go`.
- **Dependency repointed to `github.com/dev-resolute/resolute-llm-go v0.6.0`** (was
  `github.com/resolute-sh/pi-llm-go v0.5.0`) — same code under the renamed module identity.
- **No behaviour change** — pure module-path rename + dependency repoint; the full test suite
  passes unchanged. ADR-0005 carries a rebrand note. Set `GOPRIVATE` to include
  `github.com/dev-resolute/*` to resolve the private dependency.

## [0.3.0] - 2026-06-26

Bumps the `pi-llm-go` dependency v0.2.0 → v0.5.0, adopting the upstream 0.79.10 re-diff
fixes for the agent's LLM layer. No changes to pi-core-agent-go's own API — existing callers
compile and behave identically, with corrected and expanded provider behaviour underneath.

### Changed

- **`pi-llm-go` v0.2.0 → v0.5.0.** Pulls in:
  - **Gemini 3 correctness + streaming fix** (pi-llm-go v0.3.0, LLM-5): capabilities and thinking
    config now derive by generation (Gemini 3.x / Gemma 4 use `thinkingLevel`), `IncludeThoughts`
    surfaces reasoning, and the stream loop no longer drops or garbles multi-chunk responses —
    which affected all Gemini models, including the `gemini-2.5-flash` the live agent suite uses.
  - **Gemini Vertex AI backend** (pi-llm-go v0.3.0): `gemini.Config` ADC / Workload-Identity path.
  - **OpenAI-compat `Compat` + `deepseek`/`chat-template` thinking formats** (pi-llm-go v0.4.0/v0.5.0,
    LLM-6/LLM-7): DeepSeek V4 on opencode-go and Qwen3/DeepSeek-R1 behind vLLM.
  - **`ErrContextOverflow` detection** (pi-llm-go v0.5.0, LLM-8): `errors.Is`-matchable seam for
    the deferred auto-compaction story (ADR-0003).

## [0.2.0] - 2026-05-28

Tracks upstream pi-agent-core 0.79.1.

This release adopts upstream Pi's single-runner, mutable-Agent shape (ADR-0006) and
ports capabilities from upstream pi-agent-core 0.76.0–0.79.1. It is a breaking release;
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
  `SetThinkingLevel`, `SetSkills`, `SetActiveTools`; turn-snapshot semantics (setters
  affect the next turn, never the in-flight turn). (ADR-0006)
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
- **`Hooks.OnConfigUpdate`** — config-update notification hook; fires synchronously
  inside every setter (`SetModel`, `SetThinkingLevel`, `SetTools`, `SetActiveTools`,
  `SetSystemPrompt`, `SetSkills`) regardless of whether a Prompt is in flight. Carries
  `ConfigUpdateCtx` with the changed `ConfigField` and old/new values. Go-shaped port
  of upstream's `model_update` / `thinking_level_update` / `tools_update` events
  (upstream 0.77.0; ADR-0007).
- **Active-tools registry** — registered vs. active tool distinction:
  `Agent.GetTools` returns all registered tools; `Agent.GetActiveTools` /
  `Agent.SetActiveTools` manage the active subset; `AgentConfig.ActiveToolNames` seeds
  the initial active set. Duplicate tool-name registration is rejected at construction.
  The active-set change is persisted as an `active_tools_change` session entry and
  replayed on resume. (upstream 0.77.0)

### Changed

- **Compaction summarization prompts** aligned with upstream 0.79.1's structured
  templates and neutral "AI assistant" wording (upstream 0.79.0). Prompt text now
  matches upstream exactly; callers using `UpdateSummarizationPrompt` see no API
  change.

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
- Prompt loop auto-continues after a tool-call turn instead of halting; matches the
  CONTEXT.md Prompt contract and upstream parity.
- Tool-batch wait is bounded by `ShutdownTimeout`; a tool that ignores ctx emits
  `ToolLeakEvent` and does not block `<-stream.Done` indefinitely. (ADR-0004
  deviation 4)
- Caller-context cancellation maps to `ErrPromptCancelled` on `PromptResult.Err`;
  `Agent.Stop()` maps to `ErrAgentStopped`. (ADR-0004 deviations 1–2)
