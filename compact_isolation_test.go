package pi

import (
	"context"
	"strings"
	"testing"

	"github.com/dev-resolute/resolute-llm-go"
)

// TestSummarizationRequestsCarryNoSessionID pins upstream #6618 parity:
// turn requests carry the run's SessionID (LLM-3 wiring: affinity headers +
// prompt_cache_key downstream), while summarization requests must stay cache-
// and affinity-isolated. Our summarizeOnce achieves upstream's "fresh routing
// id + cacheRetention none" by sending no SessionID at all; this test keeps
// LLM-3 wiring from ever leaking the turn's session id into summary calls.
func TestSummarizationRequestsCarryNoSessionID(t *testing.T) {
	ctx := context.Background()

	// --- Turn request, then ordinary (single-call) compact on the same
	// agent and session. ---
	session := newInternalMemorySession()
	sid, err := session.Create(ctx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	long := strings.Repeat("bicycle history that should be summarized away. ", 40)
	for i := 0; i < 6; i++ {
		if err := session.Append(ctx, sid, NewText("user", long), NewText("assistant", long)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			if call == 1 {
				events <- llm.TextDeltaEvent{Delta: "ok"}
				events <- llm.MessageEndEvent{StopReason: llm.StopReasonStop}
				return
			}
			events <- llm.TextDeltaEvent{Delta: "## Goal\nsummarized"}
			events <- llm.MessageEndEvent{}
		},
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
	defer agent.Close()

	stream, err := agent.Prompt(ctx, NewText("user", "go"), PromptOpts{SessionID: sid})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	_, result := drain(t, stream)
	if result.Err != nil {
		t.Fatalf("Prompt result.Err = %v, want nil", result.Err)
	}

	turnReq, ok := provider.requestForCall(1)
	if !ok {
		t.Fatal("no turn request recorded")
	}
	if turnReq.SessionID == "" {
		t.Error("turn request SessionID is empty, want the run's session id")
	}
	if turnReq.SessionID != string(sid) {
		t.Errorf("turn request SessionID = %q, want %q", turnReq.SessionID, sid)
	}

	compactResult, err := agent.Compact(ctx, CompactOpts{SessionID: sid})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if compactResult.RemovedCount == 0 {
		t.Fatal("Compact removed nothing, want a summarized prefix (fixture must trigger compaction)")
	}

	callCount := provider.callCount()
	if callCount < 2 {
		t.Fatalf("provider called %d times, want the turn call plus at least one summarization call", callCount)
	}
	for call := 2; call <= callCount; call++ {
		req, ok := provider.requestForCall(call)
		if !ok {
			t.Fatalf("no request recorded for call %d", call)
		}
		if req.SessionID != "" {
			t.Errorf("summarization request (call %d) SessionID = %q, want empty", call, req.SessionID)
		}
	}

	// --- Split-turn path: both summarization requests must also carry no
	// SessionID. ---
	splitSession, splitSID := buildMidTurnSession(t)
	splitProvider := &loopProvider{
		emit: func(_ int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			events <- llm.TextDeltaEvent{Delta: "## Goal\nsummarized"}
			events <- llm.MessageEndEvent{}
		},
	}
	splitAgent := newCompactionAgent(t, splitProvider, splitSession, 1)

	if _, err := splitAgent.Compact(ctx, CompactOpts{SessionID: splitSID}); err != nil {
		t.Fatalf("split-turn Compact: %v", err)
	}
	if got := splitProvider.callCount(); got != 2 {
		t.Fatalf("split-turn provider calls = %d, want 2 (history + turn prefix)", got)
	}
	for call := 1; call <= 2; call++ {
		req, ok := splitProvider.requestForCall(call)
		if !ok {
			t.Fatalf("no split-turn request recorded for call %d", call)
		}
		if req.SessionID != "" {
			t.Errorf("split-turn summarization request (call %d) SessionID = %q, want empty", call, req.SessionID)
		}
	}
}
