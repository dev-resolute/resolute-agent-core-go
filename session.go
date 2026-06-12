package pi

import (
	"context"
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
	AppendActiveToolsChange(ctx context.Context, id SessionID, names []string) error
	Load(ctx context.Context, id SessionID) ([]Message, error)
	List(ctx context.Context) ([]SessionMeta, error)
	AppendBranchSummary(ctx context.Context, id SessionID, summary BranchSummary) error
	LoadBranchSummaries(ctx context.Context, id SessionID) ([]BranchSummary, error)
	Delete(ctx context.Context, id SessionID) error
}
