package pi

import (
	"context"
	"fmt"
	"slices"

	"github.com/dev-resolute/resolute-llm-go"
)

// SetModel sets the model reference used for subsequent turns. The change takes
// effect on the next turn snapshot, not the in-flight turn.
func (a *Agent) SetModel(model string) {
	a.mu.Lock()
	old := a.model
	a.model = model
	a.mu.Unlock()
	if a.hooks.OnConfigUpdate != nil {
		a.hooks.OnConfigUpdate(ConfigUpdateCtx{
			Field:    ConfigFieldModel,
			OldModel: old,
			NewModel: model,
		})
	}
}

// SetTools replaces the Agent's tool set. The change takes effect on the next
// turn snapshot, not the in-flight turn. It returns an error without mutating the
// Agent when tool names are not unique, or when the current active set references
// a tool the new set no longer registers.
func (a *Agent) SetTools(tools []RegisteredTool) error {
	a.mu.Lock()
	if err := validateToolConfig(tools, a.activeToolNames); err != nil {
		a.mu.Unlock()
		return err
	}
	old := a.tools
	a.tools = tools
	a.mu.Unlock()
	if a.hooks.OnConfigUpdate != nil {
		a.hooks.OnConfigUpdate(ConfigUpdateCtx{
			Field:    ConfigFieldTools,
			OldTools: old,
			NewTools: tools,
		})
	}
	return nil
}

// SetSystemPrompt replaces the Agent's system prompt. The change takes effect
// on the next turn snapshot.
func (a *Agent) SetSystemPrompt(prompt string) {
	a.mu.Lock()
	old := a.systemPrompt
	a.systemPrompt = prompt
	a.mu.Unlock()
	if a.hooks.OnConfigUpdate != nil {
		a.hooks.OnConfigUpdate(ConfigUpdateCtx{
			Field:           ConfigFieldSystemPrompt,
			OldSystemPrompt: old,
			NewSystemPrompt: prompt,
		})
	}
}

// SetThinkingLevel sets the thinking level used for subsequent turns. The
// change takes effect on the next turn snapshot, not the in-flight turn.
func (a *Agent) SetThinkingLevel(level llm.ThinkingLevel) {
	a.mu.Lock()
	old := a.thinkingLevel
	a.thinkingLevel = level
	a.mu.Unlock()
	if a.hooks.OnConfigUpdate != nil {
		a.hooks.OnConfigUpdate(ConfigUpdateCtx{
			Field:            ConfigFieldThinkingLevel,
			OldThinkingLevel: old,
			NewThinkingLevel: level,
		})
	}
}

// SetActiveTools sets the subset of registered tools offered to the model on
// subsequent turns; nil means all registered tools are active. The change takes
// effect on the next turn snapshot, not the in-flight turn. It returns an error
// without mutating the Agent when a name is not registered or is duplicated.
//
// Persistence follows the bound session: when a session is bound and idle the
// change is written immediately; when a prompt is in flight it is queued and
// flushed at the next turn-end safe point. Before the first prompt no session is
// bound and nothing is persisted — instead the change is recorded at bind time
// (see Prompt) if the active set differs from the full registered set, so resume
// still restores it.
func (a *Agent) SetActiveTools(ctx context.Context, names []string) error {
	a.mu.Lock()
	if err := validateToolConfig(a.tools, names); err != nil {
		a.mu.Unlock()
		return err
	}
	stored := slices.Clone(names)
	old := a.activeToolNames
	a.activeToolNames = stored
	sid := a.lastSessionID
	bound := sid != ""
	running := a.isRunning()
	if bound && running {
		a.pendingActiveTools = append(a.pendingActiveTools, stored)
	}
	a.mu.Unlock()

	if bound && !running {
		if err := a.session.Append(ctx, sid, NewActiveToolsChange(stored)); err != nil {
			return fmt.Errorf("persisting active tools change: %w", err)
		}
	}

	if a.hooks.OnConfigUpdate != nil {
		a.hooks.OnConfigUpdate(ConfigUpdateCtx{
			Field:          ConfigFieldActiveTools,
			OldActiveTools: old,
			NewActiveTools: stored,
		})
	}
	return nil
}

// SetSkills replaces the Agent's skill set. The change takes effect on the next
// turn snapshot.
func (a *Agent) SetSkills(skills []Skill) {
	a.mu.Lock()
	old := a.skills
	a.skills = skills
	a.mu.Unlock()
	if a.hooks.OnConfigUpdate != nil {
		a.hooks.OnConfigUpdate(ConfigUpdateCtx{
			Field:     ConfigFieldSkills,
			OldSkills: old,
			NewSkills: skills,
		})
	}
}
