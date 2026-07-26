package pi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dev-resolute/resolute-llm-go"
)

// TestToolArgsFlowFromEndEvent pins the openai-compat-shaped event contract:
// a provider that streams tool-call args incrementally emits ToolCallStartEvent
// with nil Args and the finalized call on ToolCallEndEvent. The agent must
// execute with the finalized args (before v0.9.0 it collected at the start
// event and openai-compat tools ran with nil args).
func TestToolArgsFlowFromEndEvent(t *testing.T) {
	t.Parallel()

	var gotArgs json.RawMessage
	provider := &loopProvider{emit: func(call int, req llm.LLMRequest, events chan<- llm.LLMEvent) {
		if call == 1 {
			events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: "echo", Args: nil}
			events <- llm.ToolCallEndEvent{CallID: "c1", ToolName: "echo", Args: echoArgs("hi")}
			events <- llm.MessageEndEvent{StopReason: llm.StopReasonToolUse}
			return
		}
		events <- llm.TextDeltaEvent{Delta: "done"}
		events <- llm.MessageEndEvent{StopReason: llm.StopReasonStop}
	}}

	echo := NewTool(Tool[echoParams]{
		Name:        "echo",
		Description: "echo",
		Execute: func(ctx context.Context, p echoParams) (ToolResult, error) {
			gotArgs = echoArgs(p.Value)
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
	events, result := drain(t, stream)
	if result.Err != nil {
		t.Fatalf("result.Err = %v, want nil", result.Err)
	}

	wantArgs := echoArgs("hi")
	if string(gotArgs) != string(wantArgs) {
		t.Errorf("tool executed with args %s, want %s", gotArgs, wantArgs)
	}

	var sawStart bool
	for _, ev := range events {
		if tc, ok := ev.(ToolCallStartEvent); ok {
			sawStart = true
			if string(tc.Args) != string(wantArgs) {
				t.Errorf("agent-level ToolCallStartEvent.Args = %s, want %s", tc.Args, wantArgs)
			}
		}
	}
	if !sawStart {
		t.Fatal("no agent-level ToolCallStartEvent observed")
	}

	var sawTranscriptCall bool
	for _, m := range result.Messages {
		if m.Type != "tool_call" {
			continue
		}
		callID, toolName, args, ok := m.ToolCall()
		if !ok || callID != "c1" || toolName != "echo" {
			continue
		}
		sawTranscriptCall = true
		if string(args) != string(wantArgs) {
			t.Errorf("transcript tool_call args = %s, want %s", args, wantArgs)
		}
	}
	if !sawTranscriptCall {
		t.Fatalf("no tool_call transcript entry for c1/echo: %+v", result.Messages)
	}
}
