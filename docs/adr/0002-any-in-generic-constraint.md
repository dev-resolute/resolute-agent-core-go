# 2. `any` is permitted as a generic type-parameter constraint, banned as a value type

Date: 2026-05-24
Status: Accepted

## Context

CS-7 in `docs/go-rules/golang.md` originally read "NEVER, EVER use `any`." Read literally, this banned the token `any` everywhere, including its standard idiomatic use as an unconstrained generic type parameter (`[P any]`).

The Tool definition (see [CONTEXT.md](../../CONTEXT.md) — `Tool`) is generic over the parameter struct type: `Tool[P any]` with `Execute func(ctx, P) (ToolResult, error)`. This gives tool authors compile-time-typed access to LLM-supplied arguments and eliminates an entire class of silent bugs (typos in `args["queyr"]`). Go has no constraint that meaningfully expresses "any JSON-serializable struct" — every Go type is JSON-serializable via reflection — so the only options for a generic Tool are: use `any` as the constraint, or hide it behind a no-op rename (`type ToolParams interface{}`) which is the same thing with worse honesty.

CS-4 explicitly endorses generics ("prefer generics when it clarifies and speeds"). CS-7's rhetoric ("we are better than that") plainly targets lazy untyped values, not parametric polymorphism.

## Decision

CS-7 is amended:

- **Value-position `any` remains banned.** No `var x any`, no `func f() any`, no `map[string]any`, no struct field of type `any`, no return type `any`.
- **Constraint-position `any` is permitted** when it expresses parametric polymorphism (`Tool[P any]`, `func Map[T, U any](...)`).

The two uses are semantically different: the former hides type information, the latter exposes it through parametricity.

## Why

- The strict reading would force `Tool` to take `json.RawMessage` params and lose typed Execute — the exact ergonomic win that made Option A from Q5 worth picking. The regression is observable in every tool definition.
- The sealed-marker workaround (`isToolParams()` method on user structs) adds friction and serves no semantic purpose other than satisfying a textual rule.
- Renaming `any` to `interface{}` (the pre-Go-1.18 form) is the same thing and does not satisfy CS-7's intent.

## Consequences

- Reviewers must distinguish value-position from constraint-position when applying CS-7. The amended rule text makes this distinction explicit, but it is a real cognitive cost during review.
- This precedent is narrow. Any future relaxation of CS-7 needs its own ADR; the exception does not extend to "but my use case is special too."
