// Package session provides session storage backends for the agent.
package session

import (
	"context"
	"time"

	"github.com/resolute-sh/pi-core-agent-go"
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
	Append(ctx context.Context, id SessionID, msgs ...pi.Message) error
	Load(ctx context.Context, id SessionID) ([]pi.Message, error)
	List(ctx context.Context) ([]SessionMeta, error)
	AppendBranchSummary(ctx context.Context, id SessionID, summary BranchSummary) error
	LoadBranchSummaries(ctx context.Context, id SessionID) ([]BranchSummary, error)
	Delete(ctx context.Context, id SessionID) error
}
