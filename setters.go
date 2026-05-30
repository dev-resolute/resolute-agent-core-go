package pi

import "github.com/resolute-sh/pi-llm-go"

// SetModel sets the model reference used for subsequent turns. The change takes
// effect on the next turn snapshot, not the in-flight turn.
func (a *Agent) SetModel(model string) {
	a.mu.Lock()
	a.model = model
	a.mu.Unlock()
}

// SetTools replaces the Agent's tool set. The change takes effect on the next
// turn snapshot, not the in-flight turn.
func (a *Agent) SetTools(tools []RegisteredTool) {
	a.mu.Lock()
	a.tools = tools
	a.mu.Unlock()
}

// SetSystemPrompt replaces the Agent's system prompt. The change takes effect
// on the next turn snapshot.
func (a *Agent) SetSystemPrompt(prompt string) {
	a.mu.Lock()
	a.systemPrompt = prompt
	a.mu.Unlock()
}

// SetThinkingLevel sets the thinking level used for subsequent turns. The
// change takes effect on the next turn snapshot, not the in-flight turn.
func (a *Agent) SetThinkingLevel(level llm.ThinkingLevel) {
	a.mu.Lock()
	a.thinkingLevel = level
	a.mu.Unlock()
}

// SetSkills replaces the Agent's skill set. The change takes effect on the next
// turn snapshot.
func (a *Agent) SetSkills(skills []Skill) {
	a.mu.Lock()
	a.skills = skills
	a.mu.Unlock()
}
