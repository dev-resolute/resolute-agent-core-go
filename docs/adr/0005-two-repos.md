# 5. Ship `pi-llm-go` and `pi-core-agent-go` as two separate repositories

Date: 2026-05-25
Status: Accepted

> **Rebrand note (2026-06-27):** the two-repos decision stands; only the names/org changed.
> `resolute-sh/pi-llm-go` → `dev-resolute/resolute-llm-go`, `resolute-sh/pi-core-agent-go` →
> `dev-resolute/resolute-agent-core-go`. The paths in this ADR's body are retained as the
> historical record. See `docs/prds/resolute-rebrand-rediff-0.80.2-providers.md`.

## Context

Q14 considered three module-layout options: (A) single Go module with subpackages, (B) two modules in one repo coordinated via `go.work`, (C) two separate repositories. The recommendation was B for its low coordination cost and matching of upstream Pi's separate-package model without the operational overhead of two repos.

The user chose C and renamed `pi-ai-go` to `pi-llm-go` ("LLM" is more honest than "AI" for what this layer actually is).

## Decision

Ship two separate GitHub repositories under the `resolute-sh` org:

- `github.com/resolute-sh/pi-llm-go` — provider abstraction (OpenAI-compatible, Gemini)
- `github.com/resolute-sh/pi-core-agent-go` — agent loop, harness, session, compaction

`pi-core-agent-go` depends on `pi-llm-go` via standard Go module versioning (`go get github.com/resolute-sh/pi-llm-go@vX.Y.Z`).

## Why

- **Maximum independence for `pi-llm-go`.** It is genuinely useful as a standalone library for Go developers who want a multi-provider LLM client without the opinionated agent loop. A separate repo makes that adoption path frictionless — no need to import a larger project to use a smaller one.
- **Honest dependency graph.** The agent literally cannot reach into internal LLM-layer types because they're in another module's unexported scope. Reviewers don't need to enforce a docs convention.
- **Independent release cadence.** Provider bugs in `pi-llm-go` ship without dragging agent changes along. Agent improvements ship without re-publishing the LLM layer.
- **Matches upstream Pi's split.** `@earendil-works/pi-ai` and `@earendil-works/pi-agent` are independently published npm packages. Two repos preserves that model in Go.

## Consequences

- **Two-PR dance for cross-repo changes.** Any change to `LLMProvider` (or any other interface `pi-core-agent-go` depends on) requires: PR + tag in `pi-llm-go`, then `go get` + PR in `pi-core-agent-go`. The workspace option (B) avoided this.
- **`replace` directives for local dev.** During development of cross-cutting changes, `pi-core-agent-go/go.mod` uses a `replace github.com/resolute-sh/pi-llm-go => ../pi-llm-go` line to point at the local checkout. The `replace` line must be uncommitted before merging — discipline required.
- **Duplicated style/rules docs.** `docs/go-rules/` is copied into both repos. ADR-0002 (the CS-7 generic-constraint exception) is copied or summarized into both. Both repos enforce the same rules.
- **ADR renumbering at migration.** During migration from `pi-research`, each ADR is renumbered to its target repo's sequence. The current numbering (0001-0005 in `pi-research`) is transitional.
- **CI runs twice** — once per repo. Most cross-repo bugs surface at the `go get` boundary; both repos need integration coverage that pulls the other in.
