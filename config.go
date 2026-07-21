package pi

import (
	"time"

	"github.com/dev-resolute/resolute-llm-go"
)

// ToolExecutionMode controls whether tools execute in parallel or serially.
type ToolExecutionMode int

const (
	// ToolExecParallel runs a turn's tool calls concurrently. It is the zero value and the default.
	ToolExecParallel ToolExecutionMode = iota
	// ToolExecSequential runs a turn's tool calls one at a time, in the order the model requested them.
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
	// SummarizationRetry configures bounded retries with exponential backoff
	// for the summarization calls made by Compact. The zero value disables
	// retries, matching pre-0.7.0 behavior. Retry lifecycle is reported
	// through Hooks.OnSummarizationRetry. Ported from upstream 0.81.1.
	SummarizationRetry SummarizationRetryPolicy
	// Transport is the preferred stream transport forwarded to every LLMRequest.
	// Zero value behaves as TransportAuto.
	Transport llm.TransportPreference
	// Skills is the initial skill set offered to the model; hot-reload via SetSkills.
	Skills []Skill
}

// ConvertToLLMFn transforms agent-side Messages into LLM-shaped messages.
type ConvertToLLMFn func(messages []Message) []llm.Message
