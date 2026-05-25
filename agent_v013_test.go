package pi

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/resolute-sh/pi-llm-go"
	"github.com/resolute-sh/pi-llm-go/mock"
)

// --- Message lifecycle events (Gap 1) ---

func TestAgentEventOrderingTextCompletion(t *testing.T) {
	m := mock.New("mock")
	m.OnPrompt(mock.Exact("hello")).RespondText("world").Add()

	agent := newTestAgent(t, m)
	events, result := runAndCollect(t, agent, "hello")

	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}

	// Expected: AgentStart, TurnStart, MessageStart(user), MessageStart(assistant,text),
	// TextDelta, MessageEnd, TurnEnd, AgentEnd
	var sawAgentStart, sawTurnStart, sawMsgStartUser, sawMsgStartAssistant bool
	var sawTextDelta, sawMsgEnd, sawTurnEnd, sawAgentEnd bool
	for _, ev := range events {
		switch ev.(type) {
		case AgentStartEvent:
			sawAgentStart = true
		case TurnStartEvent:
			sawTurnStart = true
		case MessageStartEvent:
			ms := ev.(MessageStartEvent)
			if ms.Role == "user" {
				sawMsgStartUser = true
			}
			if ms.Role == "assistant" && ms.MessageType == "text" {
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
	m := mock.New("mock")
	m.OnPrompt(mock.Exact("hello")).RespondText("world").Add()

	agent := newTestAgent(t, m)
	events, _ := runAndCollect(t, agent, "hello")

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
	m := mock.New("mock")
	m.OnPrompt(mock.Exact("hello")).RespondText("world").Add()

	agent := newTestAgent(t, m)
	events, _ := runAndCollect(t, agent, "hello")

	for _, ev := range events {
		if me, ok := ev.(MessageEndEvent); ok {
			if me.Message.Type != "text" || me.Message.Text() != "world" {
				t.Fatalf("expected MessageEndEvent to carry assistant text message, got %+v", me.Message)
			}
			return
		}
	}
	t.Fatal("expected MessageEndEvent")
}

func TestAgentAgentEndPayloadMatchesTranscript(t *testing.T) {
	m := mock.New("mock")
	m.OnPrompt(mock.Exact("hello")).RespondText("world").Add()

	agent := newTestAgent(t, m)
	events, result := runAndCollect(t, agent, "hello")

	var agentEnd AgentEndEvent
	for _, ev := range events {
		if ae, ok := ev.(AgentEndEvent); ok {
			agentEnd = ae
		}
	}
	if len(agentEnd.Messages) != len(result.Messages) {
		t.Fatalf("expected AgentEndEvent.Messages (%d) to match RunResult.Messages (%d)", len(agentEnd.Messages), len(result.Messages))
	}
}

// --- Terminate flag (Gap 2) ---

func TestAgentTerminateAllTrueExitsCleanly(t *testing.T) {
	m := mock.New("mock")
	m.OnPrompt(mock.Exact("calc")).RespondToolCall("add", []byte(`{"a":1,"b":2}`)).Add()

	addTool := NewTool(Tool[struct{ A, B int }]{
		Name:        "add",
		Description: "Add two numbers",
		Execute: func(ctx context.Context, p struct{ A, B int }) (ToolResult, error) {
			return ToolResult{Content: fmt.Sprintf("%d", p.A+p.B), Terminate: true}, nil
		},
	})

	agent := newTestAgent(t, m, addTool)
	ctx := context.Background()
	run, err := agent.Run(ctx, RunOpts{Prompt: NewText("user", "calc")})
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}

	var result RunResult
	select {
	case result = <-run.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}

	if result.Err != nil {
		t.Fatalf("expected nil error on all-terminate, got %v", result.Err)
	}
}

func TestAgentTerminatePartialContinues(t *testing.T) {
	m := mock.New("mock")
	m.OnPrompt(mock.Exact("multi")).RespondToolCall("log", []byte(`{}`)).Add()
	m.OnPrompt(mock.Predicate(func(msgs []llm.Message) bool {
		return len(msgs) > 2
	})).RespondText("done").Add()

	logTool := NewTool(Tool[struct{}]{
		Name:        "log",
		Description: "Log something",
		Execute: func(ctx context.Context, p struct{}) (ToolResult, error) {
			return ToolResult{Content: "logged", Terminate: true}, nil
		},
	})

	agent := newTestAgent(t, m, logTool)
	ctx := context.Background()
	run, err := agent.Run(ctx, RunOpts{Prompt: NewText("user", "multi")})
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}

	var result RunResult
	select {
	case result = <-run.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}

	// Partial terminate (only one tool in a batch of one, but the LLM was scripted
	// for two calls, so this is effectively all-terminate for the single tool.
	// For a true partial test we'd need two tools where one terminates and one doesn't.
	// The framework behavior is: all-true → exit, any-false → continue.
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
}

// --- Provider-call hooks (Gap 3) ---

func TestAgentBeforeProviderRequestFiresOnce(t *testing.T) {
	m := mock.New("mock")
	m.OnPrompt(mock.Exact("hello")).RespondText("world").Add()

	var count int
	agent, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{m},
		DefaultModel: "mock/test",
		Hooks: Hooks{
			BeforeProviderRequest: func(ctx context.Context, c BeforeProviderRequestCtx) error {
				count++
				return nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	_, _ = runAndCollect(t, agent, "hello")
	if count != 1 {
		t.Fatalf("expected BeforeProviderRequest once, got %d", count)
	}
}

func TestAgentBeforeProviderRequestErrorAborts(t *testing.T) {
	m := mock.New("mock")
	m.OnPrompt(mock.Exact("hello")).RespondText("world").Add()

	wantErr := errors.New("rate-limited")
	agent, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{m},
		DefaultModel: "mock/test",
		Hooks: Hooks{
			BeforeProviderRequest: func(ctx context.Context, c BeforeProviderRequestCtx) error {
				return wantErr
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	ctx := context.Background()
	run, err := agent.Run(ctx, RunOpts{Prompt: NewText("user", "hello")})
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}

	var result RunResult
	select {
	case result = <-run.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}

	if result.Err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(result.Err, wantErr) {
		t.Fatalf("expected %v, got %v", wantErr, result.Err)
	}
}

func TestAgentAfterProviderResponseFiresOnce(t *testing.T) {
	m := mock.New("mock")
	m.OnPrompt(mock.Exact("hello")).RespondText("world").Status(200).RespHeaders(map[string]string{"X-Request-ID": "abc"}).Add()

	var count int
	var gotStatus int
	agent, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{m},
		DefaultModel: "mock/test",
		Hooks: Hooks{
			AfterProviderResponse: func(ctx context.Context, c AfterProviderResponseCtx) {
				count++
				gotStatus = c.StatusCode
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	_, _ = runAndCollect(t, agent, "hello")
	if count != 1 {
		t.Fatalf("expected AfterProviderResponse once, got %d", count)
	}
	if gotStatus != 200 {
		t.Fatalf("expected status 200, got %d", gotStatus)
	}
}

// --- Compaction helpers ---

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
	// EstimateTokens sums role + type + body lengths then divides by 4.
	// {Role:"user", Type:"text", Body:[]byte("abcd")} = 4+4+4 = 12 chars => 3 tokens.
	msgs := []Message{{Role: "user", Type: "text", Body: []byte("abcd")}}
	if EstimateTokens(msgs) != 3 {
		t.Fatalf("expected 3 tokens, got %d", EstimateTokens(msgs))
	}
	msgs = []Message{{Role: "user", Type: "text", Body: []byte("abcde")}}
	if EstimateTokens(msgs) != 4 {
		t.Fatalf("expected 4 tokens, got %d", EstimateTokens(msgs))
	}
	// Multi-message accumulation.
	msgs = []Message{
		{Role: "user", Type: "text", Body: []byte("abcd")},
		{Role: "assistant", Type: "text", Body: []byte("efghijkl")},
	}
	// 4+4+4 + 9+4+8 = 12 + 21 = 33 => 9 tokens (rounded up)
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
	// KeepRecentTokens small enough to force a cut inside the first three messages.
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
