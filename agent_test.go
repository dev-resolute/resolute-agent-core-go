package pi

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

func TestAgentSessionResume(t *testing.T) {
	m := mock.New("mock")
	m.OnPrompt(mock.Exact("hello")).RespondText("world").Add()
	m.OnPrompt(mock.LastUser("resume")).RespondText("continued").Add()

	agent := newTestAgent(t, m)

	// First run
	ctx := context.Background()
	run1, err := agent.Run(ctx, RunOpts{Prompt: NewText("user", "hello")})
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	var result1 RunResult
	select {
	case result1 = <-run1.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("timeout on run1")
	}
	if result1.Err != nil {
		t.Fatalf("run1 error: %v", result1.Err)
	}
	sid := run1.State().SessionID
	if sid == "" {
		t.Fatal("expected non-empty session id from run1")
	}

	// Second run with same session
	run2, err := agent.Run(ctx, RunOpts{
		Prompt:    NewText("user", "resume"),
		SessionID: sid,
	})
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	var result2 RunResult
	select {
	case result2 = <-run2.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("timeout on run2")
	}
	if result2.Err != nil {
		t.Fatalf("run2 error: %v", result2.Err)
	}

	// Transcript should include messages from run1
	transcript := run2.Transcript()
	if len(transcript) < 3 {
		t.Fatalf("expected transcript to have messages from run1, got %d", len(transcript))
	}
}

func TestAgentDefaultSessionRoundTrip(t *testing.T) {
	m := mock.New("mock")
	m.OnPrompt(mock.Exact("hello")).RespondText("world").Add()

	agent, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{m},
		DefaultModel: "mock/test",
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

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

func TestAgentCompactNoSessionReturnsEmptyResult(t *testing.T) {
	agent := newTestAgent(t, nil)
	ctx := context.Background()
	result, err := agent.Compact(ctx, CompactOpts{})
	if err != nil {
		t.Fatalf("unexpected error from Compact: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil CompactResult")
	}
}

func TestAgentCompactPreconditionBusy(t *testing.T) {
	m := mock.New("mock")
	m.OnPrompt(mock.Exact("slow")).RespondText("done").Delay(2 * time.Second).Add()

	agent := newTestAgent(t, m)
	ctx := context.Background()

	// Start a run that will take a while
	go func() {
		_, err := agent.Run(ctx, RunOpts{Prompt: NewText("user", "slow")})
		if err != nil {
			t.Logf("run error: %v", err)
		}
	}()

	// Give the run time to start
	time.Sleep(100 * time.Millisecond)

	_, err := agent.Compact(ctx, CompactOpts{})
	if err == nil {
		t.Fatal("expected error from Compact while busy")
	}
	if !errors.Is(err, ErrAgentBusy) {
		t.Fatalf("expected ErrAgentBusy, got %v", err)
	}
}

func TestAgentConcurrentRuns(t *testing.T) {
	m := mock.New("mock")
	m.OnAny().RespondText("result a").Add()
	m.OnAny().RespondText("result b").Add()

	agent := newTestAgent(t, m)
	ctx := context.Background()

	var resultA, resultB RunResult
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		run, err := agent.Run(ctx, RunOpts{Prompt: NewText("user", "run a")})
		if err != nil {
			t.Errorf("run a: %v", err)
			return
		}
		select {
		case resultA = <-run.Done():
		case <-time.After(5 * time.Second):
			t.Errorf("timeout on run a")
		}
	}()

	go func() {
		defer wg.Done()
		run, err := agent.Run(ctx, RunOpts{Prompt: NewText("user", "run b")})
		if err != nil {
			t.Errorf("run b: %v", err)
			return
		}
		select {
		case resultB = <-run.Done():
		case <-time.After(5 * time.Second):
			t.Errorf("timeout on run b")
		}
	}()

	wg.Wait()

	if resultA.Err != nil {
		t.Fatalf("run a error: %v", resultA.Err)
	}
	if resultB.Err != nil {
		t.Fatalf("run b error: %v", resultB.Err)
	}
}
