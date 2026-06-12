package pi

import (
	"context"

	"github.com/resolute-sh/pi-llm-go"
)

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

	// ShouldStopAfterTurn is called at each turn boundary — after turn_end is
	// emitted and tool results are flushed to the session, before the
	// steer/follow-up queues are polled or the next LLM call starts. When it
	// returns true the loop exits with a clean, nil-error PromptResult. Nil is
	// a no-op. Matches upstream pi 0.72.0 shouldStopAfterTurn decision-point
	// semantics. It is also invoked on turns that end via ToolResult.Terminate;
	// on those turns the return value is ignored — the prompt ends regardless.
	// The auto-continue loop imposes no turn cap by design (parity with
	// upstream pi); this hook is the mechanism for imposing one.
	ShouldStopAfterTurn func(ctx context.Context, c AfterTurnCtx) bool

	// OnConfigUpdate is called synchronously by each setter (SetModel,
	// SetThinkingLevel, SetTools, SetSystemPrompt, SetSkills, SetActiveTools)
	// after the new
	// value is committed, on the setter's calling goroutine, without holding
	// the Agent's internal mutex. This means the hook may safely call Agent
	// getters (e.g. State()) without deadlocking. Because the mutex is released
	// before the hook runs, a concurrent setter may write a newer value between
	// the commit and the hook invocation — the hook may therefore observe a
	// newer Agent state than ConfigUpdateCtx.Old* reflects. Nil is a no-op.
	OnConfigUpdate func(ConfigUpdateCtx)
}

// ConfigField identifies which Agent configuration field changed.
type ConfigField string

const (
	ConfigFieldModel         ConfigField = "model"
	ConfigFieldThinkingLevel ConfigField = "thinking_level"
	ConfigFieldTools         ConfigField = "tools"
	ConfigFieldSystemPrompt  ConfigField = "system_prompt"
	ConfigFieldSkills        ConfigField = "skills"
	ConfigFieldActiveTools   ConfigField = "active_tools"
)

// ConfigUpdateCtx is passed to the OnConfigUpdate hook.
// Only the typed pair matching Field is populated; all other pairs are zero.
type ConfigUpdateCtx struct {
	Field ConfigField

	OldModel string
	NewModel string

	OldThinkingLevel llm.ThinkingLevel
	NewThinkingLevel llm.ThinkingLevel

	OldTools []RegisteredTool
	NewTools []RegisteredTool

	OldSystemPrompt string
	NewSystemPrompt string

	OldSkills []Skill
	NewSkills []Skill

	OldActiveTools []string
	NewActiveTools []string
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

// AfterTurnCtx is passed to the ShouldStopAfterTurn hook.
type AfterTurnCtx struct {
	// Turn is the 1-based index of the turn that just completed.
	Turn int
	// HadToolCalls reports whether the LLM returned tool calls this turn.
	HadToolCalls bool
}
