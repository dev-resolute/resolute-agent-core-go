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

**Message**: Agent-side unit of transcript content. Struct with `Role`, `Type` (discriminator), and untyped `Body json.RawMessage`.

**ConvertToLLM**: User-provided function called at the LLM-API boundary. Transforms agent transcript into provider-shaped payload.

### Tools

**Tool**: Generic over parameter struct type (`Tool[P]`). Framework unmarshals LLM arguments into `P` before calling `Execute`.

**ToolResult**: Concrete struct: `Content string`, `Data json.RawMessage`, `IsError bool`.

**Dynamic tool**: Escape hatch for runtime-schema tools via `NewDynamicTool`.

**Registered vs active tools**: *Registered* tools are every tool on the Agent (`AgentConfig.Tools` / `SetTools`). *Active* tools are the subset offered to the model on a turn (`AgentConfig.ActiveToolNames` / `SetActiveTools`); a nil active set means all registered tools are active. The turn snapshot carries only the active subset, so an inactive tool is never offered to the model nor executed. Registered names must be unique and active names must reference registered tools without duplicates — validated by one shared helper at construction, `SetTools`, and `SetActiveTools` (`ErrDuplicateToolName`, `ErrUnknownActiveTool`).

**active_tools_change**: Bookkeeping transcript `Message` (`Type: "active_tools_change"`, `Body: {"activeToolNames":[...]}`) recording a change to the active set. It is never sent to the model (excluded by `BuildLLMContext` and `DefaultConvertToLLM`) and is never a compaction cut point. On resume, the active set is restored by scanning for the last such entry (absent ⇒ all tools active). The empty-vs-nil distinction is load-bearing and preserved end-to-end: a recorded empty set (`[]`) means *no* tools active and resumes as such, whereas a recorded nil (`null`) or an absent entry means *all* tools active — so the bind-time record must keep an empty set empty, not collapse it to nil. Restored names are validated lazily, not on restore: a name the current registry no longer registers (the tool set may differ between runs) is silently dropped by `filterActiveTools` at snapshot time, and if every restored name is stale the model is offered zero tools. When a session is bound, `SetActiveTools` persists immediately (idle) or via a deferral queue flushed at the turn-end safe point (mid-prompt); the queue is also drained on every prompt-exit path (success, error, cancellation) so no entry is stranded or leaked into a later prompt's session. Before the first prompt nothing is written, and the active set is recorded at session-bind time if it differs from the full registered set.

### Skills

**Skill**: A unit of model-invokable expertise carried on the Agent (`Name`, `Description`, `Content`, `FilePath`, `DisableModelInvocation`). Part of the mutable runtime config (`AgentConfig.Skills` / `Agent.SetSkills`) and the turn snapshot.

**Skill index**: The model-visible XML block (`<available_skills>` of `<skill>` entries carrying `<name>`/`<description>`/`<location>` — never `Content`) rendered by `formatSkillsForSystemPrompt`. Skills with `DisableModelInvocation` are excluded; with no model-visible skills it renders the empty string. It is auto-attached to the effective system prompt **per turn** (in the derived `[]llm.Message`, not the persisted transcript), so `SetSkills` is reflected on the next turn and the index never leaks into session storage.

**Content-reader contract**: The framework ships no tool that reads a skill's `FilePath`; the index exposes only name/description/location, and the model fetches a skill's full instructions on demand through a user-supplied tool that resolves `FilePath`.

**piskills**: Opt-in subpackage (`github.com/dev-resolute/resolute-agent-core-go/piskills`) whose `Load(dir)` walks `SKILL.md` files, parses frontmatter (`name`, `description`, `disable-model-invocation`), honors `.gitignore`/`.ignore`, and returns skills plus `Diagnostic`s for malformed entries. A directory with a `SKILL.md` is a skill leaf (not descended into). Core never imports it, so importing core pulls in no filesystem/skill-loading code.

### Events

**AgentEvent**: Sealed interface for events on `EventStream.Events`. Concrete variants: `TextDeltaEvent`, `ToolCallStartEvent`, `ToolCallEndEvent`, `ToolErrorEvent`, `ThinkingDeltaEvent`, `TurnStartEvent`, `TurnEndEvent`, `ErrorEvent`, `LLMRetryEvent`, `ThinkingUnsupportedEvent`, `ToolLeakEvent`, `UserMessageEvent`, `SteerInjectedEvent`, `FollowUpInjectedEvent`, `CompactionStartEvent`, `CompactionEndEvent`.

### Hooks

**Hooks**: Flat struct of optional function fields. Nil fields are no-ops.

**Hook context structs**: Each hook receives a concrete per-hook context struct (`BeforeToolCallCtx`, `BeforeCompactCtx`, etc.).

### Session storage

**SessionRepo**: Interface for storage backends. Domain operations: create, append, append active-tools change, load, list, append/load branch summaries, delete.

**SessionID**: Opaque string type.

**MemorySession**: Default in-process backend.

**JSONLSession**: On-disk session backend. The format is **Go-native, append-only JSONL**: one line per entry, each the Go `Message` codec shape (flat `{"Role","Type","Body"}`). This is **not** wire-compatible with upstream's `{type,id,parentId,timestamp}` tree today; cross-runtime interchange is tracked as future work (a separate issue), not a current guarantee. Session-format migration is explicitly out of scope here.

**BranchSummary**: Compaction artifact replacing a message range with a summary.

### Compaction

**Compact**: `Agent.Compact(ctx, opts)` — manually invoked. Collapses older messages into a `BranchSummary`.

**Cut point**: Transcript index separating "summarize" from "keep verbatim".

### Cancellation

**Stop**: `Agent.Stop()` — fire-and-forget. Cancels the in-flight prompt's internal context with cause `ErrAgentStopped`.

**Agent.Context**: `Agent.Context()` — returns the in-flight prompt's context. Cancellation via the caller's `ctx` or `Stop()` is observable through it, making it safe to anchor nested goroutines or sub-operations that must not outlive the prompt. When no prompt is in flight (idle, never started, or after completion), returns a pre-cancelled context with cause `ErrNoPromptInFlight`; any stale work tied to that context exits immediately rather than leaking into the next prompt.

**ShutdownTimeout**: Bound on waiting for tools to honor cancelled ctx. Default 30s.

**ToolLeakEvent**: Emitted when a tool fails to honor cancelled ctx within `ShutdownTimeout`.

### Concurrency

**Single-runner Agent**: One `Agent` corresponds to one session/conversation; at most one prompt in flight at a time. Runtime config is mutable via setters and picked up on the next turn snapshot. Concurrent users get N Agents, not one Agent shared across N runs. Reverses the v0.1.x "Multi-runner Agent" invariant (ADR-0006).

**Goroutine-safety contract**: `Tool.Execute`, hooks, `ConvertToLLM`, and `SessionRepo` implementations must be safe for concurrent invocation. Setters on `*Agent` are also concurrent-safe. `OnConfigUpdate` fires after the setter releases the Agent mutex, so the hook may safely call getters; however, it may observe a newer Agent state than the captured `ConfigUpdateCtx.Old*`/`New*` values if another setter races in between.
