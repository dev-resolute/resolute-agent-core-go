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

// An errored sibling (unknown tool) in a suspending batch still persists its
// error result; only the Suspend-marked call persists nothing, and the prompt
// still suspends.
func TestSuspendBatchErrorSiblingPersists(t *testing.T) {
	t.Parallel()

	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: "ghost", Args: echoArgs("x")}
			events <- llm.ToolCallStartEvent{CallID: "c2", ToolName: "hold", Args: echoArgs("x")}
			events <- llm.ToolCallEndEvent{CallID: "c1", ToolName: "ghost", Args: echoArgs("x")}
			events <- llm.ToolCallEndEvent{CallID: "c2", ToolName: "hold", Args: echoArgs("x")}
			events <- llm.MessageEndEvent{}
		},
	}
	hold := NewTool(Tool[echoParams]{
		Name: "hold", Description: "suspends",
		Execute: func(ctx context.Context, p echoParams) (ToolResult, error) {
			return ToolResult{Suspend: true}, nil
		},
	})
	a, err := NewAgent(AgentConfig{
		Providers: []llm.LLMProvider{provider}, DefaultModel: "test/model",
		Tools: []RegisteredTool{hold}, // "ghost" deliberately unregistered
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
	var results int
	for _, m := range result.Messages {
		if m.Type != "tool_result" {
			continue
		}
		results++
		callID, _, content, _, isErr, _ := m.ToolResult()
		if callID != "c1" || !isErr || content != ErrToolNotFound.Error() {
			t.Errorf("persisted result = (%q, %q, err=%v), want (c1, %q, err=true)",
				callID, content, isErr, ErrToolNotFound.Error())
		}
	}
	if results != 1 {
		t.Errorf("transcript has %d results, want 1 (only the errored sibling)", results)
	}
}

// ShouldStopAfterTurn is still evaluated on a suspend turn — its side effects
// run on every completed turn — even though the suspend exit ignores its answer.
func TestSuspendEvaluatesShouldStopAfterTurn(t *testing.T) {
	t.Parallel()

	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: "hold", Args: echoArgs("x")}
			events <- llm.ToolCallEndEvent{CallID: "c1", ToolName: "hold", Args: echoArgs("x")}
			events <- llm.MessageEndEvent{}
		},
	}
	hold := NewTool(Tool[echoParams]{
		Name: "hold", Description: "suspends",
		Execute: func(ctx context.Context, p echoParams) (ToolResult, error) {
			return ToolResult{Suspend: true}, nil
		},
	})
	type hookCall struct {
		turn         int
		hadToolCalls bool
	}
	var fired []hookCall
	a, err := NewAgent(AgentConfig{
		Providers: []llm.LLMProvider{provider}, DefaultModel: "test/model",
		Tools: []RegisteredTool{hold},
		Hooks: Hooks{
			ShouldStopAfterTurn: func(ctx context.Context, c AfterTurnCtx) bool {
				fired = append(fired, hookCall{turn: c.Turn, hadToolCalls: c.HadToolCalls})
				return false
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	_, result := drain(t, stream)

	if !result.Suspended {
		t.Fatal("result.Suspended = false, want true")
	}
	if len(fired) != 1 || fired[0] != (hookCall{turn: 1, hadToolCalls: true}) {
		t.Errorf("ShouldStopAfterTurn calls = %v, want exactly [{1 true}]", fired)
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
