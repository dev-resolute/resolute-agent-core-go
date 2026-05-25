package pi

import "errors"

// Sentinel errors for pi-core-agent-go.
var (
	ErrRunCancelled      = errors.New("run cancelled by caller context")
	ErrRunStopped        = errors.New("run stopped by caller")
	ErrToolLeaked        = errors.New("tool execution leaked goroutine")
	ErrToolNotFound      = errors.New("tool not found")
	ErrCompactFailed     = errors.New("compaction failed")
	ErrInvalidModel      = errors.New("invalid model")
	ErrInvalidModelRef   = errors.New("invalid model reference")
	ErrAgentBusy         = errors.New("agent is busy")
	ErrSessionNotFound   = errors.New("session not found")
	ErrUnsupportedFeature = errors.New("unsupported feature")
)
