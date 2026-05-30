package pi

// Skill is a unit of model-invokable expertise carried on the Agent. In
// AGENT-1 it is part of the mutable runtime config and the turn snapshot;
// rendering it into the system prompt is owned by a later issue (AGENT-10).
type Skill struct {
	Name                   string
	Description            string
	Content                string
	FilePath               string
	DisableModelInvocation bool
}
