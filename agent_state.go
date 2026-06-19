package pi

import (
	"time"

	"github.com/resolute-sh/pi-llm-go"
)

// AgentPhase describes the current phase of a run.
type AgentPhase int

const (
	// PhaseIdle is the agent's phase at construction and between prompts, with no prompt in flight.
	PhaseIdle AgentPhase = iota
	// PhaseWaitingLLM means the run is blocked on a model response.
	PhaseWaitingLLM
	// PhaseExecutingTools means the run is executing tool calls requested by the model.
	PhaseExecutingTools
	// PhaseCompacting means the run is compacting the transcript to reclaim context window.
	PhaseCompacting
	// PhaseShuttingDown means the run is draining in-flight work after a stop.
	PhaseShuttingDown
	// PhaseDone means the prompt has finished.
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
