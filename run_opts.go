package pi

import "github.com/resolute-sh/pi-llm-go"

// RunOpts carries per-run overrides.
type RunOpts struct {
	Prompt        Message
	SessionID     SessionID
	Model         string
	SystemPrompt  string
	Thinking      llm.ThinkingLevel
	ProviderHints llm.ProviderHints
}
