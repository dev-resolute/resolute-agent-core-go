package pi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dev-resolute/resolute-llm-go"
)

// buildMidTurnSession creates a session whose cut point lands right after a
// tool_call — mid-turn — forcing Agent.Compact onto the splitTurnSummarize
// path. Mirrors the fixture in compact_prompt_order_test.go's "split-turn"
// subtest.
func buildMidTurnSession(t *testing.T) (SessionRepo, SessionID) {
	t.Helper()
	ctx := context.Background()
	session := newInternalMemorySession()
	sid, err := session.Create(ctx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	long := strings.Repeat("bicycle history that should be summarized away. ", 40)
	for i := 0; i < 4; i++ {
		if err := session.Append(ctx, sid, NewText("user", long), NewText("assistant", long)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	toolCall := NewToolCall("assistant", "call_1", "research", json.RawMessage(`{"q":"bicycles"}`))
	if err := session.Append(ctx, sid, toolCall, NewText("assistant", "kept tail")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	return session, sid
}

// sequencedProvider records, under its own mutex, a "start<N>"/"end<N>" marker
// pair around each Stream call (N is the 1-based call order) plus the request
// each call received. Each call sleeps briefly between its start and end
// marker so that two calls running concurrently would very likely interleave
// their markers instead of completing one after the other.
type sequencedProvider struct {
	mu       sync.Mutex
	calls    int
	markers  []string
	requests [][]llm.Message
}

func (*sequencedProvider) Name() string { return "test" }

func (*sequencedProvider) Capabilities(string) llm.ProviderCapabilities {
	return llm.ProviderCapabilities{Streaming: true}
}

func (p *sequencedProvider) Stream(ctx context.Context, req llm.LLMRequest) llm.EventStream {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.requests = append(p.requests, req.Messages)
	p.markers = append(p.markers, fmt.Sprintf("start%d", n))
	p.mu.Unlock()

	time.Sleep(5 * time.Millisecond)

	p.mu.Lock()
	p.markers = append(p.markers, fmt.Sprintf("end%d", n))
	p.mu.Unlock()

	events := make(chan llm.LLMEvent, 2)
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

func (p *sequencedProvider) snapshot() ([]string, [][]llm.Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	markers := make([]string, len(p.markers))
	copy(markers, p.markers)
	requests := make([][]llm.Message, len(p.requests))
	copy(requests, p.requests)
	return markers, requests
}

// TestSplitTurnSummarizationIsSequential pins upstream #5536: the two
// split-turn summarization calls run strictly one after the other (history
// first), and never interleave.
func TestSplitTurnSummarizationIsSequential(t *testing.T) {
	session, sid := buildMidTurnSession(t)
	provider := &sequencedProvider{}
	agent := newCompactionAgent(t, provider, session, 1)

	if _, err := agent.Compact(context.Background(), CompactOpts{SessionID: sid}); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	markers, requests := provider.snapshot()
	wantMarkers := []string{"start1", "end1", "start2", "end2"}
	if len(markers) != len(wantMarkers) {
		t.Fatalf("markers = %v, want %v", markers, wantMarkers)
	}
	for i, m := range wantMarkers {
		if markers[i] != m {
			t.Fatalf("markers = %v, want %v (calls interleaved instead of running strictly in sequence)", markers, wantMarkers)
		}
	}

	if len(requests) != 2 {
		t.Fatalf("captured %d requests, want 2 (history + turn prefix)", len(requests))
	}
	if got := messageText(t, requests[0][len(requests[0])-1]); got != SummarizationPrompt {
		t.Errorf("request 1 last message = %.60q..., want SummarizationPrompt", got)
	}
	if got := messageText(t, requests[1][len(requests[1])-1]); got != TurnPrefixSummarizationPrompt {
		t.Errorf("request 2 last message = %.60q..., want TurnPrefixSummarizationPrompt", got)
	}
}

// TestSplitTurnHistoryErrorShortCircuits pins upstream #5536: a history
// summarization failure must short-circuit split-turn summarization — the
// turn-prefix call must never be issued.
func TestSplitTurnHistoryErrorShortCircuits(t *testing.T) {
	session, sid := buildMidTurnSession(t)
	provider := &flakySummarizingProvider{failures: 99, failErr: fmt.Errorf("gemini: %w", llm.ErrProviderFatal)}
	agent := newCompactionAgent(t, provider, session, 1)

	_, err := agent.Compact(context.Background(), CompactOpts{SessionID: sid})
	if err == nil {
		t.Fatal("Compact should fail when history summarization fails")
	}
	if !strings.Contains(err.Error(), "history summarization") {
		t.Errorf("Compact error = %v, want it to wrap %q", err, "history summarization")
	}
	if got := provider.callCount(); got != 1 {
		t.Errorf("provider calls = %d, want 1 (turn-prefix call must never be issued)", got)
	}
}

// scriptedSummaryProvider returns a fixed summary text per call, in order.
type scriptedSummaryProvider struct {
	mu    sync.Mutex
	calls int
	texts []string
}

func (*scriptedSummaryProvider) Name() string { return "test" }

func (*scriptedSummaryProvider) Capabilities(string) llm.ProviderCapabilities {
	return llm.ProviderCapabilities{Streaming: true}
}

func (p *scriptedSummaryProvider) Stream(ctx context.Context, req llm.LLMRequest) llm.EventStream {
	p.mu.Lock()
	text := p.texts[p.calls]
	p.calls++
	p.mu.Unlock()

	events := make(chan llm.LLMEvent, 2)
	done := make(chan llm.StreamResult, 1)
	events <- llm.TextDeltaEvent{Delta: text}
	events <- llm.MessageEndEvent{}
	close(events)
	done <- llm.StreamResult{Messages: append(req.Messages, llm.Message{
		Role:    "assistant",
		Content: llm.TextContent{Text: text},
	})}
	close(done)
	return llm.NewEventStream(events, done)
}

// TestSplitTurnSummaryJoinFormat pins the upstream model-facing join
// template (compaction.ts:790) byte-for-byte.
func TestSplitTurnSummaryJoinFormat(t *testing.T) {
	session, sid := buildMidTurnSession(t)
	provider := &scriptedSummaryProvider{texts: []string{"HIST", "TURN"}}
	agent := newCompactionAgent(t, provider, session, 1)

	res, err := agent.Compact(context.Background(), CompactOpts{SessionID: sid})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	want := "HIST\n\n---\n\n**Turn Context (split turn):**\n\nTURN"
	if got := res.Summary.Summary; got != want {
		t.Errorf("BranchSummary.Summary = %q, want %q", got, want)
	}
}
