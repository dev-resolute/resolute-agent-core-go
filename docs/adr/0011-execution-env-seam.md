# ADR-0011 — Execution environment seam: closure-captured, Go-shaped `ExecutionEnv`

**Status:** Accepted (2026-07-25)
**Context repos:** `resolute-agent-core-go`
**Relates to:** ADR-0004 (cancellation model deviations — ctx-first methods, Go errors over thrown
exceptions); extends that decision from "tool execution generally" to "the filesystem/shell seam
built-in tools execute against".

## Context

Upstream pi @0.82.0 matured `ExecutionEnv` (`packages/agent/src/harness/env/*.ts`) and a per-call
`ExecutionToolContext` (`tool-context.ts`) as the seam its built-in tools (read/write/edit/bash) run
through: a `NodeExecutionEnv` implementing async filesystem + shell methods that return
`Result<T, E>` (a discriminated-union `Result`/`ok`/`err` helper, not thrown exceptions), each
tool's `execute(callId, params, signal, onUpdate, context)` receiving `context.env` plus arbitrary
caller-supplied per-call context fields (e.g. `workspace`), and an `AbortSignal` threaded per call
for cancellation.

Porting the four built-in tools (AGENT-18 R2) required designing the Go-side equivalent of this
seam from scratch — there was no prior `ExecutionEnv`-shaped abstraction in this port. ADR-0004
covers cancellation generally (ctx as a MUST contract, Go errors over a thrown-string convention)
but did not yet apply that decision to a filesystem/shell boundary, because no such boundary existed
in the port until this work.

## Decision

`ExecutionEnv` (`tools/env.go`) is a **closure-captured, Go-shaped seam**, not a literal port of
upstream's per-call-context design:

- **`ExecutionEnv` is injected once, at tool-construction time** (`ReadToolOptions.Env`,
  `WriteToolOptions.Env`, `EditToolOptions.Env`, `BashToolOptions.Env`), not threaded as a per-call
  context argument. There is no Go analog to upstream's arbitrary per-call `TContext` object — every
  built-in tool closes over its `Env` the same way it closes over its other options
  (`CommandPrefix`, `Prepare`, `ImageProcessor`, ...), matching how every other tool in this port is
  constructed (`Tool[P]`, Task 4).
- **Every method is ctx-first** (`func(ctx context.Context, ...) (T, error)`), per CTX-1/CTX-2 and
  ADR-0004's precedent — replacing upstream's optional `AbortSignal` parameter with the same
  MUST-contract ADR-0004 already established for tool execution generally.
- **Methods return plain Go `(T, error)`, not `Result[T, E]`.** Upstream's
  `Result<T, FileError | ExecutionError>` discriminated union exists because JS has no typed-error
  return convention; Go already has one (ERR-1/ERR-2/ERR-3). `fileErrorCode` (`edit.go`) maps the
  small, stable error-code vocabulary (`not_found`, `permission_denied`, `not_directory`,
  `is_directory`, `unknown`) from Go's `errors.Is`/`syscall` checks, instead of a typed
  `FileError.code` field flowing through a `Result`.
- **The mutation queue keys on pointer identity, not a string/handle.**
  `mutationKey{env ExecutionEnv, path string}` requires `ExecutionEnv` implementations to be pointer
  types (documented on the interface) so two calls against the *same* `Env` instance share a lock
  regardless of how many `ExecutionEnv`-typed variables reference it — matching upstream's
  per-harness-instance `Map`-keyed queue without inventing a separate identity/handle concept.
- **`AppendFile` and `CreateTemp` are additions.** Upstream's `ExecutionEnv` also has both, but they
  were not in this task's originally-drafted seam (`env.go`'s package comment records the gap).
  Both are load-bearing for `bash.go`'s spill-file mechanism (`shell_output.go`'s tail-buffer +
  full-output-file scheme) and were added rather than worked around.
- **POSIX-only.** `OSEnv.Exec`'s process-group-kill implementation (`env_unix.go`,
  `//go:build unix`) mirrors upstream's cross-platform `killProcessTree`, but only the POSIX half
  (`syscall.Kill(-pid, SIGKILL)` via `Setpgid`) is implemented. No Windows build tag exists.

## Alternatives considered

1. **Port the per-call `TContext` parameter literally** (every `Tool[P].Execute` gains a generic
   per-call context argument). Rejected: no existing tool in this port takes a per-call context
   object; retrofitting one for four built-in tools alone would fork the `Tool[P]` interface shape
   from every other tool, for a feature (arbitrary caller-supplied per-call data, e.g. `workspace`)
   that closure-capture at construction time already covers for every observed use case — see
   `bash_test.go`'s `TestBashToolPrepareMutatesCwdAndInheritEnv`, which proves `Prepare` can mutate
   `Cwd`/`Env`/`InheritEnv` without a per-call context object.
2. **Model errors as `Result[T, E]`** (a generic sum-type wrapper, mirroring upstream). Rejected:
   fights Go idiom and this repo's own ERR-1..3 rules; every other seam in this port (`SessionRepo`,
   `LLMProvider`) already returns plain `(T, error)`.
3. **Key the mutation queue on the resolved path string alone**, dropping `env` from the key.
   Rejected: two different `ExecutionEnv` instances (e.g. two different sandboxes, or a test double
   per test) could then serialize against each other's identical-looking paths despite operating on
   entirely different filesystems — a correctness bug, not just an over-serialization inefficiency.

## Consequences

- **Sandbox/remote adapters plug in at exactly this seam.** A future `ExecutionEnv` implementation
  (container, remote filesystem, VM) only needs to satisfy 9 ctx-first methods returning plain
  `(T, error)` plus the ctx-free `Cwd()` — no `Result[T, E]` machinery, no per-call context
  threading — to be a drop-in `Env`
  for all four built-in tools simultaneously.
- **Per-turn/per-call context is an app-layer concern, not a framework one.** An app that needs
  upstream's "workspace"-style per-call data implements it via `BashToolOptions.Prepare`/closures
  over its own `ExecutionEnv`, or by constructing a fresh tool per turn with a different `Env` — the
  framework does not own a per-call context slot.
- **`ExecutionEnv` implementations MUST be pointer types.** Documented on the interface; a
  value-type implementation would break the mutation queue's identity-keying silently (two value
  copies would never contend), so this is a hard constraint, not a style preference.
- **No Windows support today.** Any Windows `ExecutionEnv.Exec` implementation needs its own
  process-tree-kill primitive; `env_unix.go`'s `//go:build unix` tag makes the gap a compile-time
  absence (no `OSEnv.Exec` on non-Unix), not a silent runtime failure.
- **A known, deliberately out-of-scope gap surfaced while writing the v0.8.0 fixture sweep** (see
  `bash_test.go`'s package comment and the Task-13 report): `ExecutionEnv.Exec`'s `OnChunk` contract
  is only *implicitly* synchronous — satisfied structurally by `OSEnv`, which blocks until every
  `OnChunk` call has completed — but nothing in the interface's documented contract forbids an
  adapter from calling `OnChunk` asynchronously after `Exec` returns, and `bash.go`'s `bashThrottle`
  has no guard against that (unlike upstream's explicit `acceptingOutput` flag,
  `shell-output.ts`). Any future adapter with async output delivery must either deliver `OnChunk`
  synchronously within `Exec`, or `bash.go` needs a settled-guard added first — tracked as a
  follow-up, not fixed by this ADR.
- **ADR-0004 lineage.** This ADR extends ADR-0004's ctx-first-MUST-contract and
  Go-errors-over-thrown-exceptions decisions from "tool execution generally" to "the filesystem/shell
  seam tools execute against" — the same reasoning, applied one layer down.
