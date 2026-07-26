# Changelog

## [0.9.0] - 2026-07-25

### Breaking (custom `llm.LLMProvider` test doubles only)

- **Tool calls are now collected from the finalized `llm.ToolCallEndEvent`, not
  `llm.ToolCallStartEvent` (AGENT-22, upstream #6285).** openai-compat-shaped providers stream
  tool-call arguments incrementally and only finalize them on the end event; collecting at the
  start event executed tools with nil args and durably recorded nil args in harness logs. **Any
  custom `llm.LLMProvider` test double that emits `ToolCallStartEvent` with the call's args and a
  bare `ToolCallEndEvent{CallID: ...}` (no `ToolName`/`Args`) must be updated to put the finalized
  `ToolName` and `Args` on the end event** — the agent no longer reads them off the start event.
  Real providers are unaffected; this only breaks fixtures/test doubles that modeled the older
  event contract.

### Changed

- **`resolute-llm-go` v0.9.0 → v0.10.0.** Paired dependency bump required by the
  `ToolCallEndEvent`-sourced tool-call collection above.
- **Split-turn summarization calls are now serialized (AGENT-19, upstream #5536).** The history-prefix
  and turn-prefix summarization calls that a mid-turn compaction cut point requires ran concurrently
  since v0.7.0; they now run strictly in sequence — history first, short-circuiting before the
  turn-prefix call is ever issued if history summarization fails — because single-concurrency
  providers must not see overlapping generations and `OnSummarizationRetry` lifecycle events need to
  stay ordered. `OnSummarizationRetry` can no longer fire concurrently from the split-turn path's two
  goroutines (there's only one goroutine now).
- **Split-turn summary join now uses upstream's template (model-facing change).** The history and
  turn-prefix summaries are joined with `"\n\n---\n\n**Turn Context (split turn):**\n\n"` instead of
  a bare `"\n"`, matching upstream's split-turn summary format. Any consumer that parses or displays
  split-turn summary text will see the new separator.

### Added

- **`Usage{InputTokens, OutputTokens}` and `BranchSummary.Usage *Usage` (AGENT-20, upstream #6671).**
  A new exported `Usage` type carries provider token-usage totals; `Compact` now attaches the
  summarization call's usage to the persisted `BranchSummary` (and to `CompactResult`/`AfterCompactCtx`),
  summing both calls for a split-turn summary via `combineUsage`. `Usage` is nil when the provider
  reports no usage at all — it does not default to a zero-valued struct. The JSONL session backend
  round-trips `BranchSummary.Usage` and is backward-compatible with pre-v0.9.0 summary lines that have
  no `Usage` key at all (they load with `Usage == nil`).
- **Length-truncated assistant messages' tool calls now fail instead of executing (AGENT-22, upstream
  #6285).** When an assistant message's `MessageEndEvent.StopReason` is `llm.StopReasonLength`, every
  tool call it carries may have truncated arguments (streamed arguments can parse yet still be
  incomplete), so none are executed. Each gets a synthesized error `ToolResult` instead, with
  upstream's exact message: `Tool call "<name>" was not executed: the response hit the output token
  limit, so its arguments may be truncated. Re-issue the tool call with complete arguments.` The loop
  continues so the model can re-issue the call with complete arguments.
- **Regression test pinning summarization requests carry no `SessionID` (AGENT-21, upstream #6618
  parity).** `summarizeOnce`'s LLM request has always omitted `SessionID` (keeping summarization
  cache- and affinity-isolated from the turn, matching upstream's fresh-routing-id-per-call
  behavior); `TestSummarizationRequestsCarryNoSessionID` now pins that both the single-call and
  split-turn summarization paths never leak the turn's `SessionID` into a summarization request, no
  behavior change.

### Fixed

- **`bashThrottle` gained a settled guard, closing the ADR-0011 follow-up.** `tools/bash.go`'s
  `bashThrottle` now drops any `OnChunk` callback that arrives after execution has settled
  (`finalFlush`/`stop`), mirroring upstream's `acceptingOutput` flag (`shell-output.ts`) and porting
  upstream's "ignores output callbacks after execution settles" test case, which the AGENT-18 R2
  sweep deliberately skipped. Closes the gap recorded in ADR-0011's Consequences section: no shipped
  `ExecutionEnv` (`OSEnv`) can trigger the race structurally, but a future adapter that delivers
  `OnChunk` asynchronously after `Exec` returns no longer leaks a further emit or leaves a throttle
  timer running past `ExecuteStream`'s return.

## [0.8.0] - 2026-07-25

### Added

- **`tools` package: four built-in tools + the `ExecutionEnv` seam (AGENT-18 R2, ported from
  upstream pi @0.82.0).** New subpackage `github.com/dev-resolute/resolute-agent-core-go/tools`
  ships `read`, `write`, `edit`, and `bash` — the same model-facing tools upstream ships — over a
  new `ExecutionEnv` interface (`tools/env.go`, `OSEnv` the local-process implementation) that
  built-in-tool authors and sandbox/remote adapters both plug into. See ADR-0011 for the seam's
  design (closure-captured, ctx-first, Go errors over `Result[T]`, pointer-identity mutation-queue
  keys, POSIX-only). Model-facing strings (tool descriptions, error messages, truncation notices)
  are byte-for-byte identical to upstream's, verified against
  `packages/agent/src/harness/tools/*.ts` and pinned by dedicated schema/string tests per tool.
  Highlights:
  - **`read`**: offset/limit paging with continuation notices, line/byte truncation, and image
    detection by content (magic-byte sniffing, not extension) with an injectable
    `ReadToolOptions.ImageProcessor` for format conversion/resizing.
  - **`write`**: creates parent directories automatically; routes through the mutation queue.
  - **`edit`**: fuzzy multi-block replacement (`edits[]`), unified-diff + display-diff details,
    BOM/CRLF-preserving I/O, a legacy `oldText`/`newText` argument shim, and symlink-aware
    same-file serialization via the mutation queue.
  - **`bash`**: streaming output with throttled partial updates, timeout/cancellation/non-zero-exit
    status text, and full-output spill-to-temp-file when output is truncated — surviving even a
    timeout (the spilled file's path is still reported in the error).
  - **Mutation queue** (`tools/mutation_queue.go`): serializes concurrent `write`/`edit` calls that
    target the same underlying file (by canonical, symlink-resolved path) so read-modify-write tool
    implementations never race themselves; calls against different files proceed fully concurrently.
- **`Tool[P].ExecuteStream` + `ToolUpdateEvent`: a streaming seam for tools that emit partial
  results.** A tool may now set `ExecuteStream` instead of `Execute` to call `emit(ToolResult)` zero
  or more times before returning its final result; each emitted partial result is delivered to the
  prompt loop as an ephemeral `ToolUpdateEvent` on `EventStream.Events` — **never persisted** to the
  transcript (only the final `ToolResult` is). A tool built from `Tool.ExecuteStream` still satisfies
  plain `Execute` for callers that don't care about partial updates (`emit` is a silent no-op on that
  path). `bash` is the first tool to use this.
- **`ToolResult.Images` / `Message.Images` (+ `NewToolResultMsg`).** `ToolResult` gains an `Images
  []llm.ImageContent` field so a tool (e.g. `read`, on an image file) can return image attachments
  alongside text `Content`. `Message` gains the matching `Images` field, populated by the new
  `NewToolResultMsg(callID, toolName string, result ToolResult) Message` constructor, which copies
  `result.Images` through — additive; the existing `NewToolResult` constructor is unchanged and
  still produces a `Message` with no `Images`. `DefaultConvertToLLM` threads `Message.Images` onto
  the rebuilt `llm.ToolResultContent` so images reach the provider.
- **`EstimateTokens` counts images at a flat 4800 chars each** (upstream 0.76.0 attachment
  heuristic), applied to both the batch estimator and the per-message accounting used by
  compaction's cut-point search — conservative per ADR-0003, and now exercised by real image-bearing
  messages via the `read` tool rather than only a synthetic fixture.

### Changed

- **`resolute-llm-go` v0.8.2 → v0.9.0.** Paired dependency bump: adds `llm.ImageContent`, the type
  `ToolResult.Images`/`Message.Images` carry and `DefaultConvertToLLM` threads onto
  `llm.ToolResultContent`.

### Fixed / Subsumed

- **AGENT-14** (image content → attachment token accounting) and **AGENT-15** (truncate-tail port)
  are fully subsumed by this release's `tools` package and `EstimateTokens` image accounting; both
  issues are closed as absorbed by AGENT-18 R2 (see
  `docs/prds/agent18-builtin-tools-images-streaming.md`).

### Deliberate deviations from upstream (documented inline, not gaps)

- **`readParams.Limit`/`bashParams.Timeout` are pointers (`*int`/`*float64`), not plain values,**
  specifically so an explicit zero (`limit: 0`, `timeout: 0`) can be told apart from an omitted
  field and validated the way upstream's `!== undefined` checks do — `limit: 0` selects a genuine
  zero-line window (not "read to EOF"); `timeout: 0` is rejected exactly as upstream rejects it
  (`timeout <= 0`), rather than silently collapsing to "no timeout enforced". See `read.go`/`bash.go`
  package comments and `TestReadToolExplicitZeroLimitIsNotOmitted`/
  `TestBashToolExplicitZeroTimeoutIsRejected`.
- **`read`'s negative-`limit` clamp does not reproduce upstream's negative-index slice
  reinterpretation.** For a negative `limit`, upstream's raw (unclamped) arithmetic can produce a
  nonsensical negative `nextOffset` in its continuation notice (verified against upstream's actual
  JS semantics — see `read.go`'s `TestReadToolNegativeLimitClampsSafely`). Go has no equivalent
  negative-index slice reinterpretation (an unclamped range panics), so this port clamps `endLine`
  to `startLine` instead, both to avoid crashing and to avoid propagating a nonsensical offset a
  caller could never use. This is this port's own considered output for a degenerate input, not a
  port of upstream's.

## [0.7.0] - 2026-07-21

### Added

- **Resilient summarization (port of upstream 0.81.1, pi#6901).** Compact's
  summarization calls (first summary, summary update, and the split-turn pair)
  now retry transient provider failures per the new
  `AgentConfig.SummarizationRetry` policy — bounded attempts with exponential
  backoff (`BaseDelay * 2^(attempt-1)`, capped at `MaxDelay`). Fatal provider
  errors (`llm.ErrProviderFatal`), context overflow (`llm.ErrContextOverflow`),
  and caller cancellation fail fast without retries. The zero policy disables
  retries, matching pre-0.7.0 behavior. Retry lifecycle is reported through
  the new `Hooks.OnSummarizationRetry` hook (scheduled / attempt-start /
  finished phases) — the Go-shaped equivalent of upstream's
  `summarization_retry_*` events; Compact has no EventStream, so a hook is the
  delivery path (ADR-0007 precedent). The hook may fire concurrently from the
  split-turn path's two goroutines.

## [0.6.3] - 2026-07-04

### Added

- **`ToolCallStartEvent.ThoughtSignature`** (additive): the agent-level tool-call event now
  carries the provider's opaque thought signature (Gemini 3), copied from the underlying
  `llm.ToolCallStartEvent`. Event consumers that persist tool calls outside the transcript —
  resolute-harness-go authors durable `assistant_tool_call` records from this event — previously
  had no way to capture the signature, so mid-turn crash recovery replayed signature-less tool
  calls that Gemini 3 rejects with `400 INVALID_ARGUMENT` (HARNESS-11). Nil for providers
  without signatures.

## [0.6.2] - 2026-07-04

### Fixed

- **Summarization instruction now follows the transcript it references (AGENT-17).** All three
  summarization paths (first summary, update of an existing summary, split-turn history + turn
  prefix) assembled the request as `[system, instruction, ...transcript]` while the instruction
  wording says "the messages above" — real models saw an ordinary conversation as the last
  message and continued it instead of emitting the structured checkpoint, silently losing facts
  from the folded span. Requests are now `[system, ...transcript, instruction]`, matching the
  upstream TS implementation the prompts were written for. Prompt-shape regression tests pin the
  ordering per path.

## [0.6.1] - 2026-07-04

### Added

- **`CompactOpts.SessionID`** (additive): selects the session to compact explicitly.
  Empty keeps the existing behavior (the session bound by the most recent prompt).
  A fresh `Agent` that has never prompted previously had no way to compact a
  pre-existing session -- `Compact` silently no-oped; harnesses that materialize a
  fresh `Agent` per operation over a durable session store (resolute-harness-go's
  manual `Compact` operation) need to name the session directly.

## [0.6.0] - 2026-07-01

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
