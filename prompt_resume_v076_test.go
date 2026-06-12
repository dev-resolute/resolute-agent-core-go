package pi

import (
	"context"
	"testing"

	"github.com/resolute-sh/pi-llm-go"
)

// Fix (upstream 0.52.7): queued steering/follow-up messages resume correctly when
// the context currently ends in an assistant message, preserving one-at-a-time
// ordering during assistant-tail resumes.
//
// Upstream wired this into continue(): an Agent-level steeringQueue/followUpQueue
// drained one entry at a time when the transcript ended in an assistant message.
// The Go port has no continue() and no persistent Agent-level queues — ADR-0006
// makes steering in-flight-only via the per-prompt steerCh/followUpCh channels.
// The equivalent invariant is realized structurally by the loop seams: after a
// text-only (assistant-tail) turn the loop drains exactly one queued steer at the
// post-batch seam (steer before follow-up), then re-runs a turn, so multiple
// queued steers resume one-at-a-time in FIFO order, each driving its own LLM call.
// These fixtures pin that behavior against today's loop.

// TestQueuedSteersResumeOnAssistantTailOneAtATime_v076 queues two steers while a
// text-only turn is in flight. They must resume one-at-a-time in order: s1 drives
// the second LLM call (with s2 NOT yet in context), then s2 drives the third.
func TestQueuedSteersResumeOnAssistantTailOneAtATime_v076(t *testing.T) {
	t.Parallel()

	steersQueued := make(chan struct{})
	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			switch call {
			case 1:
				events <- llm.TextDeltaEvent{Delta: "turn1"}
				<-steersQueued
				events <- llm.MessageEndEvent{}
			case 2:
				events <- llm.TextDeltaEvent{Delta: "after-s1"}
				events <- llm.MessageEndEvent{}
			case 3:
				events <- llm.TextDeltaEvent{Delta: "after-s2"}
				events <- llm.MessageEndEvent{}
			default:
				events <- llm.TextDeltaEvent{Delta: "done"}
				events <- llm.MessageEndEvent{}
			}
		},
	}

	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if err := a.Steer(context.Background(), NewText("user", "s1")); err != nil {
		t.Fatalf("Steer s1: %v", err)
	}
	if err := a.Steer(context.Background(), NewText("user", "s2")); err != nil {
		t.Fatalf("Steer s2: %v", err)
	}
	close(steersQueued)

	_, result := drain(t, stream)
	if result.Err != nil {
		t.Fatalf("result.Err = %v, want nil", result.Err)
	}

	if got := provider.callCount(); got != 3 {
		t.Fatalf("provider called %d times, want 3 (each queued steer drives its own assistant-tail resume)", got)
	}

	// One-at-a-time: the second LLM call sees s1 but NOT s2 (still queued).
	req2, ok := provider.requestForCall(2)
	if !ok {
		t.Fatal("no second request recorded")
	}
	if !requestMentions(req2, "s1") {
		t.Error("second LLM call did not see s1 in context")
	}
	if requestMentions(req2, "s2") {
		t.Error("second LLM call saw s2 — steers were not resumed one-at-a-time")
	}

	// The third LLM call sees s2 (resumed after s1's turn).
	req3, ok := provider.requestForCall(3)
	if !ok {
		t.Fatal("no third request recorded")
	}
	if !requestMentions(req3, "s2") {
		t.Error("third LLM call did not see s2 in context")
	}

	// Both steers land after the first assistant message (assistant-tail resume),
	// and in FIFO order s1 before s2.
	firstAssistant, s1Idx, s2Idx := -1, -1, -1
	for i, m := range result.Messages {
		if m.Role == "assistant" && m.Type == "text" && firstAssistant < 0 {
			firstAssistant = i
		}
		if m.Role == "user" && m.Type == "text" {
			switch m.Text() {
			case "s1":
				s1Idx = i
			case "s2":
				s2Idx = i
			}
		}
	}
	if firstAssistant < 0 {
		t.Fatalf("no assistant message in transcript: %+v", result.Messages)
	}
	if s1Idx < 0 || s2Idx < 0 {
		t.Fatalf("steers missing from transcript: s1=%d s2=%d msgs=%+v", s1Idx, s2Idx, result.Messages)
	}
	if s1Idx <= firstAssistant {
		t.Errorf("s1 at %d must resume after the assistant tail at %d", s1Idx, firstAssistant)
	}
	if s2Idx <= s1Idx {
		t.Errorf("steer order not preserved: s2 at %d must follow s1 at %d", s2Idx, s1Idx)
	}
}

// TestQueuedFollowUpResumesOnAssistantTail_v076 queues a follow-up while a
// text-only turn is in flight. On the assistant-tail seam the loop resumes it,
// driving a second LLM call that carries the follow-up in context.
func TestQueuedFollowUpResumesOnAssistantTail_v076(t *testing.T) {
	t.Parallel()

	followUpQueued := make(chan struct{})
	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			if call == 1 {
				events <- llm.TextDeltaEvent{Delta: "turn1"}
				<-followUpQueued
				events <- llm.MessageEndEvent{}
				return
			}
			events <- llm.TextDeltaEvent{Delta: "done"}
			events <- llm.MessageEndEvent{}
		},
	}

	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if err := a.FollowUp(context.Background(), NewText("user", "follow")); err != nil {
		t.Fatalf("FollowUp: %v", err)
	}
	close(followUpQueued)

	_, result := drain(t, stream)
	if result.Err != nil {
		t.Fatalf("result.Err = %v, want nil", result.Err)
	}

	if got := provider.callCount(); got != 2 {
		t.Fatalf("provider called %d times, want 2 (follow-up must resume on the assistant tail)", got)
	}

	req2, ok := provider.requestForCall(2)
	if !ok {
		t.Fatal("no second request recorded")
	}
	if !requestMentions(req2, "follow") {
		t.Error("second LLM call did not see the follow-up in context")
	}

	firstAssistant, followIdx := -1, -1
	for i, m := range result.Messages {
		if m.Role == "assistant" && m.Type == "text" && firstAssistant < 0 {
			firstAssistant = i
		}
		if m.Role == "user" && m.Type == "text" && m.Text() == "follow" {
			followIdx = i
		}
	}
	if firstAssistant < 0 {
		t.Fatalf("no assistant message in transcript: %+v", result.Messages)
	}
	if followIdx < 0 {
		t.Fatalf("follow-up missing from transcript: %+v", result.Messages)
	}
	if followIdx <= firstAssistant {
		t.Errorf("follow-up at %d must resume after the assistant tail at %d", followIdx, firstAssistant)
	}
}

// TestQueuedSteerPrecedesFollowUpOnAssistantTail_v076 pins the resume precedence:
// with both a steer and a follow-up queued, the steer resumes first (second LLM
// call) and the follow-up second (third call) — mirroring upstream continue()
// checking the steering queue before the follow-up queue.
func TestQueuedSteerPrecedesFollowUpOnAssistantTail_v076(t *testing.T) {
	t.Parallel()

	queued := make(chan struct{})
	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			if call == 1 {
				events <- llm.TextDeltaEvent{Delta: "turn1"}
				<-queued
				events <- llm.MessageEndEvent{}
				return
			}
			events <- llm.TextDeltaEvent{Delta: "done"}
			events <- llm.MessageEndEvent{}
		},
	}

	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if err := a.FollowUp(context.Background(), NewText("user", "follow")); err != nil {
		t.Fatalf("FollowUp: %v", err)
	}
	if err := a.Steer(context.Background(), NewText("user", "steer")); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	close(queued)

	_, result := drain(t, stream)
	if result.Err != nil {
		t.Fatalf("result.Err = %v, want nil", result.Err)
	}

	if got := provider.callCount(); got != 3 {
		t.Fatalf("provider called %d times, want 3 (steer then follow-up each resume)", got)
	}

	req2, ok := provider.requestForCall(2)
	if !ok {
		t.Fatal("no second request recorded")
	}
	if !requestMentions(req2, "steer") {
		t.Error("steer did not resume first on the assistant tail")
	}
	if requestMentions(req2, "follow") {
		t.Error("follow-up resumed before the steer — precedence not preserved")
	}

	steerIdx, followIdx := -1, -1
	for i, m := range result.Messages {
		if m.Role == "user" && m.Type == "text" {
			switch m.Text() {
			case "steer":
				steerIdx = i
			case "follow":
				followIdx = i
			}
		}
	}
	if steerIdx < 0 || followIdx < 0 {
		t.Fatalf("steer/follow-up missing: steer=%d follow=%d msgs=%+v", steerIdx, followIdx, result.Messages)
	}
	if followIdx <= steerIdx {
		t.Errorf("steer at %d must resume before follow-up at %d", steerIdx, followIdx)
	}
}
