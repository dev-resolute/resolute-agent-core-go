package pi

import (
	"context"
	"errors"
	"testing"
)

func TestPromptReturnsErrAgentBusyWhenInFlight(t *testing.T) {
	agent := newTestAgent(t)
	ctx := context.Background()

	stream1, err := agent.Prompt(ctx,
		NewText("user", "Write a long, detailed essay about the history of computing."), PromptOpts{})
	if err != nil {
		t.Fatalf("first prompt: %v", err)
	}

	// The first prompt is still streaming, so a second must be rejected.
	_, err = agent.Prompt(ctx, NewText("user", "second"), PromptOpts{})
	if !errors.Is(err, ErrAgentBusy) {
		t.Fatalf("second prompt while first in flight: got %v, want ErrAgentBusy", err)
	}

	agent.Stop()
	<-stream1.Done
}

// TestBehaviorPreservationEventSequence is the no-behavior-change guard: a
// no-setter prompt must still produce the canonical event ordering and a clean
// terminal result, exactly as the v0.1.x loop did.
func TestBehaviorPreservationEventSequence(t *testing.T) {
	agent := newTestAgent(t)
	events, result := runAndCollect(t, agent, "Reply with a short greeting.")

	if result.Err != nil {
		t.Fatalf("unexpected terminal error: %v", result.Err)
	}
	if len(result.Messages) == 0 {
		t.Fatal("expected non-empty terminal messages")
	}
	if len(events) == 0 {
		t.Fatal("expected events")
	}

	if _, ok := events[0].(AgentStartEvent); !ok {
		t.Fatalf("first event = %T, want AgentStartEvent", events[0])
	}
	if _, ok := events[len(events)-1].(AgentEndEvent); !ok {
		t.Fatalf("last event = %T, want AgentEndEvent", events[len(events)-1])
	}

	idx := func(pred func(AgentEvent) bool) int {
		for i, ev := range events {
			if pred(ev) {
				return i
			}
		}
		return -1
	}

	turnStart := idx(func(e AgentEvent) bool { _, ok := e.(TurnStartEvent); return ok })
	userStart := idx(func(e AgentEvent) bool { m, ok := e.(MessageStartEvent); return ok && m.Role == "user" })
	asstStart := idx(func(e AgentEvent) bool {
		m, ok := e.(MessageStartEvent)
		return ok && m.Role == "assistant"
	})
	textDelta := idx(func(e AgentEvent) bool { _, ok := e.(TextDeltaEvent); return ok })
	msgEnd := idx(func(e AgentEvent) bool { _, ok := e.(MessageEndEvent); return ok })
	turnEnd := idx(func(e AgentEvent) bool { _, ok := e.(TurnEndEvent); return ok })

	for name, got := range map[string]int{
		"TurnStartEvent":               turnStart,
		"MessageStartEvent(user)":      userStart,
		"MessageStartEvent(assistant)": asstStart,
		"TextDeltaEvent":               textDelta,
		"MessageEndEvent":              msgEnd,
		"TurnEndEvent":                 turnEnd,
	} {
		if got < 0 {
			t.Fatalf("missing expected event %s", name)
		}
	}

	if !(userStart < asstStart) {
		t.Errorf("user MessageStart (%d) should precede assistant MessageStart (%d)", userStart, asstStart)
	}
	if !(asstStart < textDelta && textDelta < msgEnd) {
		t.Errorf("expected assistant start (%d) < text delta (%d) < message end (%d)", asstStart, textDelta, msgEnd)
	}
	if !(turnStart < turnEnd) {
		t.Errorf("TurnStart (%d) should precede TurnEnd (%d)", turnStart, turnEnd)
	}
}
