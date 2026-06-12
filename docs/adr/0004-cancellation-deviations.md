# 4. Cancellation model deviates from Pi in 5 specific places where Go requires it

Date: 2026-05-25
Status: Accepted

## Context

Upstream Pi's cancellation surface is built around a single `abort()` method, an `AbortController` propagated to tool execution and LLM streams, and a string error `"Operation aborted"`. There is no shutdown timeout, no graceful drain, and no caller-supplied cancel trigger (TypeScript has no equivalent of `context.Context`). If a tool ignores the abort signal, the agent harness hangs indefinitely.

We considered matching Pi exactly versus designing a Go-idiomatic cancellation model. The five places we deviate are deliberate and individually justified.

## Decision

Adopt a Go-idiomatic cancellation model that matches Pi conceptually but takes 5 specific deviations (plus a sixth, narrower deviation — item 6 — added 2026-06-12):

1. **Caller `ctx` is a first-class cancel trigger** alongside `Agent.Stop()`. Both paths cancel the prompt's internal `context.WithCancelCause`; the cause distinguishes them. (Pi has only `abort()` because JS has no parent-ctx primitive.)

2. **Typed sentinel errors** (`ErrPromptCancelled`, `ErrAgentStopped`, `ErrToolLeaked`, `ErrNoPromptInFlight`) instead of Pi's string `"Operation aborted"`. Required by ERR-2 and ERR-3.

3. **Explicit session-write drain** before final close. Pi's JS event loop guarantees this implicitly; Go has no such guarantee, and a session write goroutine cancelled mid-flush leaves a corrupt JSONL file.

4. **`ShutdownTimeout` + `ToolLeakEvent`** for buggy tools. Pi hangs forever if a tool ignores the signal. In Go this is silent because the caller is waiting on `<-stream.Done`, which never fires. The timeout doesn't kill the leaked goroutine (Go can't) — it bounds time-to-`Done` and surfaces the leak observably.

5. **Tool ctx is a MUST contract**, not Pi's optional `signal?` parameter. Go convention is `ctx` as the first parameter to any long-running call; making it optional would be unidiomatic.

6. **Struct-stored `ctx` for the exposure-only lifecycle handle** (added 2026-06-12, AGENT-5). `promptRun.ctx` holds the inner prompt context so `Agent.Context()` can return it. This is a narrow, deliberate exception to CTX-1 ("never store ctx in structs"): the field is an exposure-only handle for a lifecycle accessor, not a context threaded through call arguments. Every function still takes `ctx` as its first parameter, and `loop()` receives the inner context explicitly. Precedent: the standard library's `net/http.Request` stores its context for `Request.Context()` for exactly this reason — exposing a request/operation-scoped lifecycle to handlers without rethreading it.

## Why

- Deviations 1, 2, and 5 are forced by Go's language model and your own rules — we have no choice.
- Deviation 3 prevents real corruption that Pi avoids accidentally via JS semantics.
- Deviation 4 is the only one with a true alternative (match Pi, hang on buggy tools, document it). We chose the timeout + observable leak because callers waiting on `<-Done` deserve bounded time-to-completion, and `ToolLeakEvent` is the only way to alert on this class of bug in production.

## Consequences

- **`Agent.Stop()` is fire-and-forget.** Caller observes `<-stream.Done` to know when shutdown completes. Stop is idempotent.
- **Cancel cause is the authoritative discriminator.** Callers use `errors.Is(result.Err, ErrAgentStopped)` etc. on `PromptResult.Err`, which holds `context.Cause(internalCtx)`.
- **Tool authors must respect ctx.** Documented as a MUST in the Tool godoc; reviewers enforce. The framework defends against ignorance via `ShutdownTimeout`, but a leaked tool is the author's bug.
- **`ShutdownTimeout` defaults to 30s.** Configurable on `AgentConfig`. Tunable down for tests, up for long-cleanup tools (database transactions, etc.).
- **`ToolLeakEvent` is observable but non-fatal.** The leaked goroutine continues to run; the framework just gives up waiting for it. Production users should alert on this event.
- **Session writes drain.** Bounded set, fast; the only synchronous part of shutdown.
- **`promptRun.ctx` is set once and read-only after launch.** It is assigned in `Agent.Prompt` before the loop goroutine starts and never reassigned; `Agent.Context()` and `Stop()` read it under `cancelMu`. The inner context is cancelled on every loop exit — a clean completion adopts `ErrNoPromptInFlight`, while `Stop()` (`ErrAgentStopped`) and caller cancellation (`ErrPromptCancelled`) are preserved by first-cause-wins — so `Agent.Context()` never hands out a permanently-live context for a finished prompt, and the inner context is unregistered from its cancellable parent rather than leaking.
