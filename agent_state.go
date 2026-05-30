package pi

import (
	"time"

	"github.com/resolute-sh/pi-llm-go"
)

// AgentPhase describes the current phase of a run.
type AgentPhase int

const (
	PhaseIdle AgentPhase = iota
	PhaseWaitingLLM
	PhaseExecutingTools
	PhaseCompacting
	PhaseShuttingDown
	PhaseDone
)

// PendingToolCall describes an in-flight tool execution.
type PendingToolCall struct {
	CallID   string
	ToolName string
}

// AgentState is a value-type snapshot of a run's current state.
type AgentState struct {
	Phase            AgentPhase
	ActiveModel      string
	Thinking         llm.ThinkingLevel
	SessionID        SessionID
	TurnNumber       int
	TranscriptLen    int
	PendingToolCalls []PendingToolCall
	LastEvent        AgentEvent
	LastError        error
	StartedAt        time.Time
	LastActivityAt   time.Time
}
