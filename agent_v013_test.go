package pi

import (
	"context"
	"errors"
	"testing"

	"github.com/dev-resolute/resolute-llm-go"
)

// --- Message lifecycle events (Gap 1) ---

func TestAgentEventOrderingTextCompletion(t *testing.T) {
	agent := newTestAgent(t)
	events, result := runAndCollect(t, agent, "Reply with a short greeting.")

	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}

	var sawAgentStart, sawTurnStart, sawMsgStartUser, sawMsgStartAssistant bool
	var sawTextDelta, sawMsgEnd, sawTurnEnd, sawAgentEnd bool
	for _, ev := range events {
		switch e := ev.(type) {
		case AgentStartEvent:
			sawAgentStart = true
		case TurnStartEvent:
			sawTurnStart = true
		case MessageStartEvent:
			if e.Role == "user" {
				sawMsgStartUser = true
			}
			if e.Role == "assistant" && e.MessageType == "text" {
				sawMsgStartAssistant = true
			}
		case TextDeltaEvent:
			sawTextDelta = true
		case MessageEndEvent:
			sawMsgEnd = true
		case TurnEndEvent:
			sawTurnEnd = true
		case AgentEndEvent:
			sawAgentEnd = true
		}
	}
	if !sawAgentStart {
		t.Fatal("expected AgentStartEvent")
	}
	if !sawTurnStart {
		t.Fatal("expected TurnStartEvent")
	}
	if !sawMsgStartUser {
		t.Fatal("expected MessageStartEvent for user")
	}
	if !sawMsgStartAssistant {
		t.Fatal("expected MessageStartEvent for assistant")
	}
	if !sawTextDelta {
		t.Fatal("expected TextDeltaEvent")
	}
	if !sawMsgEnd {
		t.Fatal("expected MessageEndEvent")
	}
	if !sawTurnEnd {
		t.Fatal("expected TurnEndEvent")
	}
	if !sawAgentEnd {
		t.Fatal("expected AgentEndEvent")
	}
}

func TestAgentMessageStartFiresOncePerAssistantMessage(t *testing.T) {
	agent := newTestAgent(t)
	events, _ := runAndCollect(t, agent, "Reply with a short greeting.")

	var assistantStartCount int
	for _, ev := range events {
		if ms, ok := ev.(MessageStartEvent); ok && ms.Role == "assistant" {
			assistantStartCount++
		}
	}
	if assistantStartCount != 1 {
		t.Fatalf("expected MessageStartEvent for assistant exactly once, got %d", assistantStartCount)
	}
}

func TestAgentMessageEndCarriesMessage(t *testing.T) {
	agent := newTestAgent(t)
	events, _ := runAndCollect(t, agent, "Reply with a short greeting.")

	for _, ev := range events {
		if me, ok := ev.(MessageEndEvent); ok {
			if me.Message.Type != "text" || me.Message.Text() == "" {
				t.Fatalf("expected MessageEndEvent to carry non-empty assistant text, got %+v", me.Message)
			}
			return
		}
	}
	t.Fatal("expected MessageEndEvent")
}

func TestAgentAgentEndPayloadMatchesTranscript(t *testing.T) {
	agent := newTestAgent(t)
	events, result := runAndCollect(t, agent, "Reply with a short greeting.")

	var agentEnd AgentEndEvent
	for _, ev := range events {
		if ae, ok := ev.(AgentEndEvent); ok {
			agentEnd = ae
		}
	}
	if len(agentEnd.Messages) != len(result.Messages) {
		t.Fatalf("expected AgentEndEvent.Messages (%d) to match PromptResult.Messages (%d)", len(agentEnd.Messages), len(result.Messages))
	}
}

// --- Terminate flag (Gap 2) ---

func TestAgentTerminateAllTrueExitsCleanly(t *testing.T) {
	finishTool := NewTool(Tool[struct{}]{
		Name:        "finish",
		Description: "Signal that the task is complete. Call this to finish.",
		Execute: func(ctx context.Context, p struct{}) (ToolResult, error) {
			return ToolResult{Content: "done", Terminate: true}, nil
		},
	})

	agent := newTestAgent(t, finishTool)
	_, result := runAndCollect(t, agent, "Call the finish tool now. You must call the tool.")
	if result.Err != nil {
		t.Fatalf("expected nil error on all-terminate, got %v", result.Err)
	}
}

func TestAgentTerminatePartialContinues(t *testing.T) {
	logTool := NewTool(Tool[struct{}]{
		Name:        "log",
		Description: "Record a log entry. Does not finish the task.",
		Execute: func(ctx context.Context, p struct{}) (ToolResult, error) {
			return ToolResult{Content: "logged", Terminate: false}, nil
		},
	})

	agent := newTestAgent(t, logTool)
	// A non-terminating tool does not force an early exit via the terminate
	// path; the prompt completes cleanly (Err == nil), matching v0.1.x.
	_, result := runAndCollect(t, agent,
		"Call the log tool with no arguments. You must call the tool.")
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
}

// --- Provider-call hooks (Gap 3) ---

func TestAgentBeforeProviderRequestFiresOnce(t *testing.T) {
	var count int
	agent := newTestAgentWithHooks(t, Hooks{
		BeforeProviderRequest: func(ctx context.Context, c BeforeProviderRequestCtx) error {
			count++
			return nil
		},
	})

	_, result := runAndCollect(t, agent, "Reply with a short greeting.")
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if count != 1 {
		t.Fatalf("expected BeforeProviderRequest once, got %d", count)
	}
}

func TestAgentBeforeProviderRequestErrorAborts(t *testing.T) {
	wantErr := errors.New("rate-limited")
	agent := newTestAgentWithHooks(t, Hooks{
		BeforeProviderRequest: func(ctx context.Context, c BeforeProviderRequestCtx) error {
			return wantErr
		},
	})

	_, result := runAndCollect(t, agent, "Reply with a short greeting.")
	if result.Err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(result.Err, wantErr) {
		t.Fatalf("expected %v, got %v", wantErr, result.Err)
	}
}

func TestAgentAfterProviderResponseFiresOnce(t *testing.T) {
	var count int
	agent := newTestAgentWithHooks(t, Hooks{
		AfterProviderResponse: func(ctx context.Context, c AfterProviderResponseCtx) {
			count++
		},
	})

	_, result := runAndCollect(t, agent, "Reply with a short greeting.")
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	// Fires once per provider turn. Note: the gemini provider does not surface
	// an HTTP status through the genai SDK, so StatusCode is not asserted here
	// (it is provider-specific; the mock-era status==200 assertion does not
	// carry over to a live provider).
	if count != 1 {
		t.Fatalf("expected AfterProviderResponse once, got %d", count)
	}
}

// --- Compaction helpers (pure logic, provider-independent) ---

func TestShouldCompact(t *testing.T) {
	settings := CompactionSettings{Enabled: true, ReserveTokens: 1000}
	if ShouldCompact(500, 2000, settings) {
		t.Fatal("500 tokens in 2000 window with 1000 reserve should NOT compact")
	}
	if !ShouldCompact(1100, 2000, settings) {
		t.Fatal("1100 tokens in 2000 window with 1000 reserve SHOULD compact")
	}
	if ShouldCompact(1000, 2000, settings) {
		t.Fatal("exactly at threshold should NOT compact (strict less-than)")
	}
}

func TestEstimateTokens(t *testing.T) {
	if EstimateTokens(nil) != 0 {
		t.Fatal("empty input should be 0")
	}
	msgs := []Message{{Role: "user", Type: "text", Body: []byte("abcd")}}
	if EstimateTokens(msgs) != 3 {
		t.Fatalf("expected 3 tokens, got %d", EstimateTokens(msgs))
	}
	msgs = []Message{{Role: "user", Type: "text", Body: []byte("abcde")}}
	if EstimateTokens(msgs) != 4 {
		t.Fatalf("expected 4 tokens, got %d", EstimateTokens(msgs))
	}
	msgs = []Message{
		{Role: "user", Type: "text", Body: []byte("abcd")},
		{Role: "assistant", Type: "text", Body: []byte("efghijkl")},
	}
	if got := EstimateTokens(msgs); got != 9 {
		t.Fatalf("expected 9 tokens, got %d", got)
	}
}

func TestFindCutPointSkipsToolResult(t *testing.T) {
	msgs := []Message{
		NewText("user", "a"),
		NewToolCall("assistant", "c1", "calc", []byte(`{}`)),
		NewToolResult("tool", "c1", "calc", "3", nil, false),
		NewText("user", "b"),
		NewText("assistant", "c"),
	}
	cut := findCutPoint(msgs, 1)
	if msgs[cut].Type == "tool_result" {
		t.Fatalf("cut point should never land on tool_result, got index %d (%s)", cut, msgs[cut].Type)
	}
}

func TestBuildLLMContextSubstitutesSummary(t *testing.T) {
	msgs := []Message{
		NewText("user", "old1"),
		NewText("assistant", "old2"),
		NewText("user", "old3"),
		NewText("assistant", "keep1"),
		NewText("user", "keep2"),
	}
	summaries := []BranchSummary{
		{StartIdx: 0, EndIdx: 3, Summary: "summary of first three"},
	}
	out := BuildLLMContext(msgs, summaries)
	if len(out) != 3 {
		t.Fatalf("expected 3 messages after substitution, got %d", len(out))
	}
	if out[0].Type != "branch_summary" {
		t.Fatalf("expected first message to be branch_summary, got %s", out[0].Type)
	}
	if out[1].Text() != "keep1" {
		t.Fatalf("expected second message to be 'keep1', got %s", out[1].Text())
	}
}

func TestNewBranchSummaryMessageRoundTrip(t *testing.T) {
	m := NewBranchSummaryMessage("test summary")
	if m.Type != "branch_summary" {
		t.Fatalf("expected type branch_summary, got %s", m.Type)
	}
	if m.Text() != "test summary" {
		t.Fatalf("expected text 'test summary', got %s", m.Text())
	}
}

func TestDefaultConvertToLLMBranchSummary(t *testing.T) {
	msgs := []Message{NewBranchSummaryMessage("the summary")}
	out := DefaultConvertToLLM(msgs)
	if len(out) != 1 {
		t.Fatal("expected 1 output message")
	}
	tc, ok := out[0].Content.(llm.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", out[0].Content)
	}
	if tc.Text != "<summary>the summary</summary>" {
		t.Fatalf("expected wrapped summary, got %q", tc.Text)
	}
}
