// Package compaction provides transcript compaction via LLM summarization.
package compaction

import (
	"context"
	"fmt"

	"github.com/resolute-sh/pi-llm-go"
	"github.com/resolute-sh/pi-core-agent-go/session"
)

// Hooks mirrors the compaction-relevant hook fields.
type Hooks struct {
	BeforeCompact func(ctx context.Context, c BeforeCompactCtx) error
	AfterCompact  func(ctx context.Context, c AfterCompactCtx) error
}

// BeforeCompactCtx is passed to the BeforeCompact hook.
type BeforeCompactCtx struct {
	SessionID session.SessionID
	CutPoint  int
}

// AfterCompactCtx is passed to the AfterCompact hook.
type AfterCompactCtx struct {
	SessionID     session.SessionID
	BranchSummary session.BranchSummary
}

// Compact collapses older transcript messages into a BranchSummary.
func Compact(ctx context.Context, providers []llm.LLMProvider, defaultModel string, repo session.SessionRepo, opts Opts, hooks Hooks) (*Result, error) {
	// TODO(v0.x): see ADR-0003
	return nil, fmt.Errorf("compaction not yet implemented")
}
