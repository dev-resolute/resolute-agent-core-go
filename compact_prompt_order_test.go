package pi

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/dev-resolute/resolute-llm-go"
)

// capturingProvider records every summarization request it receives and
// answers with a fixed summary. Calls are serialized per turn (v0.9.0).
type capturingProvider struct {
	mu       sync.Mutex
	requests [][]llm.Message
}

func (*capturingProvider) Name() string { return "test" }

func (*capturingProvider) Capabilities(string) llm.ProviderCapabilities {
	return llm.ProviderCapabilities{Streaming: true}
}

func (p *capturingProvider) Stream(ctx context.Context, req llm.LLMRequest) llm.EventStream {
	p.mu.Lock()
	p.requests = append(p.requests, req.Messages)
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

func (p *capturingProvider) captured() [][]llm.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]llm.Message, len(p.requests))
	copy(out, p.requests)
	return out
}

func messageText(t *testing.T, m llm.Message) string {
	t.Helper()
	tc, ok := m.Content.(llm.TextContent)
	if !ok {
		return ""
	}
	return tc.Text
}

// assertInstructionLast pins AGENT-17: the summarization instruction (whose
// wording says "the messages above") must be the LAST message of the request,
// with the system prompt first and the transcript in between.
func assertInstructionLast(t *testing.T, req []llm.Message, instruction, transcriptMarker string) {
	t.Helper()
	if len(req) < 3 {
		t.Fatalf("summarization request has %d messages, want >= 3 (system, transcript..., instruction)", len(req))
	}
	first := req[0]
	if first.Role != "system" || messageText(t, first) != SummarizationSystemPrompt {
		t.Errorf("request[0] = role %q, text %.60q..., want the summarization system prompt", first.Role, messageText(t, first))
	}
	last := req[len(req)-1]
	if last.Role != "user" || messageText(t, last) != instruction {
		t.Errorf("last message = role %q, text %.60q..., want the user instruction (prompt says the transcript is above it)", last.Role, messageText(t, last))
	}
	var sawTranscript bool
	for _, m := range req[1 : len(req)-1] {
		if strings.Contains(messageText(t, m), transcriptMarker) {
			sawTranscript = true
			break
		}
	}
	if !sawTranscript {
		t.Errorf("no transcript message containing %q found between system prompt and instruction", transcriptMarker)
	}
}

func newCompactionAgent(t *testing.T, provider llm.LLMProvider, session SessionRepo, keepRecentTokens int) *Agent {
	t.Helper()
	agent, err := NewAgent(AgentConfig{
		Providers:        []llm.LLMProvider{provider},
		DefaultModel:     "test/model",
		Session:          session,
		KeepRecentTokens: keepRecentTokens,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { agent.Close() })
	return agent
}

// TestSummarizationInstructionFollowsTranscript covers all three summarization
// paths: first summary, update of an existing summary, and split-turn.
func TestSummarizationInstructionFollowsTranscript(t *testing.T) {
	ctx := context.Background()
	long := strings.Repeat("bicycle history that should be summarized away. ", 40)

	t.Run("first", func(t *testing.T) {
		provider := &capturingProvider{}
		session := newInternalMemorySession()
		sid, err := session.Create(ctx)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		for i := 0; i < 6; i++ {
			if err := session.Append(ctx, sid, NewText("user", long), NewText("assistant", long)); err != nil {
				t.Fatalf("Append: %v", err)
			}
		}
		agent := newCompactionAgent(t, provider, session, 50)
		if _, err := agent.Compact(ctx, CompactOpts{SessionID: sid}); err != nil {
			t.Fatalf("Compact: %v", err)
		}
		reqs := provider.captured()
		if len(reqs) != 1 {
			t.Fatalf("captured %d summarization requests, want 1", len(reqs))
		}
		assertInstructionLast(t, reqs[0], SummarizationPrompt, "bicycle history")
	})

	t.Run("update", func(t *testing.T) {
		provider := &capturingProvider{}
		session := newInternalMemorySession()
		sid, err := session.Create(ctx)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		for i := 0; i < 6; i++ {
			if err := session.Append(ctx, sid, NewText("user", long), NewText("assistant", long)); err != nil {
				t.Fatalf("Append: %v", err)
			}
		}
		agent := newCompactionAgent(t, provider, session, 50)
		if _, err := agent.Compact(ctx, CompactOpts{SessionID: sid}); err != nil {
			t.Fatalf("first Compact: %v", err)
		}
		for i := 0; i < 6; i++ {
			if err := session.Append(ctx, sid, NewText("user", long), NewText("assistant", long)); err != nil {
				t.Fatalf("Append: %v", err)
			}
		}
		if _, err := agent.Compact(ctx, CompactOpts{SessionID: sid}); err != nil {
			t.Fatalf("second Compact: %v", err)
		}
		reqs := provider.captured()
		if len(reqs) != 2 {
			t.Fatalf("captured %d summarization requests, want 2", len(reqs))
		}
		req := reqs[1]
		assertInstructionLast(t, req, UpdateSummarizationPrompt, "bicycle history")
		// The previous summary must precede the new transcript messages.
		if !strings.Contains(messageText(t, req[1]), "<summary>") {
			t.Errorf("request[1] = %.60q..., want the previous branch summary right after the system prompt", messageText(t, req[1]))
		}
	})

	t.Run("split-turn", func(t *testing.T) {
		provider := &capturingProvider{}
		session := newInternalMemorySession()
		sid, err := session.Create(ctx)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Long history ending in a tool_call, then one short kept message:
		// the cut lands right after the tool_call, forcing split-turn.
		for i := 0; i < 4; i++ {
			if err := session.Append(ctx, sid, NewText("user", long), NewText("assistant", long)); err != nil {
				t.Fatalf("Append: %v", err)
			}
		}
		toolCall := NewToolCall("assistant", "call_1", "research", json.RawMessage(`{"q":"bicycles"}`))
		if err := session.Append(ctx, sid, toolCall, NewText("assistant", "kept tail")); err != nil {
			t.Fatalf("Append: %v", err)
		}
		agent := newCompactionAgent(t, provider, session, 1)
		if _, err := agent.Compact(ctx, CompactOpts{SessionID: sid}); err != nil {
			t.Fatalf("Compact: %v", err)
		}
		reqs := provider.captured()
		if len(reqs) != 2 {
			t.Fatalf("captured %d summarization requests, want 2 (history + turn prefix)", len(reqs))
		}
		var sawHistory, sawTurnPrefix bool
		for _, req := range reqs {
			switch messageText(t, req[len(req)-1]) {
			case SummarizationPrompt:
				sawHistory = true
				assertInstructionLast(t, req, SummarizationPrompt, "bicycle history")
			case TurnPrefixSummarizationPrompt:
				sawTurnPrefix = true
				assertInstructionLast(t, req, TurnPrefixSummarizationPrompt, "bicycle history")
			}
		}
		if !sawHistory || !sawTurnPrefix {
			t.Errorf("split-turn requests missing an instruction as last message: history=%v turnPrefix=%v", sawHistory, sawTurnPrefix)
		}
	})
}
