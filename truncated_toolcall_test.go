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

// TestLengthTruncatedToolCallsFailWithoutExecuting ports upstream
// agent-loop.test.ts "should not execute tool calls from a length-truncated
// assistant message": a StopReasonLength message's calls are never executed;
// each gets a synthesized error result and the loop continues so the model
// can re-issue the call.
func TestLengthTruncatedToolCallsFailWithoutExecuting(t *testing.T) {
	t.Parallel()

	executed := 0
	provider := &loopProvider{emit: func(call int, req llm.LLMRequest, events chan<- llm.LLMEvent) {
		if call == 1 {
			events <- llm.ToolCallEndEvent{CallID: "c1", ToolName: "echo", Args: echoArgs("hel")}
			events <- llm.MessageEndEvent{StopReason: llm.StopReasonLength}
			return
		}
		events <- llm.TextDeltaEvent{Delta: "done"}
		events <- llm.MessageEndEvent{StopReason: llm.StopReasonStop}
	}}

	echo := NewTool(Tool[echoParams]{
		Name:        "echo",
		Description: "echo",
		Execute: func(ctx context.Context, p echoParams) (ToolResult, error) {
			executed++
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

	if executed != 0 {
		t.Errorf("executed = %d, want 0 (truncated tool call must never run)", executed)
	}
	if got := provider.callCount(); got != 2 {
		t.Fatalf("provider called %d times, want 2 (loop must continue and re-prompt)", got)
	}

	wantMsg := `Tool call "echo" was not executed: the response hit the output token limit, so its arguments may be truncated. Re-issue the tool call with complete arguments.`
	req, ok := provider.requestForCall(2)
	if !ok {
		t.Fatal("no second request recorded")
	}
	if !requestHasToolResult(req, "c1", wantMsg) {
		t.Errorf("second LLM request did not carry the synthesized error tool_result for c1: %+v", req.Messages)
	}

	var sawStart, sawEnd bool
	for _, ev := range events {
		switch te := ev.(type) {
		case ToolCallStartEvent:
			if te.CallID == "c1" {
				sawStart = true
			}
		case ToolCallEndEvent:
			if te.CallID == "c1" {
				sawEnd = true
				if te.ToolName != "echo" {
					t.Errorf("ToolCallEndEvent.ToolName = %q, want %q", te.ToolName, "echo")
				}
				if !te.Result.IsError {
					t.Errorf("ToolCallEndEvent.Result.IsError = false, want true")
				}
				if te.Result.Content != wantMsg {
					t.Errorf("ToolCallEndEvent.Result.Content = %q, want %q", te.Result.Content, wantMsg)
				}
			}
		case ToolUpdateEvent:
			if te.CallID == "c1" {
				t.Error("observed ToolUpdateEvent for c1, want none (truncated call must never execute)")
			}
		}
	}
	if !sawStart {
		t.Fatal("no agent-level ToolCallStartEvent observed for c1")
	}
	if !sawEnd {
		t.Fatal("no agent-level ToolCallEndEvent observed for c1")
	}
}
