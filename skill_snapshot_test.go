package pi

import (
	"strings"
	"testing"

	"github.com/resolute-sh/pi-llm-go"
)

func systemText(t *testing.T, req llm.LLMRequest) string {
	t.Helper()
	for _, m := range req.Messages {
		if m.Role != "system" {
			continue
		}
		text, ok := m.Content.(llm.TextContent)
		if ok {
			return text.Text
		}
	}
	t.Fatalf("no system message in request: %+v", req.Messages)
	return ""
}

// TestSkillsAutoRenderAndHotReload verifies skills are auto-rendered into the
// per-turn system prompt and that SetSkills is reflected on the next turn.
func TestSkillsAutoRenderAndHotReload(t *testing.T) {
	provider := newRecordingProvider("test")
	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
		SystemPrompt: "base prompt",
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	// given: turn 1 runs before any skills are configured
	runOnePrompt(t, a)
	sys1 := systemText(t, provider.capturedReq())
	if sys1 != "base prompt" {
		t.Errorf("turn 1 system prompt = %q, want %q", sys1, "base prompt")
	}
	if strings.Contains(sys1, "<available_skills>") {
		t.Errorf("turn 1 system prompt unexpectedly carries a skills index: %q", sys1)
	}

	// when: skills are hot-reloaded between turns
	a.SetSkills([]Skill{
		{Name: "research", Description: "do research", FilePath: "/s/research/SKILL.md"},
		{Name: "hidden", Description: "secret", FilePath: "/s/hidden/SKILL.md", DisableModelInvocation: true},
	})

	// then: turn 2 renders the index, appended to the configured system prompt,
	// excluding the model-disabled skill
	runOnePrompt(t, a)
	sys2 := systemText(t, provider.capturedReq())
	if !strings.HasPrefix(sys2, "base prompt\n\n") {
		t.Errorf("turn 2 system prompt = %q, want it to start with the configured prompt", sys2)
	}
	if !strings.Contains(sys2, "<available_skills>") {
		t.Errorf("turn 2 system prompt missing skills index: %q", sys2)
	}
	if !strings.Contains(sys2, "<name>research</name>") {
		t.Errorf("turn 2 system prompt missing research skill: %q", sys2)
	}
	if strings.Contains(sys2, "<name>hidden</name>") {
		t.Errorf("turn 2 system prompt leaked model-disabled skill: %q", sys2)
	}
}

// TestSkillsFromConfig verifies that AgentConfig.Skills seeds the initial skill
// set so turn-1 system prompt carries the index without a SetSkills call.
func TestSkillsFromConfig(t *testing.T) {
	provider := newRecordingProvider("test")
	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
		SystemPrompt: "base prompt",
		Skills: []Skill{
			{Name: "startup-skill", Description: "available from config", FilePath: "/s/startup/SKILL.md"},
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	runOnePrompt(t, a)
	sys := systemText(t, provider.capturedReq())
	if !strings.Contains(sys, "<available_skills>") {
		t.Errorf("turn 1 system prompt missing skills index: %q", sys)
	}
	if !strings.Contains(sys, "<name>startup-skill</name>") {
		t.Errorf("turn 1 system prompt missing startup-skill: %q", sys)
	}
}

// TestSkillsNotPersistedToTranscript verifies the rendered index is derived
// per-turn and never written into the session transcript.
func TestSkillsNotPersistedToTranscript(t *testing.T) {
	provider := newRecordingProvider("test")
	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
		SystemPrompt: "base prompt",
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	a.SetSkills([]Skill{{Name: "research", Description: "do research", FilePath: "/s/research/SKILL.md"}})
	runOnePrompt(t, a)

	for _, m := range a.Transcript() {
		if strings.Contains(string(m.Body), "available_skills") {
			t.Fatalf("skills index leaked into persisted transcript: %s", m.Body)
		}
	}
}
