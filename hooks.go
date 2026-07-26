package pi

import (
	"context"
	"time"

	"github.com/dev-resolute/resolute-llm-go"
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

	// OnSummarizationRetry is called at each retry-lifecycle point when a
	// summarization call made by Compact fails transiently and
	// AgentConfig.SummarizationRetry allows a retry. It is never called when
	// the policy disables retries or the first call succeeds. Calls are
	// serial: split-turn summarization runs its two summarization calls in
	// sequence, so lifecycle events never interleave. It must not call back
	// into the Agent. Nil is a no-op.
	OnSummarizationRetry func(ctx context.Context, c SummarizationRetryCtx)

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
	// ConfigFieldModel reports a SetModel change.
	ConfigFieldModel ConfigField = "model"
	// ConfigFieldThinkingLevel reports a SetThinkingLevel change.
	ConfigFieldThinkingLevel ConfigField = "thinking_level"
	// ConfigFieldTools reports a SetTools change.
	ConfigFieldTools ConfigField = "tools"
	// ConfigFieldSystemPrompt reports a SetSystemPrompt change.
	ConfigFieldSystemPrompt ConfigField = "system_prompt"
	// ConfigFieldSkills reports a SetSkills change.
	ConfigFieldSkills ConfigField = "skills"
	// ConfigFieldActiveTools reports a SetActiveTools change.
	ConfigFieldActiveTools ConfigField = "active_tools"
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

// SummarizationRetryPhase identifies which point of the retry lifecycle an
// OnSummarizationRetry call reports. Mirrors upstream 0.81.1's
// summarization_retry_scheduled / _attempt_start / _finished events.
type SummarizationRetryPhase int

const (
	_ SummarizationRetryPhase = iota // zero value is invalid
	// SummarizationRetryScheduled fires before the backoff sleep of each retry.
	SummarizationRetryScheduled
	// SummarizationRetryAttemptStart fires after the backoff sleep, immediately
	// before the retried call starts.
	SummarizationRetryAttemptStart
	// SummarizationRetryFinished fires once when the retry loop ends, whether
	// it succeeded, exhausted its budget, or was aborted during backoff.
	SummarizationRetryFinished
)

// SummarizationRetryCtx describes one retry-lifecycle point of a failed
// summarization call (Compact's first summary, summary update, or split-turn
// pair).
type SummarizationRetryCtx struct {
	Phase SummarizationRetryPhase
	// Attempt is the 1-indexed retry attempt this point belongs to.
	Attempt int
	// MaxAttempts is the configured MaxRetries. Set on Scheduled.
	MaxAttempts int
	// Delay is the backoff about to be slept. Set on Scheduled.
	Delay time.Duration
	// Success reports the loop outcome. Set on Finished.
	Success bool
	// Err is the error that triggered this retry on Scheduled, and the final
	// error on an unsuccessful Finished. Nil on AttemptStart and on a
	// successful Finished.
	Err error
}
