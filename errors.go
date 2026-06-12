package pi

import "errors"

// Sentinel errors for pi-core-agent-go.
var (
	ErrPromptCancelled = errors.New("prompt cancelled by caller context")
	ErrAgentStopped    = errors.New("prompt stopped by caller")
	ErrToolLeaked      = errors.New("tool execution leaked goroutine")
	ErrToolNotFound    = errors.New("tool not found")
	ErrCompactFailed   = errors.New("compaction failed")
	ErrInvalidModel    = errors.New("invalid model")
	ErrInvalidModelRef = errors.New("invalid model reference")
	ErrAgentBusy       = errors.New("agent is busy")

	// ErrNoPromptInFlight is returned by Steer and FollowUp when no prompt
	// is currently in flight. It is also the cancel cause of the idle context
	// returned by Agent.Context(), ensuring any stale nested work tied to
	// that context exits immediately rather than leaking.
	ErrNoPromptInFlight   = errors.New("no prompt in flight")
	ErrSessionNotFound    = errors.New("session not found")
	ErrUnsupportedFeature = errors.New("unsupported feature")
	ErrDuplicateToolName  = errors.New("duplicate tool name")
	ErrUnknownActiveTool  = errors.New("active tool not registered")
)
