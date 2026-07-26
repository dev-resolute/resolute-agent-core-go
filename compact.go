package pi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dev-resolute/resolute-llm-go"
)

// SummarizationSystemPrompt is the system prompt used for summarization calls.
// Matches upstream 0.79.1 — neutral "AI assistant" wording so compaction works
// correctly for non-coding harnesses.
const SummarizationSystemPrompt = `You are a context summarization assistant. Your task is to read a conversation between a user and an AI assistant, then produce a structured summary following the exact format specified.

Do NOT continue the conversation. Do NOT respond to any questions in the conversation. ONLY output the structured summary.`

// SummarizationPrompt is the user prompt for the first summarization of a conversation prefix.
// Aligned with upstream 0.79.1 structured template.
const SummarizationPrompt = `The messages above are a conversation to summarize. Create a structured context checkpoint summary that another LLM will use to continue the work.

Use this EXACT format:

## Goal
[What is the user trying to accomplish? Can be multiple items if the session covers different tasks.]

## Constraints & Preferences
- [Any constraints, preferences, or requirements mentioned by user]
- [Or "(none)" if none were mentioned]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Current work]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered list of what should happen next]

## Critical Context
- [Any data, examples, or references needed to continue]
- [Or "(none)" if not applicable]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

// UpdateSummarizationPrompt is used when updating an existing summary with new messages.
// Aligned with upstream 0.79.1 structured template.
// Adaptation: upstream references "<previous-summary> tags" (injected inline in TS);
// Go passes the previous summary as a BranchSummary message above, so that phrase is reworded.
const UpdateSummarizationPrompt = `The messages above are NEW conversation messages to incorporate into the existing summary provided as a prior compaction summary message in this conversation.

Update the existing structured summary with new information. RULES:
- PRESERVE all existing information from the previous summary
- ADD new progress, decisions, and context from the new messages
- UPDATE the Progress section: move items from "In Progress" to "Done" when completed
- UPDATE "Next Steps" based on what was accomplished
- PRESERVE exact file paths, function names, and error messages
- If something is no longer relevant, you may remove it

Use this EXACT format:

## Goal
[Preserve existing goals, add new ones if the task expanded]

## Constraints & Preferences
- [Preserve existing, add new ones discovered]

## Progress
### Done
- [x] [Include previously done items AND newly completed items]

### In Progress
- [ ] [Current work - update based on progress]

### Blocked
- [Current blockers - remove if resolved]

## Key Decisions
- **[Decision]**: [Brief rationale] (preserve all previous, add new)

## Next Steps
1. [Update based on current state]

## Critical Context
- [Preserve important context, add new if needed]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

// TurnPrefixSummarizationPrompt is used for the turn-prefix summarization in split-turn compaction.
// Aligned with upstream 0.79.1 structured template.
const TurnPrefixSummarizationPrompt = `This is the PREFIX of a turn that was too large to keep. The SUFFIX (recent work) is retained.

Summarize the prefix to provide context for the retained suffix:

## Original Request
[What did the user ask for in this turn?]

## Early Progress
- [Key decisions and work done in the prefix]

## Context for Suffix
- [Information needed to understand the retained recent work]

Be concise. Focus on what's needed to understand the kept suffix.`

// CompactionSettings controls when and how compaction runs.
type CompactionSettings struct {
	Enabled          bool
	ReserveTokens    int
	KeepRecentTokens int
}

// CompactOpts carries options for a compaction operation.
type CompactOpts struct {
	KeepRecentTokens int
	// SessionID selects the session to compact. Empty means the session
	// bound by the most recent prompt — which is also empty on an Agent
	// that has never prompted, making Compact a silent no-op there; set
	// SessionID explicitly to compact such a session (e.g. a harness
	// compacting a durable conversation with a fresh Agent).
	SessionID SessionID
}

// DefaultCompactionSettings matches upstream's DEFAULT_COMPACTION_SETTINGS.
var DefaultCompactionSettings = CompactionSettings{
	Enabled:          true,
	ReserveTokens:    16384,
	KeepRecentTokens: 20000,
}

// ShouldCompact returns true when the context has grown large enough to warrant compaction.
// It is exported so callers can build their own auto-trigger logic.
func ShouldCompact(contextTokens, contextWindow int, settings CompactionSettings) bool {
	if !settings.Enabled || contextWindow <= 0 {
		return false
	}
	return contextTokens > contextWindow-settings.ReserveTokens
}

// EstimateTokens returns a rough token estimate using the chars/4 heuristic.
// Per ADR-0003, this is a coarse approximation until local tokenizers land.
// Images count 4800 chars each (upstream 0.76.0 attachment heuristic, AGENT-14); conservative per ADR-0003.
func EstimateTokens(messages []Message) int {
	var chars int
	for _, msg := range messages {
		chars += len(msg.Body)
		chars += len(msg.Role)
		chars += len(msg.Type)
		chars += 4800 * len(msg.Images)
	}
	return (chars + 3) / 4 // round up
}

// BuildLLMContext returns a message slice with BranchSummary messages substituted
// for the ranges they cover. Summaries are sorted by StartIdx and applied in
// order. Bookkeeping entries (active_tools_change) are stripped — they are state,
// not conversation, and must never reach the model.
func BuildLLMContext(transcript []Message, summaries []BranchSummary) []Message {
	if len(summaries) == 0 {
		return excludeBookkeeping(transcript)
	}
	// Sort summaries by StartIdx ascending (stable).
	sorted := make([]BranchSummary, len(summaries))
	copy(sorted, summaries)
	for i := 0; i < len(sorted)-1; i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].StartIdx < sorted[i].StartIdx {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	var out []Message
	var lastEnd int
	for _, s := range sorted {
		if s.StartIdx < lastEnd {
			// Overlapping summary — skip to preserve correctness.
			continue
		}
		// Append messages before this summary.
		if lastEnd < s.StartIdx && s.StartIdx <= len(transcript) {
			out = append(out, transcript[lastEnd:s.StartIdx]...)
		}
		// Append the summary message.
		out = append(out, NewBranchSummaryMessage(s.Summary))
		lastEnd = s.EndIdx
	}
	// Append remaining messages after the last summary.
	if lastEnd < len(transcript) {
		out = append(out, transcript[lastEnd:]...)
	}
	return excludeBookkeeping(out)
}

// excludeBookkeeping returns msgs without active_tools_change entries, sharing the
// backing array when there is nothing to strip.
func excludeBookkeeping(msgs []Message) []Message {
	for _, m := range msgs {
		if m.Type == "active_tools_change" {
			out := make([]Message, 0, len(msgs))
			for _, m := range msgs {
				if m.Type != "active_tools_change" {
					out = append(out, m)
				}
			}
			return out
		}
	}
	return msgs
}

// CompactResult carries the outcome of a compaction.
type CompactResult struct {
	Summary      BranchSummary
	RemovedCount int
}

// Compact collapses older transcript messages into a BranchSummary.
// It must be called when the agent is idle (no in-flight run).
func (a *Agent) Compact(ctx context.Context, opts CompactOpts) (*CompactResult, error) {
	if a.isRunning() {
		return nil, fmt.Errorf("agent is busy: %w", ErrAgentBusy)
	}

	settings := CompactionSettings{
		Enabled:          true,
		ReserveTokens:    a.config.ReserveTokens,
		KeepRecentTokens: a.config.KeepRecentTokens,
	}
	if opts.KeepRecentTokens > 0 {
		settings.KeepRecentTokens = opts.KeepRecentTokens
	}

	sid := opts.SessionID
	if sid == "" {
		sid = a.sessionIDFromConfigOrLastRun()
	}
	if sid == "" {
		// No session to compact.
		return &CompactResult{}, nil
	}

	msgs, err := a.session.Load(ctx, sid)
	if err != nil {
		return nil, fmt.Errorf("loading session: %w", err)
	}

	prep, err := prepareCompaction(msgs, settings)
	if err != nil {
		return nil, fmt.Errorf("preparing compaction: %w", err)
	}
	if prep == nil {
		// Nothing to compact.
		return &CompactResult{}, nil
	}

	// Fire BeforeCompact hook.
	if a.hooks.BeforeCompact != nil {
		if err := a.hooks.BeforeCompact(ctx, BeforeCompactCtx{
			SessionID: sid,
			CutPoint:  prep.cutIdx,
		}); err != nil {
			return nil, fmt.Errorf("before compact hook: %w", err)
		}
	}

	summary, usage, err := a.summarize(ctx, sid, prep)
	if err != nil {
		return nil, fmt.Errorf("summarization failed: %w", err)
	}

	bs := BranchSummary{
		StartIdx:  0,
		EndIdx:    prep.cutIdx,
		Summary:   summary,
		CreatedAt: time.Now(),
		Usage:     usage,
	}

	if err := a.session.AppendBranchSummary(ctx, sid, bs); err != nil {
		return nil, fmt.Errorf("persisting branch summary: %w", err)
	}

	// Fire AfterCompact hook.
	if a.hooks.AfterCompact != nil {
		a.hooks.AfterCompact(ctx, AfterCompactCtx{
			SessionID:     sid,
			BranchSummary: bs,
		})
	}

	return &CompactResult{
		Summary:      bs,
		RemovedCount: prep.cutIdx,
	}, nil
}

// sessionIDFromConfigOrLastRun returns a session ID to compact.
func (a *Agent) sessionIDFromConfigOrLastRun() SessionID {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastSessionID
}

// compactionPrep holds the computed plan for a compaction.
type compactionPrep struct {
	cutIdx   int
	prefix   []Message
	keptTail []Message
}

// prepareCompaction determines whether compaction is needed and computes the cut point.
func prepareCompaction(msgs []Message, settings CompactionSettings) (*compactionPrep, error) {
	if len(msgs) == 0 {
		return nil, nil
	}
	// If the last entry is already a branch summary, nothing to do.
	if msgs[len(msgs)-1].Type == "branch_summary" {
		return nil, nil
	}

	cutIdx := findCutPoint(msgs, settings.KeepRecentTokens)
	if cutIdx <= 0 {
		return nil, nil
	}

	return &compactionPrep{
		cutIdx:   cutIdx,
		prefix:   msgs[:cutIdx],
		keptTail: msgs[cutIdx:],
	}, nil
}

// findCutPoint walks messages from newest to oldest, accumulating tokens.
// It returns the first valid cut point index (0-based) that leaves approximately
// KeepRecentTokens of retained history. Valid cut points exclude tool_result entries.
// TODO(v0.x): see ADR-0003 — tool_call/tool_result atomicity not enforced.
func findCutPoint(msgs []Message, keepRecentTokens int) int {
	var tokens int
	for i := len(msgs) - 1; i >= 0; i-- {
		tokens += estimateMessageTokens(msgs[i])
		if tokens >= keepRecentTokens {
			// We need to cut at or before i. Walk forward to find a valid cut point.
			for j := i; j < len(msgs); j++ {
				if isValidCutPoint(msgs[j]) {
					return j
				}
			}
			return i
		}
	}
	return 0
}

func estimateMessageTokens(m Message) int {
	// Images count 4800 chars each (upstream 0.76.0 attachment heuristic, AGENT-14); conservative per ADR-0003.
	chars := len(m.Body) + len(m.Role) + len(m.Type) + 4800*len(m.Images)
	return (chars + 3) / 4
}

func isValidCutPoint(m Message) bool {
	if m.Type == "tool_result" || m.Type == "active_tools_change" {
		return false
	}
	// tool_call, text, thinking, branch_summary, and user-defined types are valid.
	return true
}

// summarize runs the LLM summarization for the given compaction plan.
func (a *Agent) summarize(ctx context.Context, sid SessionID, prep *compactionPrep) (string, *Usage, error) {
	// Use the agent's default provider + model for summarization.
	provider := a.config.Providers[0]
	model := a.config.DefaultModel
	if model == "" {
		return "", nil, fmt.Errorf("no model configured for summarization: %w", ErrInvalidModel)
	}
	_, modelID, err := parseModelRef(model)
	if err != nil {
		return "", nil, err
	}

	// The instruction prompts say "the messages above", so the transcript to
	// summarize must precede the instruction (AGENT-17).
	var prefixMsgs []Message

	// Check if there is an existing summary to update.
	summaries, err := a.session.LoadBranchSummaries(ctx, sid)
	if err != nil {
		return "", nil, err
	}

	if len(summaries) > 0 {
		lastSummary := summaries[len(summaries)-1]
		// TODO(v0.x): see ADR-0003 — system message can be folded into summary.
		// Include the existing summary, the new prefix, then the instruction.
		prefixMsgs = append([]Message{NewSystem(SummarizationSystemPrompt)}, NewBranchSummaryMessage(lastSummary.Summary))
		prefixMsgs = append(prefixMsgs, prep.prefix...)
		prefixMsgs = append(prefixMsgs, NewText("user", UpdateSummarizationPrompt))
	} else {
		prefixMsgs = append([]Message{NewSystem(SummarizationSystemPrompt)}, prep.prefix...)
		prefixMsgs = append(prefixMsgs, NewText("user", SummarizationPrompt))
	}

	// Detect mid-turn cut and use split-turn summarization if needed.
	if len(prep.keptTail) > 0 && prep.cutIdx > 0 && prep.prefix[len(prep.prefix)-1].Type == "tool_call" {
		// The cut landed after a tool_call but before its result — mid-turn.
		return a.splitTurnSummarize(ctx, provider, modelID, prep)
	}

	return a.summarizeWithLLM(ctx, provider, modelID, prefixMsgs)
}

// splitTurnSummarize runs two summarization calls when the cut point falls
// mid-turn, strictly in sequence (upstream #5536: single-concurrency
// providers must not see overlapping generations; serial calls also keep
// OnSummarizationRetry lifecycle events ordered). The turn-prefix call is
// never issued if history summarization fails.
func (a *Agent) splitTurnSummarize(ctx context.Context, provider llm.LLMProvider, modelID string, prep *compactionPrep) (string, *Usage, error) {
	historyMsgs := append([]Message{NewSystem(SummarizationSystemPrompt)}, prep.prefix...)
	historyMsgs = append(historyMsgs, NewText("user", SummarizationPrompt))

	historySummary, historyUsage, err := a.summarizeWithLLM(ctx, provider, modelID, historyMsgs)
	if err != nil {
		return "", nil, fmt.Errorf("history summarization: %w", err)
	}

	turnMsgs := append([]Message{NewSystem(SummarizationSystemPrompt)}, prep.prefix...)
	turnMsgs = append(turnMsgs, NewText("user", TurnPrefixSummarizationPrompt))

	turnSummary, turnUsage, err := a.summarizeWithLLM(ctx, provider, modelID, turnMsgs)
	if err != nil {
		return "", nil, fmt.Errorf("turn prefix summarization: %w", err)
	}

	combined := historySummary + "\n\n---\n\n**Turn Context (split turn):**\n\n" + turnSummary
	return combined, combineUsage(historyUsage, turnUsage), nil
}

// combineUsage sums two optional usages; a single non-nil side stands alone
// (upstream compaction.ts combineUsage semantics).
func combineUsage(a, b *Usage) *Usage {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return &Usage{
		InputTokens:  a.InputTokens + b.InputTokens,
		OutputTokens: a.OutputTokens + b.OutputTokens,
	}
}

// summarizeWithLLM calls the provider to produce a summary from the given
// messages, retrying transient failures per AgentConfig.SummarizationRetry
// (upstream 0.81.1 parity). Retry lifecycle is reported through the
// OnSummarizationRetry hook; the hook never fires when the policy disables
// retries or the first call succeeds.
func (a *Agent) summarizeWithLLM(ctx context.Context, provider llm.LLMProvider, modelID string, msgs []Message) (string, *Usage, error) {
	policy := a.config.SummarizationRetry.normalized()

	summary, usage, err := a.summarizeOnce(ctx, provider, modelID, msgs)
	if err == nil || policy.MaxRetries == 0 || !isTransientSummarizationError(err) {
		return summary, usage, err
	}

	for attempt := 1; ; attempt++ {
		delay := policy.backoff(attempt)
		a.notifySummarizationRetry(ctx, SummarizationRetryCtx{
			Phase:       SummarizationRetryScheduled,
			Attempt:     attempt,
			MaxAttempts: policy.MaxRetries,
			Delay:       delay,
			Err:         err,
		})
		if sleepErr := sleepCtx(ctx, delay); sleepErr != nil {
			a.notifySummarizationRetry(ctx, SummarizationRetryCtx{
				Phase:   SummarizationRetryFinished,
				Attempt: attempt,
				Err:     err,
			})
			return "", nil, fmt.Errorf("summarization retry wait: %w", sleepErr)
		}
		a.notifySummarizationRetry(ctx, SummarizationRetryCtx{
			Phase:   SummarizationRetryAttemptStart,
			Attempt: attempt,
		})

		summary, usage, err = a.summarizeOnce(ctx, provider, modelID, msgs)
		if err == nil {
			a.notifySummarizationRetry(ctx, SummarizationRetryCtx{
				Phase:   SummarizationRetryFinished,
				Attempt: attempt,
				Success: true,
			})
			return summary, usage, nil
		}
		if attempt >= policy.MaxRetries || !isTransientSummarizationError(err) {
			a.notifySummarizationRetry(ctx, SummarizationRetryCtx{
				Phase:   SummarizationRetryFinished,
				Attempt: attempt,
				Err:     err,
			})
			return "", nil, err
		}
	}
}

// summarizeOnce performs a single summarization call and collects the streamed
// text and token usage. Callers wanting retry behavior should call
// summarizeWithLLM.
func (a *Agent) summarizeOnce(ctx context.Context, provider llm.LLMProvider, modelID string, msgs []Message) (string, *Usage, error) {
	llmMsgs := DefaultConvertToLLM(msgs)
	req := llm.LLMRequest{
		Model:    modelID,
		Messages: llmMsgs,
	}

	stream := provider.Stream(ctx, req)
	var summary strings.Builder
	var usage *Usage
	for ev := range stream.Events {
		switch e := ev.(type) {
		case llm.TextDeltaEvent:
			summary.WriteString(e.Delta)
		case llm.UsageEvent:
			if usage == nil {
				usage = &Usage{}
			}
			usage.InputTokens += e.InputTokens
			usage.OutputTokens += e.OutputTokens
		}
	}
	result := <-stream.Done
	if result.Err != nil {
		return "", nil, result.Err
	}
	return strings.TrimSpace(summary.String()), usage, nil
}

// notifySummarizationRetry fires the OnSummarizationRetry hook if configured.
func (a *Agent) notifySummarizationRetry(ctx context.Context, c SummarizationRetryCtx) {
	if a.hooks.OnSummarizationRetry != nil {
		a.hooks.OnSummarizationRetry(ctx, c)
	}
}
