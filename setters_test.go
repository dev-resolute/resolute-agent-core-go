package pi

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/resolute-sh/pi-llm-go"
)

func TestOnConfigUpdate_NilHookIsNoop(t *testing.T) {
	a, err := NewAgent(AgentConfig{DefaultModel: "test/model"})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	a.SetModel("test/v2")
	a.SetThinkingLevel(llm.ThinkingMedium)
	a.SetTools(nil)
	a.SetSystemPrompt("hello")
	a.SetSkills(nil)
}

func TestOnConfigUpdate_SetModel(t *testing.T) {
	var got []ConfigUpdateCtx
	a, err := NewAgent(AgentConfig{
		DefaultModel: "test/model",
		Hooks: Hooks{
			OnConfigUpdate: func(c ConfigUpdateCtx) {
				got = append(got, c)
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	a.SetModel("test/v2")

	if len(got) != 1 {
		t.Fatalf("expected 1 update, got %d", len(got))
	}
	c := got[0]
	if c.Field != ConfigFieldModel {
		t.Errorf("Field = %q, want %q", c.Field, ConfigFieldModel)
	}
	if c.OldModel != "test/model" {
		t.Errorf("OldModel = %q, want %q", c.OldModel, "test/model")
	}
	if c.NewModel != "test/v2" {
		t.Errorf("NewModel = %q, want %q", c.NewModel, "test/v2")
	}
}

func TestOnConfigUpdate_SetThinkingLevel(t *testing.T) {
	var got []ConfigUpdateCtx
	a, err := NewAgent(AgentConfig{
		DefaultModel: "test/model",
		Hooks: Hooks{
			OnConfigUpdate: func(c ConfigUpdateCtx) {
				got = append(got, c)
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	a.SetThinkingLevel(llm.ThinkingHigh)

	if len(got) != 1 {
		t.Fatalf("expected 1 update, got %d", len(got))
	}
	c := got[0]
	if c.Field != ConfigFieldThinkingLevel {
		t.Errorf("Field = %q, want %q", c.Field, ConfigFieldThinkingLevel)
	}
	if c.OldThinkingLevel != llm.ThinkingOff {
		t.Errorf("OldThinkingLevel = %v, want %v", c.OldThinkingLevel, llm.ThinkingOff)
	}
	if c.NewThinkingLevel != llm.ThinkingHigh {
		t.Errorf("NewThinkingLevel = %v, want %v", c.NewThinkingLevel, llm.ThinkingHigh)
	}
}

func TestOnConfigUpdate_SetTools(t *testing.T) {
	tool := NewTool(Tool[struct{}]{
		Name:        "noop",
		Description: "does nothing",
		Execute:     func(ctx context.Context, p struct{}) (ToolResult, error) { return ToolResult{}, nil },
	})

	var got []ConfigUpdateCtx
	a, err := NewAgent(AgentConfig{
		DefaultModel: "test/model",
		Hooks: Hooks{
			OnConfigUpdate: func(c ConfigUpdateCtx) {
				got = append(got, c)
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	a.SetTools([]RegisteredTool{tool})

	if len(got) != 1 {
		t.Fatalf("expected 1 update, got %d", len(got))
	}
	c := got[0]
	if c.Field != ConfigFieldTools {
		t.Errorf("Field = %q, want %q", c.Field, ConfigFieldTools)
	}
	if len(c.OldTools) != 0 {
		t.Errorf("OldTools len = %d, want 0", len(c.OldTools))
	}
	if len(c.NewTools) != 1 || c.NewTools[0].Name() != "noop" {
		t.Errorf("NewTools = %v, want 1 tool named noop", c.NewTools)
	}
}

func TestOnConfigUpdate_SetSystemPrompt(t *testing.T) {
	var got []ConfigUpdateCtx
	a, err := NewAgent(AgentConfig{
		DefaultModel: "test/model",
		Hooks: Hooks{
			OnConfigUpdate: func(c ConfigUpdateCtx) {
				got = append(got, c)
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	a.SetSystemPrompt("new prompt")

	if len(got) != 1 {
		t.Fatalf("expected 1 update, got %d", len(got))
	}
	c := got[0]
	if c.Field != ConfigFieldSystemPrompt {
		t.Errorf("Field = %q, want %q", c.Field, ConfigFieldSystemPrompt)
	}
	if c.OldSystemPrompt != "" {
		t.Errorf("OldSystemPrompt = %q, want empty", c.OldSystemPrompt)
	}
	if c.NewSystemPrompt != "new prompt" {
		t.Errorf("NewSystemPrompt = %q, want %q", c.NewSystemPrompt, "new prompt")
	}
}

func TestOnConfigUpdate_SetSkills(t *testing.T) {
	var got []ConfigUpdateCtx
	a, err := NewAgent(AgentConfig{
		DefaultModel: "test/model",
		Hooks: Hooks{
			OnConfigUpdate: func(c ConfigUpdateCtx) {
				got = append(got, c)
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	skills := []Skill{{Name: "research", Description: "do research"}}
	a.SetSkills(skills)

	if len(got) != 1 {
		t.Fatalf("expected 1 update, got %d", len(got))
	}
	c := got[0]
	if c.Field != ConfigFieldSkills {
		t.Errorf("Field = %q, want %q", c.Field, ConfigFieldSkills)
	}
	if len(c.OldSkills) != 0 {
		t.Errorf("OldSkills len = %d, want 0", len(c.OldSkills))
	}
	if len(c.NewSkills) != 1 || c.NewSkills[0].Name != "research" {
		t.Errorf("NewSkills = %v, want 1 skill named research", c.NewSkills)
	}
}

func TestOnConfigUpdate_HookCallingGetterNoDeadlock(t *testing.T) {
	var a *Agent
	var err error
	a, err = NewAgent(AgentConfig{
		DefaultModel: "test/model",
		Hooks: Hooks{
			OnConfigUpdate: func(c ConfigUpdateCtx) {
				_ = a.State()
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.SetModel("test/v2")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("deadlock: SetModel did not complete within 2s")
	}
}

func TestOnConfigUpdate_ConcurrentSettersNoRace(t *testing.T) {
	var mu sync.Mutex
	var count int
	a, err := NewAgent(AgentConfig{
		DefaultModel: "test/model",
		Hooks: Hooks{
			OnConfigUpdate: func(c ConfigUpdateCtx) {
				mu.Lock()
				count++
				mu.Unlock()
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n * 5)
	for i := 0; i < n; i++ {
		go func() { defer wg.Done(); a.SetModel("test/v") }()
		go func() { defer wg.Done(); a.SetThinkingLevel(llm.ThinkingMedium) }()
		go func() { defer wg.Done(); a.SetTools(nil) }()
		go func() { defer wg.Done(); a.SetSystemPrompt("p") }()
		go func() { defer wg.Done(); a.SetSkills(nil) }()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if count != n*5 {
		t.Errorf("expected %d hook calls, got %d", n*5, count)
	}
}
