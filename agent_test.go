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

func newTestAgent(t *testing.T, provider llm.LLMProvider, tools ...RegisteredTool) *Agent {
	t.Helper()
	if provider == nil {
		provider = mock.New("mock")
	}
	agent, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "mock/test",
		Tools:        tools,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	return agent
}

func runAndCollect(t *testing.T, agent *Agent, prompt string) ([]AgentEvent, RunResult) {
	t.Helper()
	ctx := context.Background()
	run, err := agent.Run(ctx, RunOpts{Prompt: NewText("user", prompt)})
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}

	var events []AgentEvent
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range run.Events() {
			events = append(events, ev)
		}
	}()

	var result RunResult
	select {
	case result = <-run.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for run.Done")
	}
	<-done
	return events, result
}

func TestNewAgentDefaults(t *testing.T) {
	agent := newTestAgent(t, nil)
	if agent == nil {
		t.Fatal("agent is nil")
	}
}

func TestAgentRunTextCompletion(t *testing.T) {
	m := mock.New("mock")
	m.OnPrompt(mock.Exact("hello")).RespondText("world").Add()

	agent := newTestAgent(t, m)
	events, result := runAndCollect(t, agent, "hello")

	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}

	var text string
	for _, ev := range events {
		if td, ok := ev.(TextDeltaEvent); ok {
			text += td.Delta
		}
	}
	if text != "world" {
		t.Fatalf("expected 'world', got %q", text)
	}
}

func TestAgentRunToolCall(t *testing.T) {
	m := mock.New("mock")
	m.OnPrompt(mock.Exact("calc")).RespondToolCall("add", []byte(`{"a":1,"b":2}`)).Add()
	m.OnPrompt(mock.Predicate(func(msgs []llm.Message) bool {
		return len(msgs) > 2
	})).RespondText("3").Add()

	addTool := NewTool(Tool[struct{ A, B int }]{
		Name:        "add",
		Description: "Add two numbers",
		Execute: func(ctx context.Context, p struct{ A, B int }) (ToolResult, error) {
			return ToolResult{Content: fmt.Sprintf("%d", p.A+p.B)}, nil
		},
	})

	agent := newTestAgent(t, m, addTool)

	ctx := context.Background()
	run, err := agent.Run(ctx, RunOpts{Prompt: NewText("user", "calc")})
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}

	var events []AgentEvent
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range run.Events() {
			events = append(events, ev)
		}
	}()

	var result RunResult
	select {
	case result = <-run.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
	<-done

	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}

	var foundToolStart, foundToolEnd bool
	for _, ev := range events {
		if _, ok := ev.(ToolCallStartEvent); ok {
			foundToolStart = true
		}
		if _, ok := ev.(ToolCallEndEvent); ok {
			foundToolEnd = true
		}
	}
	if !foundToolStart {
		t.Fatal("expected ToolCallStartEvent")
	}
	if !foundToolEnd {
		t.Fatal("expected ToolCallEndEvent")
	}
}

func TestAgentRunStop(t *testing.T) {
	m := mock.New("mock")
	m.OnPrompt(mock.Exact("slow")).RespondText("done").Delay(500 * time.Millisecond).Add()

	agent := newTestAgent(t, m)

	ctx := context.Background()
	run, err := agent.Run(ctx, RunOpts{Prompt: NewText("user", "slow")})
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}

	// Stop immediately
	run.Stop()

	var result RunResult
	select {
	case result = <-run.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for done")
	}

	if result.Err == nil {
		t.Fatal("expected error after Stop")
	}
	if !errors.Is(result.Err, ErrRunStopped) {
		t.Fatalf("expected ErrRunStopped, got %v", result.Err)
	}
}

func TestAgentRunState(t *testing.T) {
	m := mock.New("mock")
	m.OnPrompt(mock.Exact("hi")).RespondText("hello").Add()

	agent := newTestAgent(t, m)
	_, result := runAndCollect(t, agent, "hi")

	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
}
