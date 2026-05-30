# pi-core-agent-go

Stateful agent loop for Go, built on `pi-llm-go`.

## Features

- **Single-runner mutable Agent**: one `Agent` per conversation; `Agent.Prompt` returns an `EventStream`. Runtime config is mutable via setters and picked up on the next turn (ADR-0006).
- **Typed tools**: `Tool[P any]` with compile-time parameter checking.
- **Streaming events**: Sealed `AgentEvent` interface with 16+ variants.
- **Session persistence**: `MemorySession` (default) or `JSONLSession` (disk).
- **Compaction**: Manual transcript summarization.
- **Steering + follow-up**: Inject messages mid-prompt or after completion.
- **Cancellation**: Bounded shutdown with `ToolLeakEvent` observability.

## Install

```bash
go get github.com/resolute-sh/pi-core-agent-go
```

## Usage

```go
agent, _ := pi.NewAgent(pi.AgentConfig{
    Providers:    []llm.LLMProvider{provider},
    DefaultModel: "openai-compat/gpt-4o",
    Tools:        []pi.RegisteredTool{myTool},
})

stream, _ := agent.Prompt(ctx, pi.NewText("user", "Hello"), pi.PromptOpts{})

for ev := range stream.Events {
    // type-switch on pi.AgentEvent
}
result := <-stream.Done

// Reconfigure mid-conversation; takes effect on the next prompt's turn.
agent.SetModel("gemini/gemini-2.5-flash")
```

## Testing

Provider-backed tests run against a live Gemini provider and skip when
`GEMINI_API_KEY` is unset; pure-logic tests (turn snapshot, compaction,
schema) run without it.
```bash
GEMINI_API_KEY=... go test -race ./...
```

## License

MIT
