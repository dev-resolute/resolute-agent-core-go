package pi

import (
	"time"

	"github.com/resolute-sh/pi-llm-go"
)

// ToolExecutionMode controls whether tools execute in parallel or serially.
type ToolExecutionMode int

const (
	ToolExecParallel ToolExecutionMode = iota
	ToolExecSequential
)

// AgentConfig carries all settings for constructing an Agent.
type AgentConfig struct {
	Providers        []llm.LLMProvider
	DefaultModel     string
	SystemPrompt     string
	Tools            []RegisteredTool
	Hooks            Hooks
	Session          SessionRepo
	ConvertToLLM     ConvertToLLMFn
	ToolExecution    ToolExecutionMode
	MaxParallelTools int
	ShutdownTimeout  time.Duration
	EventBufferSize  int
	SteerBufferSize  int
	DefaultThinking  llm.ThinkingLevel
	ReserveTokens    int
	KeepRecentTokens int
}

// ConvertToLLMFn transforms agent-side Messages into LLM-shaped messages.
type ConvertToLLMFn func(messages []Message) []llm.Message
