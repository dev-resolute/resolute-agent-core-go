package pi

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/resolute-sh/pi-llm-go"
)

// recordingProvider captures the LLMRequest received by Stream so tests can
// assert on it without a live provider. Capabilities reports Thinking:true so
// the active thinking level is not downgraded; budgets are set unconditionally.
// When streamErr is non-nil, Stream reports it as a fatal StreamResult error
// so callers can assert on terminal error propagation without a live provider.
type recordingProvider struct {
	name      string
	emit      func(events chan<- llm.LLMEvent)
	streamErr error

	mu          sync.Mutex
	lastReq     llm.LLMRequest
	streamCalls atomic.Int32
}

func (p *recordingProvider) Name() string { return p.name }

func (p *recordingProvider) Capabilities(model string) llm.ProviderCapabilities {
	return llm.ProviderCapabilities{Streaming: true, ToolCalling: true, Thinking: true}
}

func (p *recordingProvider) Stream(ctx context.Context, req llm.LLMRequest) llm.EventStream {
	p.streamCalls.Add(1)
	p.mu.Lock()
	p.lastReq = req
	p.mu.Unlock()
	events := make(chan llm.LLMEvent)
	done := make(chan llm.StreamResult, 1)
	go func() {
		if p.streamErr == nil {
			p.emit(events)
		}
		close(events)
		done <- llm.StreamResult{Err: p.streamErr}
	}()
	return llm.NewEventStream(events, done)
}

func (p *recordingProvider) capturedBudgets() map[llm.ThinkingLevel]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastReq.ThinkingBudgets
}

func (p *recordingProvider) capturedReq() llm.LLMRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastReq
}

func (p *recordingProvider) streamCallCount() int {
	return int(p.streamCalls.Load())
}

func newRecordingProvider(name string) *recordingProvider {
	return &recordingProvider{
		name: name,
		emit: func(events chan<- llm.LLMEvent) {
			events <- llm.TextDeltaEvent{Delta: "ok"}
			events <- llm.MessageEndEvent{}
		},
	}
}

func thinkingBudgetsEqual(a, b map[llm.ThinkingLevel]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func runOnePrompt(t *testing.T, a *Agent) {
	t.Helper()
	ctx := context.Background()
	stream, err := a.Prompt(ctx, NewText("user", "hi"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	_, result := drain(t, stream)
	if result.Err != nil {
		t.Fatalf("prompt error: %v", result.Err)
	}
}

// TestThinkingBudgetsFlowsToLLMRequest verifies that AgentConfig.ThinkingBudgets
// reaches LLMRequest.ThinkingBudgets and that nil budgets produce a nil map.
func TestThinkingBudgetsFlowsToLLMRequest(t *testing.T) {
	t.Parallel()

	budgets := map[llm.ThinkingLevel]int{
		llm.ThinkingMedium: 8000,
		llm.ThinkingHigh:   16000,
	}

	tests := []struct {
		name          string
		configBudgets map[llm.ThinkingLevel]int
		wantBudgets   map[llm.ThinkingLevel]int
	}{
		{
			name:          "budgets propagate to request for active thinking level",
			configBudgets: budgets,
			wantBudgets:   budgets,
		},
		{
			name:          "nil budgets yields nil on request",
			configBudgets: nil,
			wantBudgets:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			provider := newRecordingProvider("test")
			a, err := NewAgent(AgentConfig{
				Providers:       []llm.LLMProvider{provider},
				DefaultModel:    "test/model",
				DefaultThinking: llm.ThinkingMedium,
				ThinkingBudgets: tt.configBudgets,
			})
			if err != nil {
				t.Fatalf("NewAgent: %v", err)
			}

			runOnePrompt(t, a)

			got := provider.capturedBudgets()
			if !thinkingBudgetsEqual(got, tt.wantBudgets) {
				t.Errorf("ThinkingBudgets = %v, want %v", got, tt.wantBudgets)
			}
		})
	}
}

// TestThinkingBudgetsMutationIsolation verifies that mutating the map passed to
// AgentConfig after NewAgent does not affect subsequent LLMRequests.
func TestThinkingBudgetsMutationIsolation(t *testing.T) {
	t.Parallel()

	original := map[llm.ThinkingLevel]int{llm.ThinkingMedium: 8000}

	provider := newRecordingProvider("test")
	a, err := NewAgent(AgentConfig{
		Providers:       []llm.LLMProvider{provider},
		DefaultModel:    "test/model",
		DefaultThinking: llm.ThinkingMedium,
		ThinkingBudgets: original,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	original[llm.ThinkingHigh] = 99999

	runOnePrompt(t, a)

	got := provider.capturedBudgets()
	if _, hasHighKey := got[llm.ThinkingHigh]; hasHighKey {
		t.Errorf("ThinkingBudgets[ThinkingHigh] = %d; caller mutation after NewAgent leaked into request", got[llm.ThinkingHigh])
	}
	if got[llm.ThinkingMedium] != 8000 {
		t.Errorf("ThinkingBudgets[ThinkingMedium] = %d, want 8000", got[llm.ThinkingMedium])
	}
}
