package pi

import (
	"context"
	"strings"
	"testing"

	"github.com/dev-resolute/resolute-llm-go"
)

// summarizingProvider is a hermetic provider that answers every call with a
// fixed summary text (the shape Compact's summarization call expects).
type summarizingProvider struct{}

func (summarizingProvider) Name() string { return "test" }

func (summarizingProvider) Capabilities(string) llm.ProviderCapabilities {
	return llm.ProviderCapabilities{Streaming: true}
}

func (summarizingProvider) Stream(ctx context.Context, req llm.LLMRequest) llm.EventStream {
	events := make(chan llm.LLMEvent, 4)
	done := make(chan llm.StreamResult, 1)
	events <- llm.TextDeltaEvent{Delta: "## Goal\nsummarized"}
	events <- llm.MessageEndEvent{}
	close(events)
	done <- llm.StreamResult{Messages: append(req.Messages, llm.Message{
		Role:    "assistant",
		Content: llm.TextContent{Text: "## Goal\nsummarized"},
	})}
	close(done)
	return llm.NewEventStream(events, done)
}

// TestCompactExplicitSessionID pins the CompactOpts.SessionID contract: a
// fresh Agent that has never prompted can compact a pre-existing session
// when the caller names it explicitly (the harness projection use case).
func TestCompactExplicitSessionID(t *testing.T) {
	session := newInternalMemorySession()
	ctx := context.Background()
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

	agent, err := NewAgent(AgentConfig{
		Providers:        []llm.LLMProvider{summarizingProvider{}},
		DefaultModel:     "test/model",
		Session:          session,
		KeepRecentTokens: 50,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	defer agent.Close()

	// Without an explicit id, a never-prompted Agent has no session to
	// compact and silently no-ops.
	noop, err := agent.Compact(ctx, CompactOpts{})
	if err != nil {
		t.Fatalf("Compact (implicit): %v", err)
	}
	if noop.RemovedCount != 0 {
		t.Fatalf("implicit compact removed %d, want 0 (no bound session)", noop.RemovedCount)
	}

	result, err := agent.Compact(ctx, CompactOpts{SessionID: sid})
	if err != nil {
		t.Fatalf("Compact (explicit): %v", err)
	}
	if result.RemovedCount == 0 {
		t.Fatal("explicit-session compact removed nothing, want a summarized prefix")
	}
	summaries, err := session.LoadBranchSummaries(ctx, sid)
	if err != nil {
		t.Fatalf("LoadBranchSummaries: %v", err)
	}
	if len(summaries) != 1 || !strings.Contains(summaries[0].Summary, "summarized") {
		t.Fatalf("summaries = %+v, want one containing the model summary", summaries)
	}
}
