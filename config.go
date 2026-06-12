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
	ActiveToolNames  []string
	Hooks            Hooks
	Session          SessionRepo
	ConvertToLLM     ConvertToLLMFn
	ToolExecution    ToolExecutionMode
	MaxParallelTools int
	ShutdownTimeout  time.Duration
	EventBufferSize  int
	SteerBufferSize  int
	DefaultThinking  llm.ThinkingLevel
	// ThinkingBudgets optionally sets per-level token caps forwarded to the
	// provider on every turn. Nil or empty means "use provider defaults".
	ThinkingBudgets  map[llm.ThinkingLevel]int
	ReserveTokens    int
	KeepRecentTokens int
	// Transport is the preferred stream transport forwarded to every LLMRequest.
	// Zero value behaves as TransportAuto.
	Transport llm.TransportPreference
}

// ConvertToLLMFn transforms agent-side Messages into LLM-shaped messages.
type ConvertToLLMFn func(messages []Message) []llm.Message
