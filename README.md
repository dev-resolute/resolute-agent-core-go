# pi-core-agent-go

Stateful agent loop for Go, built on `pi-llm-go`.

## Features

- **Agent + Run model**: Long-lived `Agent` spawns concurrent `*Run`s.
- **Typed tools**: `Tool[P any]` with compile-time parameter checking.
- **Streaming events**: Sealed `AgentEvent` interface with 16+ variants.
- **Session persistence**: `MemorySession` (default) or `JSONLSession` (disk).
- **Compaction**: Manual transcript summarization (v0.1.0).
- **Steering + follow-up**: Inject messages mid-run or after completion.
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

run, _ := agent.Run(ctx, pi.RunOpts{
    Prompt: pi.NewText("user", "Hello"),
})

for ev := range run.Events() {
    // type-switch on pi.AgentEvent
}
result := <-run.Done()
```

## Testing

Unit tests pass without secrets:
```bash
go test ./...
```

Integration tests (require API keys):
```bash
go test -tags=integration ./...
```

## License

MIT
