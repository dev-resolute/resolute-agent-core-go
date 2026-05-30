package pi

import "context"

// Hooks is a flat struct of optional function fields covering every lifecycle point.
// Nil fields are no-ops.
type Hooks struct {
	BeforeAgentStart      func(ctx context.Context, c BeforeAgentStartCtx) error
	BeforeToolCall        func(ctx context.Context, c BeforeToolCallCtx) error
	AfterToolCall         func(ctx context.Context, c AfterToolCallCtx) error
	BeforeCompact         func(ctx context.Context, c BeforeCompactCtx) error
	AfterCompact          func(ctx context.Context, c AfterCompactCtx) error
	TransformContext      func(ctx context.Context, c TransformContextCtx) ([]Message, error)
	BeforeProviderRequest func(ctx context.Context, c BeforeProviderRequestCtx) error
	AfterProviderResponse func(ctx context.Context, c AfterProviderResponseCtx)
}

// BeforeAgentStartCtx is passed to the BeforeAgentStart hook.
type BeforeAgentStartCtx struct {
	PromptOpts PromptOpts
}

// BeforeToolCallCtx is passed to the BeforeToolCall hook.
// Args may be rewritten by the hook.
type BeforeToolCallCtx struct {
	CallID   string
	ToolName string
	Args     []byte
}

// AfterToolCallCtx is passed to the AfterToolCall hook.
type AfterToolCallCtx struct {
	CallID   string
	ToolName string
	Result   ToolResult
}

// BeforeCompactCtx is passed to the BeforeCompact hook.
type BeforeCompactCtx struct {
	SessionID SessionID
	CutPoint  int
}

// AfterCompactCtx is passed to the AfterCompact hook.
type AfterCompactCtx struct {
	SessionID     SessionID
	BranchSummary BranchSummary
}

// TransformContextCtx is passed to the TransformContext hook.
// The returned messages replace the transcript sent to the LLM.
type TransformContextCtx struct {
	Messages []Message
}

// BeforeProviderRequestCtx is passed to the BeforeProviderRequest hook.
type BeforeProviderRequestCtx struct {
	Provider string
	Model    string
	Headers  map[string]string
}

// AfterProviderResponseCtx is passed to the AfterProviderResponse hook.
type AfterProviderResponseCtx struct {
	Provider   string
	Model      string
	StatusCode int
	Headers    map[string]string
}
