package pi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resolute-sh/pi-llm-go"
	"github.com/resolute-sh/pi-llm-go/gemini"
)

const testModel = "gemini/gemini-2.5-flash"

// liveProvider returns a real Gemini provider, skipping the test when no
// API key is configured so the suite degrades gracefully without credentials.
func liveProvider(t *testing.T) llm.LLMProvider {
	t.Helper()
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		t.Skip("GEMINI_API_KEY not set; skipping live-provider test")
	}
	p, err := gemini.New(gemini.Config{APIKey: key})
	if err != nil {
		t.Fatalf("gemini.New: %v", err)
	}
	return p
}

func newTestAgent(t *testing.T, tools ...RegisteredTool) *Agent {
	t.Helper()
	agent, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{liveProvider(t)},
		DefaultModel: testModel,
		Tools:        tools,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	return agent
}

func newTestAgentWithHooks(t *testing.T, hooks Hooks) *Agent {
	t.Helper()
	agent, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{liveProvider(t)},
		DefaultModel: testModel,
		Hooks:        hooks,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	return agent
}

// drain reads an EventStream to completion and returns the events + result.
func drain(t *testing.T, stream *EventStream) ([]AgentEvent, PromptResult) {
	t.Helper()
	var events []AgentEvent
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range stream.Events {
			events = append(events, ev)
		}
	}()
	var result PromptResult
	select {
	case result = <-stream.Done:
	case <-time.After(45 * time.Second):
		t.Fatal("timeout waiting for stream.Done")
	}
	<-done
	return events, result
}

func runAndCollect(t *testing.T, agent *Agent, prompt string) ([]AgentEvent, PromptResult) {
	t.Helper()
	stream, err := agent.Prompt(context.Background(), NewText("user", prompt), PromptOpts{})
	if err != nil {
		t.Fatalf("agent.Prompt: %v", err)
	}
	return drain(t, stream)
}

func assistantText(events []AgentEvent) string {
	var s string
	for _, ev := range events {
		if td, ok := ev.(TextDeltaEvent); ok {
			s += td.Delta
		}
	}
	return s
}

func TestNewAgentDefaults(t *testing.T) {
	agent := newTestAgent(t)
	if agent == nil {
		t.Fatal("agent is nil")
	}
}

func TestAgentRunTextCompletion(t *testing.T) {
	agent := newTestAgent(t)
	events, result := runAndCollect(t, agent, "Reply with a short greeting.")

	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if assistantText(events) == "" {
		t.Fatal("expected non-empty assistant text")
	}
}

func TestAgentRunToolCall(t *testing.T) {
	type addParams struct {
		A int `json:"a" jsonschema:"description=first addend"`
		B int `json:"b" jsonschema:"description=second addend"`
	}
	addTool := NewTool(Tool[addParams]{
		Name:        "add",
		Description: "Add two integers and return their sum",
		Execute: func(ctx context.Context, p addParams) (ToolResult, error) {
			return ToolResult{Content: fmt.Sprintf("%d", p.A+p.B)}, nil
		},
	})

	agent := newTestAgent(t, addTool)
	events, result := runAndCollect(t, agent,
		"Use the add tool to add 2 and 3. You must call the tool; do not compute it yourself.")
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
	agent := newTestAgent(t)

	stream, err := agent.Prompt(context.Background(),
		NewText("user", "Write a long, detailed essay about the history of computing."), PromptOpts{})
	if err != nil {
		t.Fatalf("agent.Prompt: %v", err)
	}

	// Stop immediately, before the provider stream completes.
	agent.Stop()

	var result PromptResult
	select {
	case result = <-stream.Done:
	case <-time.After(45 * time.Second):
		t.Fatal("timeout waiting for done")
	}

	if result.Err == nil {
		t.Fatal("expected error after Stop")
	}
	if !errors.Is(result.Err, ErrAgentStopped) {
		t.Fatalf("expected ErrAgentStopped, got %v", result.Err)
	}
}

func TestAgentRunState(t *testing.T) {
	agent := newTestAgent(t)
	_, result := runAndCollect(t, agent, "Say hi.")
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if agent.State().SessionID == "" {
		t.Fatal("expected non-empty session id in state")
	}
}

func TestAgentSessionResume(t *testing.T) {
	agent := newTestAgent(t)

	_, result1 := runAndCollect(t, agent, "Remember the number 42.")
	if result1.Err != nil {
		t.Fatalf("prompt1 error: %v", result1.Err)
	}
	sid := agent.State().SessionID
	if sid == "" {
		t.Fatal("expected non-empty session id from prompt1")
	}
	firstLen := len(agent.Transcript())

	stream2, err := agent.Prompt(context.Background(),
		NewText("user", "What number did I ask you to remember?"), PromptOpts{SessionID: sid})
	if err != nil {
		t.Fatalf("prompt2: %v", err)
	}
	_, result2 := drain(t, stream2)
	if result2.Err != nil {
		t.Fatalf("prompt2 error: %v", result2.Err)
	}

	// The resumed transcript must retain prompt1's messages and grow.
	if got := len(agent.Transcript()); got <= firstLen {
		t.Fatalf("expected resumed transcript to grow past %d, got %d", firstLen, got)
	}
}

func TestAgentDefaultSessionRoundTrip(t *testing.T) {
	agent := newTestAgent(t)
	events, result := runAndCollect(t, agent, "Reply with a short greeting.")
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if assistantText(events) == "" {
		t.Fatal("expected non-empty assistant text")
	}
}

func TestAgentCompactNoSessionReturnsEmptyResult(t *testing.T) {
	agent := newTestAgent(t)
	result, err := agent.Compact(context.Background(), CompactOpts{})
	if err != nil {
		t.Fatalf("unexpected error from Compact: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil CompactResult")
	}
}

func TestAgentCompactPreconditionBusy(t *testing.T) {
	agent := newTestAgent(t)

	// Start a prompt; while it is in flight, Compact must report the agent busy.
	stream, err := agent.Prompt(context.Background(),
		NewText("user", "Write a long, detailed essay about the history of computing."), PromptOpts{})
	if err != nil {
		t.Fatalf("agent.Prompt: %v", err)
	}

	_, err = agent.Compact(context.Background(), CompactOpts{})
	if err == nil {
		t.Fatal("expected error from Compact while busy")
	}
	if !errors.Is(err, ErrAgentBusy) {
		t.Fatalf("expected ErrAgentBusy, got %v", err)
	}

	agent.Stop()
	<-stream.Done
}

// TestMultiTenantIsolation replaces the v0.1.x concurrent-runs-on-one-Agent
// test: under ADR-0006 each conversation is its own Agent, so isolation is
// exercised by N Agents prompting concurrently with no cross-Agent bleed.
func TestMultiTenantIsolation(t *testing.T) {
	const n = 3
	agents := make([]*Agent, n)
	for i := range agents {
		agents[i] = newTestAgent(t)
	}

	markers := []string{"alpha", "bravo", "charlie"}
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := range agents {
		go func(i int) {
			defer wg.Done()
			prompt := fmt.Sprintf("Reply with exactly this word: %s", markers[i])
			stream, err := agents[i].Prompt(context.Background(), NewText("user", prompt), PromptOpts{})
			if err != nil {
				errs[i] = err
				return
			}
			_, result := drain(t, stream)
			errs[i] = result.Err
		}(i)
	}
	wg.Wait()

	for i := range agents {
		if errs[i] != nil {
			t.Fatalf("agent %d error: %v", i, errs[i])
		}
		// Each Agent's transcript must contain only its own user marker.
		for j, marker := range markers {
			present := userMarkerPresent(agents[i].Transcript(), marker)
			if j == i && !present {
				t.Errorf("agent %d transcript missing its own marker %q", i, marker)
			}
			if j != i && present {
				t.Errorf("agent %d transcript leaked marker %q from agent %d", i, marker, j)
			}
		}
	}
}

func userMarkerPresent(msgs []Message, marker string) bool {
	for _, m := range msgs {
		if m.Role == "user" && strings.Contains(m.Text(), marker) {
			return true
		}
	}
	return false
}
