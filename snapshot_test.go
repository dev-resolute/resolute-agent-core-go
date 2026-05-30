package pi

import (
	"context"
	"testing"

	"github.com/resolute-sh/pi-llm-go"
)

func TestTurnSnapshotSettersAndIsolation(t *testing.T) {
	// Pure-logic test: the snapshot module sources no provider responses, so it
	// runs hermetically without a provider or API key.
	agent, err := NewAgent(AgentConfig{DefaultModel: "gemini/gemini-2.5-flash"})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	// given: a snapshot taken before any setter
	before := agent.snapshot()

	// when: every setter mutates the Agent after the snapshot was taken
	tool := NewTool(Tool[struct{}]{
		Name:        "noop",
		Description: "does nothing",
		Execute:     func(ctx context.Context, p struct{}) (ToolResult, error) { return ToolResult{}, nil },
	})
	agent.SetModel("mock/other")
	agent.SetThinkingLevel(llm.ThinkingHigh)
	agent.SetSystemPrompt("be terse")
	agent.SetTools([]RegisteredTool{tool})
	agent.SetSkills([]Skill{{Name: "research", Description: "do research"}})

	after := agent.snapshot()

	// then: the in-flight snapshot is unaffected by the later setters
	t.Run("in-flight snapshot unchanged", func(t *testing.T) {
		if before.model != "gemini/gemini-2.5-flash" {
			t.Errorf("before.model = %q, want %q", before.model, "gemini/gemini-2.5-flash")
		}
		if before.thinkingLevel != llm.ThinkingOff {
			t.Errorf("before.thinkingLevel = %v, want ThinkingOff", before.thinkingLevel)
		}
		if before.systemPrompt != "" {
			t.Errorf("before.systemPrompt = %q, want empty", before.systemPrompt)
		}
		if len(before.tools) != 0 {
			t.Errorf("before.tools len = %d, want 0", len(before.tools))
		}
		if len(before.skills) != 0 {
			t.Errorf("before.skills len = %d, want 0", len(before.skills))
		}
	})

	// then: the next snapshot reflects every mutation
	t.Run("next snapshot reflects setters", func(t *testing.T) {
		if after.model != "mock/other" {
			t.Errorf("after.model = %q, want %q", after.model, "mock/other")
		}
		if after.thinkingLevel != llm.ThinkingHigh {
			t.Errorf("after.thinkingLevel = %v, want ThinkingHigh", after.thinkingLevel)
		}
		if after.systemPrompt != "be terse" {
			t.Errorf("after.systemPrompt = %q, want %q", after.systemPrompt, "be terse")
		}
		if len(after.tools) != 1 || after.tools[0].Name() != "noop" {
			t.Errorf("after.tools = %v, want one tool named noop", after.tools)
		}
		if len(after.skills) != 1 || after.skills[0].Name != "research" {
			t.Errorf("after.skills = %v, want one skill named research", after.skills)
		}
	})
}
