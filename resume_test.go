package pi

import (
	"context"
	"errors"
	"testing"

	"github.com/dev-resolute/resolute-llm-go"
)

// Resume continues the loop from a tool-result tail without appending input:
// the wake path of a suspended prompt (AGENT-25).
func TestResumeAfterSuspend(t *testing.T) {
	t.Parallel()

	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			if call == 1 {
				events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: "hold", Args: echoArgs("x")}
				events <- llm.ToolCallEndEvent{CallID: "c1", ToolName: "hold", Args: echoArgs("x")}
				events <- llm.MessageEndEvent{}
				return
			}
			events <- llm.TextDeltaEvent{Delta: "resumed answer"}
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
		Tools: []RegisteredTool{hold},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	// turn 1 suspends with c1 pending
	stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	_, result := drain(t, stream)
	if !result.Suspended {
		t.Fatalf("Suspended = false, want true")
	}
	sid := a.State().SessionID

	// the external resolution lands (harness wake authors this record)
	if err := a.session.Append(context.Background(), sid,
		NewToolResultMsg("c1", "hold", ToolResult{Content: "external answer"})); err != nil {
		t.Fatalf("append outcome: %v", err)
	}

	// Resume: the provider sees call + result and the turn completes
	stream2, err := a.Resume(context.Background(), PromptOpts{SessionID: sid})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	_, result2 := drain(t, stream2)
	if result2.Err != nil {
		t.Fatalf("result2.Err = %v, want nil", result2.Err)
	}
	req, ok := provider.requestForCall(2)
	if !ok {
		t.Fatal("no second provider call (resume must drive the loop)")
	}
	var sawResult bool
	for _, m := range req.Messages {
		if tr, ok := m.Content.(llm.ToolResultContent); ok && tr.CallID == "c1" && tr.Content == "external answer" {
			sawResult = true
		}
	}
	if !sawResult {
		t.Error("resume request missing the landed tool result")
	}
}

func TestResumePreconditions(t *testing.T) {
	t.Parallel()
	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			events <- llm.TextDeltaEvent{Delta: "hi"}
			events <- llm.MessageEndEvent{}
		},
	}
	a, err := NewAgent(AgentConfig{Providers: []llm.LLMProvider{provider}, DefaultModel: "test/model"})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	// empty session id
	if _, err := a.Resume(context.Background(), PromptOpts{}); !errors.Is(err, ErrNothingToResume) {
		t.Errorf("Resume without SessionID err = %v, want ErrNothingToResume", err)
	}
	// a session whose tail is a text message (not resumable)
	stream, _ := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
	drain(t, stream)
	if _, err := a.Resume(context.Background(), PromptOpts{SessionID: a.State().SessionID}); !errors.Is(err, ErrNothingToResume) {
		t.Errorf("Resume on text tail err = %v, want ErrNothingToResume", err)
	}
}
