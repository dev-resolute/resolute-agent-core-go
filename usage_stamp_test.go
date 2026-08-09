package pi

import (
	"bytes"
	"context"
	"testing"

	"github.com/dev-resolute/resolute-llm-go"
)

func TestMessageWithUsageStampsAndReads(t *testing.T) {
	t.Parallel()
	u := Usage{InputTokens: 100, OutputTokens: 42}

	// given messages of every built-in shape
	sig := []byte("sig")
	plain := NewText("assistant", "hello").WithUsage(u)
	signed := NewTextWithSignature("assistant", "hello", sig).WithUsage(u)
	call := NewToolCallWithSignature("assistant", "c1", "echo", echoArgs("ping"), sig).WithUsage(u)

	// then usage round-trips through the body, and prior fields survive
	for name, m := range map[string]Message{"plain text": plain, "signed text": signed, "tool call": call} {
		got := m.Usage()
		if got == nil || *got != u {
			t.Errorf("%s: Usage() = %+v, want %+v", name, got, u)
		}
	}
	if plain.Text() != "hello" {
		t.Errorf("plain text Text() = %q, want hello", plain.Text())
	}
	if signed.Text() != "hello" || !bytes.Equal(signed.TextThoughtSignature(), sig) {
		t.Errorf("signed text = {%q, %q}, want {hello, %q}", signed.Text(), signed.TextThoughtSignature(), sig)
	}
	callID, _, _, ok := call.ToolCall()
	if !ok || callID != "c1" || !bytes.Equal(call.ToolCallThoughtSignature(), sig) {
		t.Errorf("tool call fields after WithUsage = (%q, %v, %q), want (c1, true, %q)",
			callID, ok, call.ToolCallThoughtSignature(), sig)
	}
}

func TestMessageUsageAbsentYieldsNil(t *testing.T) {
	t.Parallel()
	// given pre-usage-transcript shapes
	for _, m := range []Message{
		NewText("assistant", "hello"),
		NewTextWithSignature("assistant", "hello", []byte("sig")),
		NewToolCall("assistant", "c1", "echo", echoArgs("ping")),
	} {
		if got := m.Usage(); got != nil {
			t.Errorf("Usage() on %s body = %+v, want nil", m.Type, got)
		}
	}
}

// The prompt loop stamps the turn's provider usage onto the last assistant
// message the turn produces — the text message for a text-only turn.
func TestTurnUsageStampsTextMessage(t *testing.T) {
	t.Parallel()

	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			events <- llm.TextDeltaEvent{Delta: "answer"}
			events <- llm.UsageEvent{InputTokens: 100, OutputTokens: 42}
			events <- llm.MessageEndEvent{}
		},
	}
	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
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
		t.Fatalf("result.Err = %v, want nil", result.Err)
	}

	var stamped bool
	for _, m := range result.Messages {
		if m.Type == "text" && m.Role == "assistant" {
			stamped = true
			got := m.Usage()
			if got == nil || *got != (Usage{InputTokens: 100, OutputTokens: 42}) {
				t.Errorf("assistant text Usage() = %+v, want {100 42}", got)
			}
		}
	}
	if !stamped {
		t.Fatal("no assistant text message in transcript")
	}
}

// For a tool-call turn the stamp lands on the last tool_call, so exactly one
// message per turn carries the turn's usage.
func TestTurnUsageStampsLastToolCall(t *testing.T) {
	t.Parallel()

	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			if call == 1 {
				events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: "echo", Args: echoArgs("ping")}
				events <- llm.ToolCallStartEvent{CallID: "c2", ToolName: "echo", Args: echoArgs("pong")}
				events <- llm.ToolCallEndEvent{CallID: "c1", ToolName: "echo", Args: echoArgs("ping")}
				events <- llm.ToolCallEndEvent{CallID: "c2", ToolName: "echo", Args: echoArgs("pong")}
				events <- llm.UsageEvent{InputTokens: 100, OutputTokens: 42}
				events <- llm.MessageEndEvent{}
				return
			}
			events <- llm.TextDeltaEvent{Delta: "done"}
			events <- llm.MessageEndEvent{}
		},
	}
	echo := NewTool(Tool[echoParams]{
		Name:        "echo",
		Description: "echo",
		Execute: func(ctx context.Context, p echoParams) (ToolResult, error) {
			return ToolResult{Content: "echoed:" + p.Value}, nil
		},
	})
	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{echo},
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
		t.Fatalf("result.Err = %v, want nil", result.Err)
	}

	want := Usage{InputTokens: 100, OutputTokens: 42}
	var stampedCalls []string
	for _, m := range result.Messages {
		if m.Type != "tool_call" {
			continue
		}
		callID, _, _, _ := m.ToolCall()
		if got := m.Usage(); got != nil {
			if *got != want {
				t.Errorf("tool_call %s Usage() = %+v, want %+v", callID, got, want)
			}
			stampedCalls = append(stampedCalls, callID)
		}
	}
	if len(stampedCalls) != 1 || stampedCalls[0] != "c2" {
		t.Errorf("stamped tool calls = %v, want exactly [c2] (the turn's last assistant message)", stampedCalls)
	}
}

func TestTurnWithoutUsageReportLeavesMessagesUnstamped(t *testing.T) {
	t.Parallel()

	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			events <- llm.TextDeltaEvent{Delta: "answer"}
			events <- llm.MessageEndEvent{}
		},
	}
	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
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
		t.Fatalf("result.Err = %v, want nil", result.Err)
	}

	for _, m := range result.Messages {
		if got := m.Usage(); got != nil {
			t.Errorf("Usage() on %s = %+v, want nil (provider reported none)", m.Type, got)
		}
	}
}
