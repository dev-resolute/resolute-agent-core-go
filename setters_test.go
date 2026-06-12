package pi

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/resolute-sh/pi-llm-go"
)

// newAgentWithHook creates an Agent whose OnConfigUpdate hook appends each call
// into the returned slice. The slice pointer stays valid across appends.
// Not safe for concurrent writes — use a mutex-guarded hook for concurrency tests.
func newAgentWithHook(t *testing.T) (*Agent, *[]ConfigUpdateCtx) {
	t.Helper()
	captured := new([]ConfigUpdateCtx)
	a, err := NewAgent(AgentConfig{
		DefaultModel: "test/model",
		Hooks: Hooks{
			OnConfigUpdate: func(c ConfigUpdateCtx) {
				*captured = append(*captured, c)
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	return a, captured
}

func TestOnConfigUpdate_NilHookIsNoop(t *testing.T) {
	t.Parallel()
	a, err := NewAgent(AgentConfig{DefaultModel: "test/model"})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	a.SetModel("test/v2")
	a.SetThinkingLevel(llm.ThinkingMedium)
	_ = a.SetTools(nil)
	a.SetSystemPrompt("hello")
	a.SetSkills(nil)
	_ = a.SetActiveTools(context.Background(), nil)
}

func TestOnConfigUpdate_Setters(t *testing.T) {
	t.Parallel()

	tool := NewTool(Tool[struct{}]{
		Name:        "noop",
		Description: "does nothing",
		Execute:     func(ctx context.Context, p struct{}) (ToolResult, error) { return ToolResult{}, nil },
	})
	skills := []Skill{{Name: "research", Description: "do research"}}

	tests := []struct {
		name  string
		set   func(*Agent)
		check func(*testing.T, ConfigUpdateCtx)
	}{
		{
			name: "SetModel",
			set:  func(a *Agent) { a.SetModel("test/v2") },
			check: func(t *testing.T, c ConfigUpdateCtx) {
				if c.Field != ConfigFieldModel {
					t.Errorf("Field = %q, want %q", c.Field, ConfigFieldModel)
				}
				if c.OldModel != "test/model" {
					t.Errorf("OldModel = %q, want %q", c.OldModel, "test/model")
				}
				if c.NewModel != "test/v2" {
					t.Errorf("NewModel = %q, want %q", c.NewModel, "test/v2")
				}
			},
		},
		{
			name: "SetThinkingLevel",
			set:  func(a *Agent) { a.SetThinkingLevel(llm.ThinkingHigh) },
			check: func(t *testing.T, c ConfigUpdateCtx) {
				if c.Field != ConfigFieldThinkingLevel {
					t.Errorf("Field = %q, want %q", c.Field, ConfigFieldThinkingLevel)
				}
				if c.OldThinkingLevel != llm.ThinkingOff {
					t.Errorf("OldThinkingLevel = %v, want %v", c.OldThinkingLevel, llm.ThinkingOff)
				}
				if c.NewThinkingLevel != llm.ThinkingHigh {
					t.Errorf("NewThinkingLevel = %v, want %v", c.NewThinkingLevel, llm.ThinkingHigh)
				}
			},
		},
		{
			name: "SetTools",
			set:  func(a *Agent) { _ = a.SetTools([]RegisteredTool{tool}) },
			check: func(t *testing.T, c ConfigUpdateCtx) {
				if c.Field != ConfigFieldTools {
					t.Errorf("Field = %q, want %q", c.Field, ConfigFieldTools)
				}
				if len(c.OldTools) != 0 {
					t.Errorf("OldTools len = %d, want 0", len(c.OldTools))
				}
				if len(c.NewTools) != 1 || c.NewTools[0].Name() != "noop" {
					t.Errorf("NewTools = %v, want 1 tool named noop", c.NewTools)
				}
			},
		},
		{
			name: "SetSystemPrompt",
			set:  func(a *Agent) { a.SetSystemPrompt("new prompt") },
			check: func(t *testing.T, c ConfigUpdateCtx) {
				if c.Field != ConfigFieldSystemPrompt {
					t.Errorf("Field = %q, want %q", c.Field, ConfigFieldSystemPrompt)
				}
				if c.OldSystemPrompt != "" {
					t.Errorf("OldSystemPrompt = %q, want empty", c.OldSystemPrompt)
				}
				if c.NewSystemPrompt != "new prompt" {
					t.Errorf("NewSystemPrompt = %q, want %q", c.NewSystemPrompt, "new prompt")
				}
			},
		},
		{
			name: "SetSkills",
			set:  func(a *Agent) { a.SetSkills(skills) },
			check: func(t *testing.T, c ConfigUpdateCtx) {
				if c.Field != ConfigFieldSkills {
					t.Errorf("Field = %q, want %q", c.Field, ConfigFieldSkills)
				}
				if len(c.OldSkills) != 0 {
					t.Errorf("OldSkills len = %d, want 0", len(c.OldSkills))
				}
				if len(c.NewSkills) != 1 || c.NewSkills[0].Name != "research" {
					t.Errorf("NewSkills = %v, want 1 skill named research", c.NewSkills)
				}
			},
		},
		{
			name: "SetActiveTools",
			set:  func(a *Agent) { _ = a.SetActiveTools(context.Background(), nil) },
			check: func(t *testing.T, c ConfigUpdateCtx) {
				if c.Field != ConfigFieldActiveTools {
					t.Errorf("Field = %q, want %q", c.Field, ConfigFieldActiveTools)
				}
				if len(c.OldActiveTools) != 0 {
					t.Errorf("OldActiveTools len = %d, want 0", len(c.OldActiveTools))
				}
				if len(c.NewActiveTools) != 0 {
					t.Errorf("NewActiveTools len = %d, want 0", len(c.NewActiveTools))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, got := newAgentWithHook(t)
			tt.set(a)
			if len(*got) != 1 {
				t.Fatalf("expected 1 update, got %d", len(*got))
			}
			tt.check(t, (*got)[0])
		})
	}
}

func TestOnConfigUpdate_HookCallingGetterNoDeadlock(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	wg.Add(n * 6)
	for i := 0; i < n; i++ {
		go func() { defer wg.Done(); a.SetModel("test/v") }()
		go func() { defer wg.Done(); a.SetThinkingLevel(llm.ThinkingMedium) }()
		go func() { defer wg.Done(); _ = a.SetTools(nil) }()
		go func() { defer wg.Done(); a.SetSystemPrompt("p") }()
		go func() { defer wg.Done(); a.SetSkills(nil) }()
		go func() { defer wg.Done(); _ = a.SetActiveTools(context.Background(), nil) }()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if count != n*6 {
		t.Errorf("expected %d hook calls, got %d", n*6, count)
	}
}
