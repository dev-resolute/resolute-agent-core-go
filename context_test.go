package pi

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/resolute-sh/pi-llm-go"
)

// stubProviderOnce wraps stubProvider so the first Stream call emits a tool
// call and all subsequent calls emit a text completion, ending the loop.
type stubProviderOnce struct {
	name    string
	mu      sync.Mutex
	called  int
	emitOne func(events chan<- llm.LLMEvent)
}

func (p *stubProviderOnce) Name() string { return p.name }

func (p *stubProviderOnce) Capabilities(model string) llm.ProviderCapabilities {
	return llm.ProviderCapabilities{Streaming: true, ToolCalling: true}
}

func (p *stubProviderOnce) Stream(ctx context.Context, req llm.LLMRequest) llm.EventStream {
	events := make(chan llm.LLMEvent)
	done := make(chan llm.StreamResult, 1)
	p.mu.Lock()
	n := p.called
	p.called++
	p.mu.Unlock()
	go func() {
		if n == 0 {
			p.emitOne(events)
		} else {
			events <- llm.TextDeltaEvent{Delta: "done"}
			events <- llm.MessageEndEvent{}
		}
		close(events)
		done <- llm.StreamResult{}
	}()
	return llm.NewEventStream(events, done)
}

func TestContext_NilBeforeAnyPrompt(t *testing.T) {
	t.Parallel()

	// given an agent that has never run a prompt
	a, err := NewAgent(AgentConfig{DefaultModel: "test/model"})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	// when Context() is called while idle
	ctx := a.Context()

	// then it is non-nil and already done
	if ctx == nil {
		t.Fatal("Context() must not return nil when idle")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Context() when idle must return an already-done context")
	}
	if !errors.Is(context.Cause(ctx), ErrNoPromptInFlight) {
		t.Fatalf("context.Cause(idle ctx) = %v, want ErrNoPromptInFlight", context.Cause(ctx))
	}
}

func TestContext_IdleAfterNormalCompletion(t *testing.T) {
	t.Parallel()

	// given an agent that has completed a prompt
	provider := &stubProvider{
		name: "test",
		emit: func(events chan<- llm.LLMEvent) {
			events <- llm.TextDeltaEvent{Delta: "hi"}
			events <- llm.MessageEndEvent{}
		},
	}
	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	stream, err := a.Prompt(context.Background(), NewText("user", "hi"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	_, result := drain(t, stream)
	if result.Err != nil {
		t.Fatalf("prompt error: %v", result.Err)
	}

	// when Context() is called after completion
	ctx := a.Context()

	// then it is non-nil and already done
	if ctx == nil {
		t.Fatal("Context() must not return nil after prompt completes")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Context() after prompt completes must return a done context")
	}
	if !errors.Is(context.Cause(ctx), ErrNoPromptInFlight) {
		t.Fatalf("context.Cause(post-prompt ctx) = %v, want ErrNoPromptInFlight", context.Cause(ctx))
	}
}

// startProbedPrompt starts a single-tool prompt whose probe tool captures
// a.Context() from inside the tool and then blocks until that context is
// cancelled. It returns the agent, the live event stream, and the agent
// context observed from inside the tool. Events are drained in the background
// so emit never blocks.
func startProbedPrompt(t *testing.T, callerCtx context.Context) (*Agent, *EventStream, context.Context) {
	t.Helper()

	agentCtxCh := make(chan context.Context, 1)
	provider := &stubProviderOnce{
		name: "test",
		emitOne: func(events chan<- llm.LLMEvent) {
			events <- llm.ToolCallStartEvent{
				CallID:   "c1",
				ToolName: "ctx_probe",
				Args:     json.RawMessage(`{}`),
			}
			events <- llm.ToolCallEndEvent{CallID: "c1"}
			events <- llm.MessageEndEvent{}
		},
	}

	var a *Agent
	probeTool := NewTool(Tool[struct{}]{
		Name:        "ctx_probe",
		Description: "probe agent context",
		Execute: func(ctx context.Context, _ struct{}) (ToolResult, error) {
			agentCtxCh <- a.Context()
			<-ctx.Done()
			return ToolResult{}, ctx.Err()
		},
	})

	var err error
	a, err = NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{probeTool},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	stream, err := a.Prompt(callerCtx, NewText("user", "hi"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	go func() {
		for range stream.Events {
		}
	}()

	var agentCtx context.Context
	select {
	case agentCtx = <-agentCtxCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: tool did not provide agent context")
	}
	return a, stream, agentCtx
}

func TestContext_ToolObservesStopCancellation(t *testing.T) {
	t.Parallel()

	// given an agent with a probe tool holding a live agent context
	a, stream, agentCtx := startProbedPrompt(t, context.Background())

	// then the context is live (not done) while the prompt is in flight
	select {
	case <-agentCtx.Done():
		t.Fatal("agent.Context() must not be done while the prompt is in flight")
	default:
	}

	// spawn nested work tied to the agent context — simulates a background
	// goroutine launched from inside a tool
	nestedDone := make(chan struct{})
	go func() {
		defer close(nestedDone)
		<-agentCtx.Done()
	}()

	// when Stop() is called
	a.Stop()

	// then nested work observes cancellation
	select {
	case <-nestedDone:
	case <-time.After(5 * time.Second):
		t.Fatal("nested goroutine did not observe cancellation after Stop")
	}

	// and the cause is ErrAgentStopped
	if cause := context.Cause(agentCtx); !errors.Is(cause, ErrAgentStopped) {
		t.Fatalf("context.Cause(agentCtx) = %v, want ErrAgentStopped", cause)
	}

	// prompt result must also reflect the stop
	select {
	case result := <-stream.Done:
		if !errors.Is(result.Err, ErrAgentStopped) {
			t.Fatalf("PromptResult.Err = %v, want ErrAgentStopped", result.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for prompt to complete after Stop")
	}
}

func TestContext_CallerCancellationObservable(t *testing.T) {
	t.Parallel()

	// given a probe tool holding a live agent context derived from a caller ctx
	callerCtx, callerCancel := context.WithCancelCause(context.Background())
	_, stream, agentCtx := startProbedPrompt(t, callerCtx)

	// when the caller context is cancelled
	callerCancel(ErrPromptCancelled)

	// then the agent context becomes done
	select {
	case <-agentCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("agent context not done after caller context cancelled")
	}

	// and the cause is ErrPromptCancelled
	if cause := context.Cause(agentCtx); !errors.Is(cause, ErrPromptCancelled) {
		t.Fatalf("context.Cause(agentCtx) = %v, want ErrPromptCancelled", cause)
	}

	<-stream.Done
}

// TestContext_NoLiveContextAfterCompletion reproduces the reviewer's probe for
// the loop-exit cancellation defect: before the fix, a prompt's inner context
// was never cancelled on normal completion, so a caller doing <-stream.Done then
// Context() could observe a live, never-cancelled context for a finished prompt
// (~1-2 per 2000 iterations, in the window between the result send and the
// running-counter decrement). 2000 iterations preserves the reviewer's measured
// repro power while staying CI-tolerable (sub-second with the hermetic stub).
//
// Not parallel: it deliberately races Context() against loop teardown and runs
// many iterations.
func TestContext_NoLiveContextAfterCompletion(t *testing.T) {
	const iterations = 2000

	provider := &stubProvider{
		name: "test",
		emit: func(events chan<- llm.LLMEvent) {
			events <- llm.TextDeltaEvent{Delta: "hi"}
			events <- llm.MessageEndEvent{}
		},
	}
	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	for i := 0; i < iterations; i++ {
		stream, err := a.Prompt(context.Background(), NewText("user", "hi"), PromptOpts{})
		if err != nil {
			t.Fatalf("iteration %d: Prompt: %v", i, err)
		}
		go func() {
			for range stream.Events {
			}
		}()

		// observe the prompt as a caller would: take the terminal result, then
		// immediately read Context()
		<-stream.Done
		ctx := a.Context()

		// a finished prompt's context must read as no-longer-in-flight: done,
		// with cause ErrNoPromptInFlight (idle context, or the inner context
		// cancelled on loop exit) — never a live, never-cancelled context
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: Context() returned a live context after stream.Done", i)
		}
		if cause := context.Cause(ctx); !errors.Is(cause, ErrNoPromptInFlight) {
			t.Fatalf("iteration %d: context.Cause = %v, want ErrNoPromptInFlight", i, cause)
		}
	}
}

func TestContext_RaceFree(t *testing.T) {
	// This test is intentionally not t.Parallel() — it exercises concurrent
	// goroutine access; the race detector validates there are no data races.

	provider := &stubProvider{
		name: "test",
		emit: func(events chan<- llm.LLMEvent) {
			events <- llm.TextDeltaEvent{Delta: "hi"}
			events <- llm.MessageEndEvent{}
		},
	}
	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	const readers = 8
	const readsPerReader = 200

	var wg sync.WaitGroup

	// concurrent readers spin-calling Context()
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < readsPerReader; j++ {
				_ = a.Context()
			}
		}()
	}

	// concurrent prompt that starts and completes during the readers
	wg.Add(1)
	go func() {
		defer wg.Done()
		stream, err := a.Prompt(context.Background(), NewText("user", "hi"), PromptOpts{})
		if err != nil {
			if errors.Is(err, ErrAgentBusy) {
				return
			}
			t.Errorf("Prompt: %v", err)
			return
		}
		_, result := drain(t, stream)
		if result.Err != nil {
			t.Errorf("prompt error: %v", result.Err)
		}
	}()

	wg.Wait()
}
