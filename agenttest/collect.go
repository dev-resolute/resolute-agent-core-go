package agenttest

import (
	"context"
	"testing"
	"time"

	"github.com/resolute-sh/pi-core-agent-go"
)

// RunAndCollect runs the agent with the given prompt, drains both channels,
// and returns all events + the terminal result.
func RunAndCollect(t *testing.T, agent *pi.Agent, prompt string) ([]pi.AgentEvent, pi.RunResult) {
	t.Helper()

	ctx := context.Background()
	run, err := agent.Run(ctx, pi.RunOpts{
		Prompt: pi.NewText("user", prompt),
	})
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}

	var events []pi.AgentEvent
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range run.Events() {
			events = append(events, ev)
		}
	}()

	var result pi.RunResult
	select {
	case result = <-run.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("timeout waiting for run.Done")
	}
	<-done

	return events, result
}
