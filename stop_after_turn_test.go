package pi

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dev-resolute/resolute-llm-go"
)

// TestShouldStopAfterTurn covers the post-turn continuation decision: a
// tool-call turn auto-continues to a second LLM call carrying the tool results,
// a ShouldStopAfterTurn predicate can still stop the loop after a tool-call
// turn, and a queued follow-up still drives a second call after a text-only turn.
func TestShouldStopAfterTurn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		stopFn        func(context.Context, AfterTurnCtx) bool
		firstTurnTool bool
		queueFollowUp bool
		wantCalls     int32
	}{
		{
			name:          "tool-call turn auto-continues to a second LLM call",
			stopFn:        nil,
			firstTurnTool: true,
			wantCalls:     2,
		},
		{
			name:          "predicate stops the loop after a tool-call turn",
			stopFn:        func(_ context.Context, _ AfterTurnCtx) bool { return true },
			firstTurnTool: true,
			wantCalls:     1,
		},
		{
			name:          "queued follow-up drives a second LLM call after a text-only turn",
			stopFn:        nil,
			firstTurnTool: false,
			queueFollowUp: true,
			wantCalls:     2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var calls atomic.Int32

			// firstTurnGate holds the provider's first emit until the test has
			// optionally queued a follow-up, guaranteeing it is present when the
			// loop reaches the post-turn select.
			firstTurnGate := make(chan struct{})

			provider := &stubProvider{
				name: "test",
				emit: func(events chan<- llm.LLMEvent) {
					n := calls.Add(1)
					if n == 1 {
						<-firstTurnGate
						if tt.firstTurnTool {
							events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: "noop", Args: []byte("{}")}
							events <- llm.ToolCallEndEvent{CallID: "c1"}
						} else {
							events <- llm.TextDeltaEvent{Delta: "first"}
						}
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

			if tt.queueFollowUp {
				// Queue a follow-up directly on the in-flight promptRun. a.current
				// is set before the loop goroutine starts, so this is safe here.
				a.mu.RLock()
				pr := a.current
				a.mu.RUnlock()
				pr.followUpCh <- NewText("user", "follow-up")
			}
			close(firstTurnGate)

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
// hook carries the correct turn number and tool-call flag. The predicate returns
// true so the single turn under test is the only one — without it a tool-call
// turn would auto-continue, and the always-tool-call stub would loop forever.
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
						return true
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
