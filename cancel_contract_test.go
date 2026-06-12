package pi

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resolute-sh/pi-llm-go"
)

// TestCallerCtxCancellation_HonoringTool pins the ADR-0004 contract: when the
// caller cancels their own context mid-prompt, PromptResult.Err is
// ErrPromptCancelled — not the raw context.Canceled that propagates through the
// inner context — and no LLMErrorEvent precedes the terminal result. The tool
// blocks on its ctx and honors the cancellation, so the loop exits via the
// auto-continue cancellation seam.
func TestCallerCtxCancellation_HonoringTool(t *testing.T) {
	t.Parallel()

	toolStarted := make(chan struct{})
	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: "block", Args: echoArgs("x")}
			events <- llm.ToolCallEndEvent{CallID: "c1"}
			events <- llm.MessageEndEvent{}
		},
	}

	var startOnce sync.Once
	block := NewTool(Tool[echoParams]{
		Name:        "block",
		Description: "blocks until ctx is cancelled, then honors it",
		Execute: func(ctx context.Context, _ echoParams) (ToolResult, error) {
			startOnce.Do(func() { close(toolStarted) })
			<-ctx.Done()
			return ToolResult{}, ctx.Err()
		},
	})

	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{block},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	callerCtx, callerCancel := context.WithCancel(context.Background())
	stream, err := a.Prompt(callerCtx, NewText("user", "go"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	var sawLLMError atomic.Bool
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for ev := range stream.Events {
			if _, ok := ev.(LLMErrorEvent); ok {
				sawLLMError.Store(true)
			}
		}
	}()

	select {
	case <-toolStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("tool never started")
	}

	callerCancel()

	var result PromptResult
	select {
	case result = <-stream.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not deliver after caller cancellation")
	}
	<-drained

	if !errors.Is(result.Err, ErrPromptCancelled) {
		t.Errorf("result.Err = %v, want ErrPromptCancelled", result.Err)
	}
	if sawLLMError.Load() {
		t.Error("spurious LLMErrorEvent emitted on caller-ctx cancellation")
	}
}

// TestCallerCtxCancellation_LeakedTool exercises the loop's terminal error
// handler. A tool that ignores its cancelled ctx forces the batch to leak past
// ShutdownTimeout, so the cancelled inner context surfaces through runOneTurn as
// the terminal error. Before the fix the handler treated the propagated
// context.Canceled as a provider failure: it emitted a spurious LLMErrorEvent
// and returned context.Canceled. The contract is ErrPromptCancelled with no
// LLMErrorEvent. A ToolLeakEvent for the ignored tool is expected and tolerated.
func TestCallerCtxCancellation_LeakedTool(t *testing.T) {
	t.Parallel()

	toolStarted := make(chan struct{})
	release := make(chan struct{})
	toolReturned := make(chan struct{})
	var releaseOnce sync.Once
	releaseTool := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		releaseTool()
		<-toolReturned
	})

	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			events <- llm.ToolCallStartEvent{CallID: "leak-1", ToolName: "hang", Args: echoArgs("x")}
			events <- llm.ToolCallEndEvent{CallID: "leak-1"}
			events <- llm.MessageEndEvent{}
		},
	}

	var startOnce sync.Once
	hang := NewTool(Tool[echoParams]{
		Name:        "hang",
		Description: "ignores ctx",
		Execute: func(ctx context.Context, _ echoParams) (ToolResult, error) {
			startOnce.Do(func() { close(toolStarted) })
			<-release
			close(toolReturned)
			return ToolResult{Content: "late"}, nil
		},
	})

	a, err := NewAgent(AgentConfig{
		Providers:       []llm.LLMProvider{provider},
		DefaultModel:    "test/model",
		Tools:           []RegisteredTool{hang},
		ShutdownTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	callerCtx, callerCancel := context.WithCancel(context.Background())
	stream, err := a.Prompt(callerCtx, NewText("user", "go"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	var sawLLMError atomic.Bool
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for ev := range stream.Events {
			if _, ok := ev.(LLMErrorEvent); ok {
				sawLLMError.Store(true)
			}
		}
	}()

	select {
	case <-toolStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("tool never started")
	}

	callerCancel()

	var result PromptResult
	select {
	case result = <-stream.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not deliver after caller cancellation")
	}
	<-drained

	if !errors.Is(result.Err, ErrPromptCancelled) {
		t.Errorf("result.Err = %v, want ErrPromptCancelled", result.Err)
	}
	if sawLLMError.Load() {
		t.Error("spurious LLMErrorEvent emitted on caller-ctx cancellation")
	}
}
