package pi

import "github.com/resolute-sh/pi-llm-go"

// PromptOpts carries per-prompt overrides. The user message is passed as the
// second argument to Agent.Prompt, not on this struct.
type PromptOpts struct {
	SessionID     SessionID
	Model         string
	SystemPrompt  string
	Thinking      llm.ThinkingLevel
	ProviderHints llm.ProviderHints
}
