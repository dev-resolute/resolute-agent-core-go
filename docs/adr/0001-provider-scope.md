# 1. Build pi-llm-go with two providers instead of adopting an existing Go LLM library

Date: 2026-05-25
Status: Accepted

## Context

The upstream Pi framework's `packages/ai` is a 30+ provider abstraction layer. Production targets for this Go port are exclusively **Gemini**, **Fireworks**, and **locally-hosted LLMs** (Ollama, vLLM, llama.cpp server, LM Studio). Anthropic — upstream Pi's default and the framework's reference behavior — is explicitly NOT a production target.

Three real options were considered for the LLM layer:

- **A′** — Build a minimal `pi-llm-go` with `LLMProvider` implementations behind a shared interface, ONE per wire protocol the production targets actually use.
- **B** — Adopt an existing Go LLM/agent framework (`cloudwego/eino`, `tmc/langchaingo`, `firebase/genkit`) and shim Pi's event/state semantics on top.
- **C** — Faithful Go port of the upstream 30-provider `ai` package.

## Decision

We chose **A′** with exactly **two providers**:

1. **OpenAI-compatible adapter** — one implementation parameterized by `BaseURL`, covering both Fireworks and every locally-hosted LLM server (all of which expose an OpenAI Chat Completions API). Built on the official `openai/openai-go` SDK.
2. **Gemini provider** — built on the official `google.golang.org/genai` SDK.

Anthropic is out of scope. So are Bedrock, Vertex AI, Mistral, Groq, xAI, DeepSeek, and every other provider in upstream's 30-provider matrix.

## Why

- **Production needs collapse to two wire protocols, not 30.** Fireworks and every local LLM server speak OpenAI Chat Completions, so one adapter covers all of them. Gemini is the second wire protocol.
- **Pi's distinctive value is the agent loop and harness semantics** (thinking blocks, cross-provider handoffs, tool-call lifecycle, steering/follow-up). Adopting eino or langchaingo would force us to layer our agent abstraction on top of theirs — two state machines in one process, fighting on event shape.
- **C is a multi-month plumbing project that solves a problem (provider breadth) we do not have.**
- **Anthropic was considered and dropped.** Upstream Pi is Anthropic-first, and most agent frameworks default to Anthropic, but the production targets are explicit and exclusive. Carrying Anthropic for "parity with upstream" would mean writing and maintaining an entire provider implementation for no production user. If a real Anthropic user appears later, the `LLMProvider` interface accepts it without ceremony.
- **Owning the `LLMProvider` interface keeps `pi-core-agent-go` clean** and lets us add Anthropic, Bedrock, Vertex, etc. later without rewriting the agent.

## Consequences

- **v0.1.0 ships both providers** (OpenAI-compatible + Gemini). Covers all three production targets — Fireworks + local LLMs via OpenAI-compatible, and Gemini natively — in one release. Larger v0.1.0 scope (~3-4 weeks) but no provider-coverage gap between v0.1.0 and downstream `pi-core-agent-go` consumption.
- **We accept the cost** of writing and maintaining two provider implementations (~2-3 weeks for OpenAI-compatible + streaming + tool calling, ~1-2 weeks for Gemini layered on top of the now-stable interface).
- **Any future provider must be implemented against `LLMProvider` ourselves**; there is no plugin ecosystem to fall back on.
- **Cross-provider feature parity** (e.g., thinking blocks, structured outputs) is our responsibility, not a library author's.
- **Validation against upstream Pi's reference Anthropic behavior is not possible** because we don't ship Anthropic. We validate against the OpenAI Chat Completions spec and Gemini's documented behavior instead.
