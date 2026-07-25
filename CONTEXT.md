# pi-core-agent-go — Glossary

## Agent Layer Terms

### Agent surface

**Agent**: Session-shaped, mutable object that owns tools, hooks, the session backend, and mutable runtime config (model, tools, system prompt, thinking level, skills). One Agent represents one conversation; at most one prompt is in flight at a time. Multi-tenant servers hold N Agents for N sessions. Control methods `Steer`, `FollowUp`, `Stop`, `State`, `Phase`, `Transcript`, `Close` live here (ADR-0006).

**Prompt**: `Agent.Prompt(ctx, msg, opts)` — starts a prompt and returns an `EventStream`. Returns `ErrAgentBusy` when a prompt is already in flight. Replaces v0.1.x `Agent.Run`; the `*Run` handle is dissolved (ADR-0006). **Multi-turn contract:** a prompt spans one or more LLM turns until the model stops calling tools — a turn that calls tools auto-continues so the model sees its tool results on the next call. The loop is **uncapped by design** (parity with upstream pi); `ShouldStopAfterTurn` is the capping mechanism, alongside `ToolResult.Terminate`, `Stop()`, and caller-`ctx` cancellation.

**EventStream**: Struct returned by `Agent.Prompt`. Carries an `Events` channel (typed events, closed by the sender on completion) and a `Done` channel (one terminal `PromptResult`).

**PromptResult**: Terminal value delivered on `EventStream.Done`. Contains final message transcript and any error. Renamed from v0.1.x `RunResult`.

**Setters**: `SetModel`/`SetTools`/`SetSystemPrompt`/`SetThinkingLevel`/`SetSkills`/`SetActiveTools` mutate the Agent under its mutex; the next turn snapshot picks up the change, never the in-flight turn. `SetTools` and `SetActiveTools` return an error and leave the Agent unchanged on invalid input (see Registered vs active tools).

**Turn snapshot**: Immutable copy of the Agent's runtime config taken under a read lock at turn start. Setters during a turn affect the next snapshot, not the one in flight.

### Messages

**Message**: Agent-side unit of transcript content. Struct with `Role`, `Type` (discriminator), untyped `Body json.RawMessage`, and `Images []llm.ImageContent` (additive, v0.8.0) — image attachments kept outside `Body` so `Body` stays text-only and token estimation (`EstimateTokens`) can count images at a flat rate rather than parsing them out of the body. Populated by `NewToolResultMsg`, which copies a tool's `ToolResult.Images` through; the older `NewToolResult` constructor is unchanged and produces a `Message` with no `Images`. `DefaultConvertToLLM` threads `Images` onto the rebuilt `llm.ToolResultContent` so attachments reach the provider.

**ConvertToLLM**: User-provided function called at the LLM-API boundary. Transforms agent transcript into provider-shaped payload.

**Thought signature**: Opaque byte token Gemini 3 attaches to a tool call and requires back verbatim when the call is replayed — missing it rejects the whole request (`400 INVALID_ARGUMENT`), so the auto-continued tool turn would always fail on `gemini-3*` models. The prompt runner copies it from `llm.ToolCallStartEvent` into the persisted tool_call body (`thought_signature`, via `NewToolCallWithSignature`), and `DefaultConvertToLLM` replays it through `Message.ToolCallThoughtSignature()` onto the rebuilt `llm.ToolCallContent`. Nil when absent — pre-existing transcripts and non-Gemini providers are unaffected; custom `ConvertToLLM` implementations targeting Gemini 3 must replay it the same way.

### Tools

**Tool**: Generic over parameter struct type (`Tool[P]`). Framework unmarshals LLM arguments into `P` before calling `Execute`.

**ToolResult**: Concrete struct: `Content string`, `Data json.RawMessage`, `IsError bool`.

**Dynamic tool**: Escape hatch for runtime-schema tools via `NewDynamicTool`.

**Tool update**: An ephemeral partial `ToolResult` snapshot emitted by a tool built from
`Tool[P].ExecuteStream` (via its `emit` callback) while it is still running — e.g. `bash`'s
throttled partial output. Delivered to the prompt loop as a `ToolUpdateEvent` on
`EventStream.Events`. **Never persisted**: only the tool's final `ToolResult` is written to the
transcript; a tool update exists only for the duration of the in-flight prompt and is not
recoverable from session storage or replay. A tool built from `ExecuteStream` still satisfies plain
`Tool.Execute` for callers that don't care about partial updates — `emit` is a silent no-op on that
path.

**Built-in tools**: The `tools` subpackage
(`github.com/dev-resolute/resolute-agent-core-go/tools`, v0.8.0) ships four model-facing tools
ported from upstream pi @0.82.0 — `read`, `write`, `edit`, `bash` — over the `ExecutionEnv` seam.
Core never imports it (same shape as `piskills`): importing core alone pulls in no
filesystem/subprocess code. See ADR-0011 for the seam's design.

**ExecutionEnv**: The filesystem/shell seam built-in tools run over (`tools.ExecutionEnv`,
ADR-0011) — ctx-first methods (`ReadFile`, `WriteFile`, `AppendFile`, `CreateTemp`, `FileInfo`,
`Exists`, `AbsolutePath`, `CanonicalPath`, `Exec`, ...) returning plain Go `(T, error)`, not
upstream's `Result[T, E]`. `OSEnv` is the local-process implementation (real files, real
subprocesses via `bash -c` in its own process group so timeout/cancellation can kill the whole
process tree). Implementations MUST be pointer types — the mutation queue keys on instance
identity. Sandbox/remote adapters plug in at this seam; there is no per-call context parameter
(upstream's arbitrary `TContext`) — a tool closes over its `ExecutionEnv` at construction time,
matching how every other tool in this port is built.

**Mutation queue**: Serializes concurrent `write`/`edit` tool calls that target the same underlying
file (`tools.withFileMutationQueue`, keyed by `{ExecutionEnv, canonical path}` — the same file
reached via a symlink and via its real path shares one key) so a read-modify-write tool
implementation never races itself; calls against different files, or the same path through
different `ExecutionEnv` instances, proceed fully concurrently. The lock is held for the duration
of the call and is **not** cancellable through `ctx` once acquired — only key resolution
(`AbsolutePath`/`CanonicalPath`) is ctx-aware.

**Registered vs active tools**: *Registered* tools are every tool on the Agent (`AgentConfig.Tools` / `SetTools`). *Active* tools are the subset offered to the model on a turn (`AgentConfig.ActiveToolNames` / `SetActiveTools`); a nil active set means all registered tools are active. The turn snapshot carries only the active subset, so an inactive tool is never offered to the model nor executed. Registered names must be unique and active names must reference registered tools without duplicates — validated by one shared helper at construction, `SetTools`, and `SetActiveTools` (`ErrDuplicateToolName`, `ErrUnknownActiveTool`).

**active_tools_change**: Bookkeeping transcript `Message` (`Type: "active_tools_change"`, `Body: {"activeToolNames":[...]}`) recording a change to the active set. It is never sent to the model (excluded by `BuildLLMContext` and `DefaultConvertToLLM`) and is never a compaction cut point. On resume, the active set is restored by scanning for the last such entry (absent ⇒ all tools active). The empty-vs-nil distinction is load-bearing and preserved end-to-end: a recorded empty set (`[]`) means *no* tools active and resumes as such, whereas a recorded nil (`null`) or an absent entry means *all* tools active — so the bind-time record must keep an empty set empty, not collapse it to nil. Restored names are validated lazily, not on restore: a name the current registry no longer registers (the tool set may differ between runs) is silently dropped by `filterActiveTools` at snapshot time, and if every restored name is stale the model is offered zero tools. When a session is bound, `SetActiveTools` persists immediately (idle) or via a deferral queue flushed at the turn-end safe point (mid-prompt); the queue is also drained on every prompt-exit path (success, error, cancellation) so no entry is stranded or leaked into a later prompt's session. Before the first prompt nothing is written, and the active set is recorded at session-bind time if it differs from the full registered set.

### Skills

**Skill**: A unit of model-invokable expertise carried on the Agent (`Name`, `Description`, `Content`, `FilePath`, `DisableModelInvocation`). Part of the mutable runtime config (`AgentConfig.Skills` / `Agent.SetSkills`) and the turn snapshot.

**Skill index**: The model-visible XML block (`<available_skills>` of `<skill>` entries carrying `<name>`/`<description>`/`<location>` — never `Content`) rendered by `formatSkillsForSystemPrompt`. Skills with `DisableModelInvocation` are excluded; with no model-visible skills it renders the empty string. It is auto-attached to the effective system prompt **per turn** (in the derived `[]llm.Message`, not the persisted transcript), so `SetSkills` is reflected on the next turn and the index never leaks into session storage.

**Content-reader contract**: The framework ships no tool that reads a skill's `FilePath`; the index exposes only name/description/location, and the model fetches a skill's full instructions on demand through a user-supplied tool that resolves `FilePath`.

**piskills**: Opt-in subpackage (`github.com/dev-resolute/resolute-agent-core-go/piskills`) whose `Load(dir)` walks `SKILL.md` files, parses frontmatter (`name`, `description`, `disable-model-invocation`), honors `.gitignore`/`.ignore`, and returns skills plus `Diagnostic`s for malformed entries. A directory with a `SKILL.md` is a skill leaf (not descended into). Core never imports it, so importing core pulls in no filesystem/skill-loading code.

### Events

**AgentEvent**: Sealed interface for events on `EventStream.Events`. Concrete variants: `TextDeltaEvent`, `ToolCallStartEvent`, `ToolCallEndEvent`, `ToolUpdateEvent` (see Tool update, v0.8.0), `ToolErrorEvent`, `ThinkingDeltaEvent`, `TurnStartEvent`, `TurnEndEvent`, `ErrorEvent`, `LLMRetryEvent`, `ThinkingUnsupportedEvent`, `ToolLeakEvent`, `UserMessageEvent`, `SteerInjectedEvent`, `FollowUpInjectedEvent`, `CompactionStartEvent`, `CompactionEndEvent`.

### Hooks

**Hooks**: Flat struct of optional function fields. Nil fields are no-ops.

**Hook context structs**: Each hook receives a concrete per-hook context struct (`BeforeToolCallCtx`, `BeforeCompactCtx`, etc.).

**OnSummarizationRetry**:
Optional `Hooks` field fired at each retry-lifecycle point (scheduled, attempt-start, finished) when a Compact summarization call fails transiently and `AgentConfig.SummarizationRetry` allows a retry. May fire concurrently from split-turn summarization's two goroutines. The Go-shaped equivalent of upstream 0.81.1's `summarization_retry_*` events — Compact has no EventStream, so a hook is the delivery path.
_Avoid_: SummarizationRetryEvent (ours is a hook, not an AgentEvent)

### Session storage

**SessionRepo**: Interface for storage backends. Domain operations: create, append, append active-tools change, load, list, append/load branch summaries, delete.

**SessionID**: Opaque string type.

**MemorySession**: Default in-process backend.

**JSONLSession**: On-disk session backend. The format is **Go-native, append-only JSONL**: one line per entry, each the Go `Message` codec shape (flat `{"Role","Type","Body"}`). This is **not** wire-compatible with upstream's `{type,id,parentId,timestamp}` tree today; cross-runtime interchange is tracked as future work (a separate issue), not a current guarantee. Session-format migration is explicitly out of scope here.

**BranchSummary**: Compaction artifact replacing a message range with a summary.

### Compaction

**Compact**: `Agent.Compact(ctx, opts)` — manually invoked. Collapses older messages into a `BranchSummary`.

**Cut point**: Transcript index separating "summarize" from "keep verbatim".

**SummarizationRetryPolicy**:
`AgentConfig` field configuring bounded retries with exponential backoff (`BaseDelay * 2^(attempt-1)`, capped at `MaxDelay`) for Compact's summarization calls. Zero value disables retries. Ported from upstream 0.81.1.
_Avoid_: RetryConfig, CompactionRetry

### Cancellation

**Stop**: `Agent.Stop()` — fire-and-forget. Cancels the in-flight prompt's internal context with cause `ErrAgentStopped`.

**Agent.Context**: `Agent.Context()` — returns the in-flight prompt's context. Cancellation via the caller's `ctx` or `Stop()` is observable through it, making it safe to anchor nested goroutines or sub-operations that must not outlive the prompt. When no prompt is in flight (idle, never started, or after completion), returns a pre-cancelled context with cause `ErrNoPromptInFlight`; any stale work tied to that context exits immediately rather than leaking into the next prompt.

**ShutdownTimeout**: Bound on waiting for tools to honor cancelled ctx. Default 30s.

**ToolLeakEvent**: Emitted when a tool fails to honor cancelled ctx within `ShutdownTimeout`.

### Concurrency

**Single-runner Agent**: One `Agent` corresponds to one session/conversation; at most one prompt in flight at a time. Runtime config is mutable via setters and picked up on the next turn snapshot. Concurrent users get N Agents, not one Agent shared across N runs. Reverses the v0.1.x "Multi-runner Agent" invariant (ADR-0006).

**Goroutine-safety contract**: `Tool.Execute`, hooks, `ConvertToLLM`, and `SessionRepo` implementations must be safe for concurrent invocation. Setters on `*Agent` are also concurrent-safe. `OnConfigUpdate` fires after the setter releases the Agent mutex, so the hook may safely call getters; however, it may observe a newer Agent state than the captured `ConfigUpdateCtx.Old*`/`New*` values if another setter races in between.
