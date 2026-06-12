package pi

import "github.com/resolute-sh/pi-llm-go"

// turnSnapshot is the immutable copy of the Agent's runtime config used to
// build exactly one provider request. It is taken at turn start under the
// Agent's read lock; setter calls after a snapshot is taken affect the next
// snapshot, never one already handed out. Slices are shallow-copied so that
// later setters cannot mutate a snapshot already in flight. tools holds only the
// active subset (see filterActiveTools), so an inactive tool is never offered to
// the model nor executed.
type turnSnapshot struct {
	model           string
	tools           []RegisteredTool
	systemPrompt    string
	thinkingLevel   llm.ThinkingLevel
	thinkingBudgets map[llm.ThinkingLevel]int
	skills          []Skill
}

// snapshot returns a turnSnapshot of the Agent's current runtime config.
func (a *Agent) snapshot() turnSnapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()

	tools := filterActiveTools(a.tools, a.activeToolNames)
	skills := make([]Skill, len(a.skills))
	copy(skills, a.skills)

	return turnSnapshot{
		model:           a.model,
		tools:           tools,
		systemPrompt:    a.systemPrompt,
		thinkingLevel:   a.thinkingLevel,
		thinkingBudgets: cloneThinkingBudgets(a.thinkingBudgets),
		skills:          skills,
	}
}

// cloneThinkingBudgets returns a defensive copy of m. Nil in, nil out.
func cloneThinkingBudgets(m map[llm.ThinkingLevel]int) map[llm.ThinkingLevel]int {
	if m == nil {
		return nil
	}
	out := make(map[llm.ThinkingLevel]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
