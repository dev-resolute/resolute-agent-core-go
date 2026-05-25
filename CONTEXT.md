# pi-core-agent-go — Glossary

## Agent Layer Terms

### Agent surface

**Agent**: Persistent, configurable object that owns tools, hooks, session backend, default options, and system prompt. Long-lived; can spawn multiple concurrent runs.

**Run**: Handle returned by `Agent.Run(...)`. Represents a single live invocation. Owns `Events` and `Done` channels and control methods (`Steer`, `FollowUp`, `Stop`).

**RunResult**: Terminal value delivered on `Run.Done`. Contains final message transcript and any error.

### Messages

**Message**: Agent-side unit of transcript content. Struct with `Role`, `Type` (discriminator), and untyped `Body json.RawMessage`.

**ConvertToLLM**: User-provided function called at the LLM-API boundary. Transforms agent transcript into provider-shaped payload.

### Tools

**Tool**: Generic over parameter struct type (`Tool[P]`). Framework unmarshals LLM arguments into `P` before calling `Execute`.

**ToolResult**: Concrete struct: `Content string`, `Data json.RawMessage`, `IsError bool`.

**Dynamic tool**: Escape hatch for runtime-schema tools via `NewDynamicTool`.

### Events

**AgentEvent**: Sealed interface for events on `Run.Events`. Concrete variants: `TextDeltaEvent`, `ToolCallStartEvent`, `ToolCallEndEvent`, `ToolErrorEvent`, `ThinkingDeltaEvent`, `TurnStartEvent`, `TurnEndEvent`, `ErrorEvent`, `LLMRetryEvent`, `ThinkingUnsupportedEvent`, `ToolLeakEvent`, `UserMessageEvent`, `SteerInjectedEvent`, `FollowUpInjectedEvent`, `CompactionStartEvent`, `CompactionEndEvent`.

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

**Stop**: `Run.Stop()` — fire-and-forget. Cancels internal context with cause `ErrRunStopped`.

**ShutdownTimeout**: Bound on waiting for tools to honor cancelled ctx. Default 30s.

**ToolLeakEvent**: Emitted when a tool fails to honor cancelled ctx within `ShutdownTimeout`.

### Concurrency

**Multi-runner Agent**: `Agent.Run` is goroutine-safe. Agent is immutable after `NewAgent`.

**Goroutine-safety contract**: `Tool.Execute`, hooks, `ConvertToLLM`, and `SessionRepo` implementations must be safe for concurrent invocation.
