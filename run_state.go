package pi

import (
	"time"

	"github.com/resolute-sh/pi-llm-go"
)

// RunPhase describes the current phase of a run.
type RunPhase int

const (
	PhaseIdle RunPhase = iota
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

// RunState is a value-type snapshot of a run's current state.
type RunState struct {
	Phase            RunPhase
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
