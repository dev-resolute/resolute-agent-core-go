# pi-core-agent-go — Glossary

## Agent Layer Terms

### Agent surface

**Agent**: Session-shaped, mutable object that owns tools, hooks, the session backend, and mutable runtime config (model, tools, system prompt, thinking level, skills). One Agent represents one conversation; at most one prompt is in flight at a time. Multi-tenant servers hold N Agents for N sessions. Control methods `Steer`, `FollowUp`, `Stop`, `State`, `Phase`, `Transcript`, `Close` live here (ADR-0006).

**Prompt**: `Agent.Prompt(ctx, msg, opts)` — starts an LLM call and returns an `EventStream`. Returns `ErrAgentBusy` when a prompt is already in flight. Replaces v0.1.x `Agent.Run`; the `*Run` handle is dissolved (ADR-0006).

**EventStream**: Struct returned by `Agent.Prompt`. Carries an `Events` channel (typed events, closed by the sender on completion) and a `Done` channel (one terminal `PromptResult`).

**PromptResult**: Terminal value delivered on `EventStream.Done`. Contains final message transcript and any error. Renamed from v0.1.x `RunResult`.

**Setters**: `SetModel`/`SetTools`/`SetSystemPrompt`/`SetThinkingLevel`/`SetSkills` mutate the Agent under its mutex; the next turn snapshot picks up the change, never the in-flight turn.

**Turn snapshot**: Immutable copy of the Agent's runtime config taken under a read lock at turn start. Setters during a turn affect the next snapshot, not the one in flight.

### Messages

**Message**: Agent-side unit of transcript content. Struct with `Role`, `Type` (discriminator), and untyped `Body json.RawMessage`.

**ConvertToLLM**: User-provided function called at the LLM-API boundary. Transforms agent transcript into provider-shaped payload.

### Tools

**Tool**: Generic over parameter struct type (`Tool[P]`). Framework unmarshals LLM arguments into `P` before calling `Execute`.

**ToolResult**: Concrete struct: `Content string`, `Data json.RawMessage`, `IsError bool`.

**Dynamic tool**: Escape hatch for runtime-schema tools via `NewDynamicTool`.

### Events

**AgentEvent**: Sealed interface for events on `EventStream.Events`. Concrete variants: `TextDeltaEvent`, `ToolCallStartEvent`, `ToolCallEndEvent`, `ToolErrorEvent`, `ThinkingDeltaEvent`, `TurnStartEvent`, `TurnEndEvent`, `ErrorEvent`, `LLMRetryEvent`, `ThinkingUnsupportedEvent`, `ToolLeakEvent`, `UserMessageEvent`, `SteerInjectedEvent`, `FollowUpInjectedEvent`, `CompactionStartEvent`, `CompactionEndEvent`.

### Hooks

**Hooks**: Flat struct of optional function fields. Nil fields are no-ops.

**Hook context structs**: Each hook receives a concrete per-hook context struct (`BeforeToolCallCtx`, `BeforeCompactCtx`, etc.).

### Session storage

**SessionRepo**: Interface for storage backends. Domain operations: create, append, load, list, append/load branch summaries, delete.

**SessionID**: Opaque string type.

**MemorySession**: Default in-process backend.

**JSONLSession**: On-disk JSONL backend, upstream-compatible.

**BranchSummary**: Compaction artifact replacing a message range with a summary.

### Compaction

**Compact**: `Agent.Compact(ctx, opts)` — manually invoked. Collapses older messages into a `BranchSummary`.

**Cut point**: Transcript index separating "summarize" from "keep verbatim".

### Cancellation

**Stop**: `Agent.Stop()` — fire-and-forget. Cancels the in-flight prompt's internal context with cause `ErrAgentStopped`.

**ShutdownTimeout**: Bound on waiting for tools to honor cancelled ctx. Default 30s.

**ToolLeakEvent**: Emitted when a tool fails to honor cancelled ctx within `ShutdownTimeout`.

### Concurrency

**Single-runner Agent**: One `Agent` corresponds to one session/conversation; at most one prompt in flight at a time. Runtime config is mutable via setters and picked up on the next turn snapshot. Concurrent users get N Agents, not one Agent shared across N runs. Reverses the v0.1.x "Multi-runner Agent" invariant (ADR-0006).

**Goroutine-safety contract**: `Tool.Execute`, hooks, `ConvertToLLM`, and `SessionRepo` implementations must be safe for concurrent invocation. Setters on `*Agent` are also concurrent-safe.
