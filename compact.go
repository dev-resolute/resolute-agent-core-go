package pi

import (
	"context"
	"fmt"
	"time"
)

// SessionID is an opaque string that identifies a single session.
type SessionID string

// SessionMeta carries metadata about a session.
type SessionMeta struct {
	ID        SessionID
	CreatedAt time.Time
	UpdatedAt time.Time
}

// BranchSummary is a persisted compaction artifact.
type BranchSummary struct {
	StartIdx  int
	EndIdx    int
	Summary   string
	CreatedAt time.Time
}

// SessionRepo is the interface every storage backend implements.
type SessionRepo interface {
	Create(ctx context.Context) (SessionID, error)
	Append(ctx context.Context, id SessionID, msgs ...Message) error
	Load(ctx context.Context, id SessionID) ([]Message, error)
	List(ctx context.Context) ([]SessionMeta, error)
	AppendBranchSummary(ctx context.Context, id SessionID, summary BranchSummary) error
	LoadBranchSummaries(ctx context.Context, id SessionID) ([]BranchSummary, error)
	Delete(ctx context.Context, id SessionID) error
}

// CompactOpts carries options for a compaction operation.
type CompactOpts struct {
	KeepRecentTokens int
}

// CompactResult carries the outcome of a compaction.
type CompactResult struct {
	Summary      BranchSummary
	RemovedCount int
}

// Compact collapses older transcript messages into a BranchSummary.
// It must be called when the agent is idle (no in-flight run).
func (a *Agent) Compact(ctx context.Context, opts CompactOpts) (*CompactResult, error) {
	// TODO(v0.x): see ADR-0003
	if a.isRunning() {
		return nil, fmt.Errorf("agent is busy: %w", ErrAgentBusy)
	}
	return nil, fmt.Errorf("compaction not yet implemented: %w", ErrUnsupportedFeature)
}
