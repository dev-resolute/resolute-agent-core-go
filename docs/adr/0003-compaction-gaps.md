# 3. Compaction ships day 1 and inherits upstream's documented gaps

Date: 2026-05-25
Status: Accepted

## Context

The compaction subsystem (`Agent.Compact`, summarization, branch summaries) is being ported from `packages/agent/src/harness/compaction/` in [the upstream Pi framework](https://github.com/earendil-works/pi). Direct source inspection (not README claims) revealed Pi's compaction is much narrower than the framing in the docs:

- **Manual invocation only.** A `shouldCompact()` helper exists but is never wired into the run loop; the agent never compacts unless the caller explicitly calls it.
- **No tool-call atomicity.** The cut point does not respect `tool_call`/`tool_result` pairing; a compaction can split a pair, leaving an orphan in the kept tail or the summary.
- **No system message pinning.** The system prompt is treated as an ordinary message and can be folded into the summary.
- **No local token counting.** Pi relies entirely on `usage` fields returned by the provider in streaming responses; there is no per-provider tokenizer in `packages/ai/`.

We considered three options: (A′) port what Pi has day 1, (B′) port + add auto-trigger, (C) defer compaction entirely.

## Decision

Ship **A′** — port Pi's compaction shape exactly, day 1. Inherit all four gaps above without "fixing" them in v0.

## Why

- The gaps are not bugs in Pi — they are design boundaries. Closing them in our port would create silent behavioral drift between the TS and Go runtimes and complicate session interchange.
- Auto-trigger has real billing implications (the agent makes an LLM call you didn't ask for) and deserves an opt-in flag designed against real usage data, not invented in v0.
- Tool-call atomicity is a fixable problem in v0.1+ once we see whether users actually hit it. Pi's gap suggests their users don't, or work around it via `TransformContext`.
- System message preservation is one line in user-side `TransformContext`. Not worth a special framework lever in v0.
- Skipping local tokenizers eliminates a multi-week scope item (three provider tokenizers, each different) that compaction does not need.

## Consequences

- **Documented limitations.** README and godoc must call out the four gaps explicitly, so users don't expect behavior we don't deliver.
- **TODO markers in code.** Each gap gets a `// TODO(v0.x): see ADR-0003` near the relevant code so future maintainers don't accidentally close the gap and break upstream parity without re-reading this decision.
- **No `ContextWindow` field on `AgentConfig` in v0.** Adding it without auto-trigger would be cargo-cult; adding it later is non-breaking.
- **Users with long conversations have two levers**: manual `Compact()` or `TransformContext` for custom pruning. Both are sufficient for the v0 use cases we've validated.
- **Auto-trigger arrives in v0.x** as `AgentConfig.AutoCompact bool` once we have data on the manual-call pattern.
