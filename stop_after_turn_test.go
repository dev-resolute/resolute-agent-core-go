package pi

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/resolute-sh/pi-llm-go"
)

// TestShouldStopAfterTurn verifies that the ShouldStopAfterTurn hook exits the
// loop cleanly before consuming queued follow-up messages or starting another
// LLM call.
func TestShouldStopAfterTurn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		stopFn    func(context.Context, AfterTurnCtx) bool
		wantCalls int32
	}{
		{
			name:      "nil predicate processes follow-up and runs two turns",
			stopFn:    nil,
			wantCalls: 2,
		},
		{
			name:      "predicate stops loop before follow-up, single LLM call",
			stopFn:    func(_ context.Context, _ AfterTurnCtx) bool { return true },
			wantCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var calls atomic.Int32

			// followUpQueued blocks the provider's first emit until the test has
			// placed a follow-up message in the loop's channel, guaranteeing the
			// message is present when the loop reaches the select.
			followUpQueued := make(chan struct{})

			provider := &stubProvider{
				name: "test",
				emit: func(events chan<- llm.LLMEvent) {
					n := calls.Add(1)
					if n == 1 {
						<-followUpQueued
						events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: "noop", Args: []byte("{}")}
						events <- llm.ToolCallEndEvent{CallID: "c1"}
						events <- llm.MessageEndEvent{}
					} else {
						events <- llm.TextDeltaEvent{Delta: "ok"}
						events <- llm.MessageEndEvent{}
					}
				},
			}

			a, err := NewAgent(AgentConfig{
				Providers:    []llm.LLMProvider{provider},
				DefaultModel: "test/model",
				Tools:        []RegisteredTool{toolNamed("noop")},
				Hooks:        Hooks{ShouldStopAfterTurn: tt.stopFn},
			})
			if err != nil {
				t.Fatalf("NewAgent: %v", err)
			}

			stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
			if err != nil {
				t.Fatalf("Prompt: %v", err)
			}

			// Queue a follow-up directly on the in-flight promptRun. a.current is
			// set before the loop goroutine starts, so this is safe to access here.
			a.mu.RLock()
			pr := a.current
			a.mu.RUnlock()
			pr.followUpCh <- NewText("user", "follow-up")

			// Unblock the provider now that the follow-up is in the channel.
			close(followUpQueued)

			_, result := drain(t, stream)

			if result.Err != nil {
				t.Errorf("unexpected error: %v", result.Err)
			}
			if got := calls.Load(); got != tt.wantCalls {
				t.Errorf("provider.Stream() called %d times, want %d", got, tt.wantCalls)
			}
		})
	}
}

// TestShouldStopAfterTurnInvokedOnTerminate verifies that a ShouldStopAfterTurn
// predicate is invoked even when a tool returns Terminate=true, with the correct
// AfterTurnCtx (HadToolCalls=true), and that the prompt exits cleanly (Err==nil).
func TestShouldStopAfterTurnInvokedOnTerminate(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		captured AfterTurnCtx
		invoked  bool
	)

	terminateTool := NewTool(Tool[struct{}]{
		Name:        "stop",
		Description: "stop",
		Execute: func(ctx context.Context, _ struct{}) (ToolResult, error) {
			return ToolResult{Terminate: true}, nil
		},
	})

	provider := &stubProvider{
		name: "test",
		emit: func(events chan<- llm.LLMEvent) {
			events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: "stop", Args: []byte("{}")}
			events <- llm.ToolCallEndEvent{CallID: "c1"}
			events <- llm.MessageEndEvent{}
		},
	}

	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{terminateTool},
		Hooks: Hooks{
			ShouldStopAfterTurn: func(_ context.Context, c AfterTurnCtx) bool {
				mu.Lock()
				captured = c
				invoked = true
				mu.Unlock()
				return false
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	_, result := drain(t, stream)

	if result.Err != nil {
		t.Errorf("unexpected error: %v", result.Err)
	}

	mu.Lock()
	c := captured
	wasInvoked := invoked
	mu.Unlock()

	if !wasInvoked {
		t.Fatal("ShouldStopAfterTurn was not invoked on a terminate turn")
	}
	if c.Turn != 1 {
		t.Errorf("AfterTurnCtx.Turn = %d, want 1", c.Turn)
	}
	if !c.HadToolCalls {
		t.Errorf("AfterTurnCtx.HadToolCalls = false, want true")
	}
}

// TestShouldStopAfterTurnContext verifies that the AfterTurnCtx passed to the
// hook carries the correct turn number and tool-call flag.
func TestShouldStopAfterTurnContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		emitFn       func(events chan<- llm.LLMEvent)
		wantTurn     int
		wantHadCalls bool
	}{
		{
			name: "text-only turn has HadToolCalls=false",
			emitFn: func(events chan<- llm.LLMEvent) {
				events <- llm.TextDeltaEvent{Delta: "hi"}
				events <- llm.MessageEndEvent{}
			},
			wantTurn: 1, wantHadCalls: false,
		},
		{
			name: "tool-call turn has HadToolCalls=true",
			emitFn: func(events chan<- llm.LLMEvent) {
				events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: "noop", Args: []byte("{}")}
				events <- llm.ToolCallEndEvent{CallID: "c1"}
				events <- llm.MessageEndEvent{}
			},
			wantTurn: 1, wantHadCalls: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var (
				mu       sync.Mutex
				captured AfterTurnCtx
			)

			a, err := NewAgent(AgentConfig{
				Providers:    []llm.LLMProvider{&stubProvider{name: "test", emit: tt.emitFn}},
				DefaultModel: "test/model",
				Tools:        []RegisteredTool{toolNamed("noop")},
				Hooks: Hooks{
					ShouldStopAfterTurn: func(_ context.Context, c AfterTurnCtx) bool {
						mu.Lock()
						captured = c
						mu.Unlock()
						return false
					},
				},
			})
			if err != nil {
				t.Fatalf("NewAgent: %v", err)
			}

			stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
			if err != nil {
				t.Fatalf("Prompt: %v", err)
			}
			_, result := drain(t, stream)
			if result.Err != nil {
				t.Fatalf("unexpected error: %v", result.Err)
			}

			mu.Lock()
			c := captured
			mu.Unlock()

			if c.Turn != tt.wantTurn {
				t.Errorf("AfterTurnCtx.Turn = %d, want %d", c.Turn, tt.wantTurn)
			}
			if c.HadToolCalls != tt.wantHadCalls {
				t.Errorf("AfterTurnCtx.HadToolCalls = %v, want %v", c.HadToolCalls, tt.wantHadCalls)
			}
		})
	}
}
