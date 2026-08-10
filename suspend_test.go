package pi

import (
	"context"
	"testing"

	"github.com/dev-resolute/resolute-llm-go"
)

// A Suspend result persists no tool_result — the pending call is the
// suspension point — and ends the prompt without auto-continuing.
func TestSuspendResultSkipsPersistAndEndsPrompt(t *testing.T) {
	t.Parallel()

	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: "echo", Args: echoArgs("ping")}
			events <- llm.ToolCallStartEvent{CallID: "c2", ToolName: "hold", Args: echoArgs("hold")}
			events <- llm.ToolCallEndEvent{CallID: "c1", ToolName: "echo", Args: echoArgs("ping")}
			events <- llm.ToolCallEndEvent{CallID: "c2", ToolName: "hold", Args: echoArgs("hold")}
			events <- llm.MessageEndEvent{}
		},
	}
	echo := NewTool(Tool[echoParams]{
		Name: "echo", Description: "echo",
		Execute: func(ctx context.Context, p echoParams) (ToolResult, error) {
			return ToolResult{Content: "echoed:" + p.Value}, nil
		},
	})
	hold := NewTool(Tool[echoParams]{
		Name: "hold", Description: "suspends",
		Execute: func(ctx context.Context, p echoParams) (ToolResult, error) {
			return ToolResult{Suspend: true}, nil
		},
	})
	a, err := NewAgent(AgentConfig{
		Providers: []llm.LLMProvider{provider}, DefaultModel: "test/model",
		Tools: []RegisteredTool{echo, hold},
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
	if !result.Suspended {
		t.Error("result.Suspended = false, want true")
	}
	if got := provider.callCount(); got != 1 {
		t.Errorf("provider calls = %d, want 1 (no auto-continue past suspension)", got)
	}
	// transcript: both tool_calls persisted; tool_result only for the sibling
	var results, calls int
	for _, m := range result.Messages {
		switch m.Type {
		case "tool_call":
			calls++
		case "tool_result":
			results++
			callID, _, content, _, _, _ := m.ToolResult()
			if callID != "c1" || content != "echoed:ping" {
				t.Errorf("persisted result = (%q, %q), want (c1, echoed:ping)", callID, content)
			}
		}
	}
	if calls != 2 || results != 1 {
		t.Errorf("transcript has %d calls / %d results, want 2 / 1", calls, results)
	}
}

func TestNoSuspendLeavesFlagClear(t *testing.T) {
	t.Parallel()
	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			events <- llm.TextDeltaEvent{Delta: "done"}
			events <- llm.MessageEndEvent{}
		},
	}
	a, err := NewAgent(AgentConfig{Providers: []llm.LLMProvider{provider}, DefaultModel: "test/model"})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	_, result := drain(t, stream)
	if result.Suspended {
		t.Error("result.Suspended = true for a normal turn, want false")
	}
}
