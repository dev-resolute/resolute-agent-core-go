package pi

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/dev-resolute/resolute-llm-go"
)

// usageEmittingProvider is a scripted provider that answers successive
// Stream calls with a fixed summary text and, optionally, a UsageEvent. A nil
// entry in usages means that call reports no usage at all.
type usageEmittingProvider struct {
	mu     sync.Mutex
	texts  []string
	usages []*llm.UsageEvent
	calls  int
}

func (*usageEmittingProvider) Name() string { return "test" }

func (*usageEmittingProvider) Capabilities(string) llm.ProviderCapabilities {
	return llm.ProviderCapabilities{Streaming: true}
}

func (p *usageEmittingProvider) Stream(ctx context.Context, req llm.LLMRequest) llm.EventStream {
	p.mu.Lock()
	n := p.calls
	p.calls++
	p.mu.Unlock()

	text := p.texts[n]
	var usage *llm.UsageEvent
	if n < len(p.usages) {
		usage = p.usages[n]
	}

	events := make(chan llm.LLMEvent, 3)
	done := make(chan llm.StreamResult, 1)
	events <- llm.TextDeltaEvent{Delta: text}
	if usage != nil {
		events <- *usage
	}
	events <- llm.MessageEndEvent{}
	close(events)
	done <- llm.StreamResult{Messages: append(req.Messages, llm.Message{
		Role:    "assistant",
		Content: llm.TextContent{Text: text},
	})}
	close(done)
	return llm.NewEventStream(events, done)
}

// buildCompactableSession creates a session with enough history that a
// single Compact call, given KeepRecentTokens 50, produces exactly one
// (non-split-turn) summarization request.
func buildCompactableSession(t *testing.T) (SessionRepo, SessionID) {
	t.Helper()
	ctx := context.Background()
	session := newInternalMemorySession()
	sid, err := session.Create(ctx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	long := strings.Repeat("history that should be summarized away. ", 40)
	for i := 0; i < 6; i++ {
		if err := session.Append(ctx, sid, NewText("user", long), NewText("assistant", long)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	return session, sid
}

// TestCompactAttachesUsage pins upstream #6671: when the provider reports
// token usage alongside the summary text, Compact's persisted BranchSummary
// carries it, and both CompactResult and the AfterCompact hook context see
// the same value.
func TestCompactAttachesUsage(t *testing.T) {
	ctx := context.Background()
	session, sid := buildCompactableSession(t)
	provider := &usageEmittingProvider{
		texts:  []string{"## Goal\nsummarized"},
		usages: []*llm.UsageEvent{{InputTokens: 1, OutputTokens: 2}},
	}

	var afterCtx AfterCompactCtx
	var afterCalled bool
	agent, err := NewAgent(AgentConfig{
		Providers:        []llm.LLMProvider{provider},
		DefaultModel:     "test/model",
		Session:          session,
		KeepRecentTokens: 50,
		Hooks: Hooks{
			AfterCompact: func(_ context.Context, c AfterCompactCtx) error {
				afterCalled = true
				afterCtx = c
				return nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { agent.Close() })

	res, err := agent.Compact(ctx, CompactOpts{SessionID: sid})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	want := &Usage{InputTokens: 1, OutputTokens: 2}
	if res.Summary.Usage == nil || *res.Summary.Usage != *want {
		t.Errorf("CompactResult.Summary.Usage = %+v, want %+v", res.Summary.Usage, want)
	}

	if !afterCalled {
		t.Fatal("AfterCompact hook was not called")
	}
	if afterCtx.BranchSummary.Usage == nil || *afterCtx.BranchSummary.Usage != *want {
		t.Errorf("AfterCompactCtx.BranchSummary.Usage = %+v, want %+v", afterCtx.BranchSummary.Usage, want)
	}

	summaries, err := session.LoadBranchSummaries(ctx, sid)
	if err != nil {
		t.Fatalf("LoadBranchSummaries: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("persisted %d summaries, want 1", len(summaries))
	}
	if summaries[0].Usage == nil || *summaries[0].Usage != *want {
		t.Errorf("persisted BranchSummary.Usage = %+v, want %+v", summaries[0].Usage, want)
	}
}

// TestCompactUsageNilWhenUnreported pins upstream #6671: when the provider
// emits no UsageEvent at all, the persisted BranchSummary.Usage stays nil
// rather than defaulting to a zero-valued struct.
func TestCompactUsageNilWhenUnreported(t *testing.T) {
	ctx := context.Background()
	session, sid := buildCompactableSession(t)
	provider := &usageEmittingProvider{
		texts:  []string{"## Goal\nsummarized"},
		usages: []*llm.UsageEvent{nil},
	}

	agent, err := NewAgent(AgentConfig{
		Providers:        []llm.LLMProvider{provider},
		DefaultModel:     "test/model",
		Session:          session,
		KeepRecentTokens: 50,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { agent.Close() })

	res, err := agent.Compact(ctx, CompactOpts{SessionID: sid})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.Summary.Usage != nil {
		t.Errorf("CompactResult.Summary.Usage = %+v, want nil", res.Summary.Usage)
	}

	summaries, err := session.LoadBranchSummaries(ctx, sid)
	if err != nil {
		t.Fatalf("LoadBranchSummaries: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("persisted %d summaries, want 1", len(summaries))
	}
	if summaries[0].Usage != nil {
		t.Errorf("persisted BranchSummary.Usage = %+v, want nil", summaries[0].Usage)
	}
}

// TestSplitTurnCombinesUsage ports upstream compaction.test.ts "combines
// usage for split-turn compaction summaries": the history call reports
// (1, 2), the turn-prefix call reports (5, 6), and the persisted
// BranchSummary.Usage must be their sum, (6, 8) (upstream #6671).
func TestSplitTurnCombinesUsage(t *testing.T) {
	ctx := context.Background()
	session, sid := buildMidTurnSession(t)
	provider := &usageEmittingProvider{
		texts: []string{"HIST", "TURN"},
		usages: []*llm.UsageEvent{
			{InputTokens: 1, OutputTokens: 2},
			{InputTokens: 5, OutputTokens: 6},
		},
	}
	agent := newCompactionAgent(t, provider, session, 1)

	res, err := agent.Compact(ctx, CompactOpts{SessionID: sid})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	want := &Usage{InputTokens: 6, OutputTokens: 8}
	if res.Summary.Usage == nil || *res.Summary.Usage != *want {
		t.Errorf("split-turn BranchSummary.Usage = %+v, want %+v", res.Summary.Usage, want)
	}
}

// TestCombineUsage exercises combineUsage's exact semantics (upstream
// compaction.ts combineUsage): split-turn usages are summed; a single
// non-nil side stands alone; nil when both sides reported none.
func TestCombineUsage(t *testing.T) {
	tests := []struct {
		name string
		a, b *Usage
		want *Usage
	}{
		{"both nil", nil, nil, nil},
		{"only a", &Usage{InputTokens: 1, OutputTokens: 2}, nil, &Usage{InputTokens: 1, OutputTokens: 2}},
		{"only b", nil, &Usage{InputTokens: 5, OutputTokens: 6}, &Usage{InputTokens: 5, OutputTokens: 6}},
		{"both", &Usage{InputTokens: 1, OutputTokens: 2}, &Usage{InputTokens: 5, OutputTokens: 6}, &Usage{InputTokens: 6, OutputTokens: 8}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := combineUsage(tt.a, tt.b)
			if (got == nil) != (tt.want == nil) {
				t.Fatalf("combineUsage(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
			if got != nil && *got != *tt.want {
				t.Errorf("combineUsage(%v, %v) = %+v, want %+v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
