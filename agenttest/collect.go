package agenttest

import (
	"context"
	"testing"
	"time"

	"github.com/dev-resolute/resolute-agent-core-go"
)

// RunAndCollect prompts the agent, drains both channels, and returns all
// events + the terminal result.
func RunAndCollect(t *testing.T, agent *pi.Agent, prompt string) ([]pi.AgentEvent, pi.PromptResult) {
	t.Helper()

	ctx := context.Background()
	stream, err := agent.Prompt(ctx, pi.NewText("user", prompt), pi.PromptOpts{})
	if err != nil {
		t.Fatalf("agent.Prompt: %v", err)
	}

	var events []pi.AgentEvent
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range stream.Events {
			events = append(events, ev)
		}
	}()

	var result pi.PromptResult
	select {
	case result = <-stream.Done:
	case <-time.After(30 * time.Second):
		t.Fatal("timeout waiting for stream.Done")
	}
	<-done

	return events, result
}
