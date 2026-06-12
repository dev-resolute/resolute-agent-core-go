package pi

import "github.com/resolute-sh/pi-llm-go"

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
// turn snapshot, not the in-flight turn.
func (a *Agent) SetTools(tools []RegisteredTool) {
	a.mu.Lock()
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
