package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/resolute-sh/pi-llm-go"
)

// PromptResult is the terminal value delivered on EventStream.Done.
type PromptResult struct {
	Messages []Message
	Err      error
}

// promptRun holds the execution state of a single in-flight prompt. It is not
// part of the public surface; callers interact with the prompt through the
// EventStream returned by Agent.Prompt and the control methods on *Agent.
type promptRun struct {
	agent         *Agent
	optModel      string            // per-prompt model override (PromptOpts.Model)
	optThinking   llm.ThinkingLevel // per-prompt thinking override (PromptOpts.Thinking)
	providerHints llm.ProviderHints
	model         string            // last resolved model id, for State()
	thinking      llm.ThinkingLevel // last effective thinking level, for State()
	sessionID     SessionID

	mu              sync.Mutex
	phase           AgentPhase
	turn            int
	transcript      []Message
	branchSummaries []BranchSummary
	lastEvent       AgentEvent
	terminated      bool

	events     chan AgentEvent
	done       chan PromptResult
	steerCh    chan steerMsg
	followUpCh chan Message

	startedAt      time.Time
	lastActivityAt time.Time

	// ctx is the inner prompt context derived from the caller's context via
	// context.WithCancelCause. It is stored here so Agent.Context() can
	// expose it as a lifecycle accessor. Stored alongside cancel under
	// cancelMu; set in Agent.Prompt before loop() starts.
	//
	// This deviates from CTX-1 ("never store ctx in structs"): the field is an
	// exposure-only handle for the lifecycle accessor, not a context threaded
	// through call arguments — every function still takes ctx as its first
	// parameter and loop() receives the inner context explicitly. The standard
	// library does the same in net/http.Request, which stores its context for
	// Request.Context(). Sanctioned as deviation 6 in
	// docs/adr/0004-cancellation-deviations.md.
	ctx      context.Context
	cancel   context.CancelCauseFunc
	cancelMu sync.Mutex
}

// stop fire-and-forget cancels the prompt.
func (r *promptRun) stop() {
	r.cancelMu.Lock()
	if r.cancel != nil {
		r.cancel(ErrAgentStopped)
	}
	r.cancelMu.Unlock()
}

// steer enqueues a message for injection at the next safe point.
func (r *promptRun) steer(ctx context.Context, m Message) error {
	select {
	case r.steerCh <- steerMsg{msg: m, injected: make(chan struct{})}:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// followUp enqueues a message for after the current prompt completes.
func (r *promptRun) followUp(ctx context.Context, m Message) error {
	select {
	case r.followUpCh <- m:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// state returns a snapshot of the prompt's current state.
func (r *promptRun) state() AgentState {
	r.mu.Lock()
	defer r.mu.Unlock()
	var pending []PendingToolCall
	return AgentState{
		Phase:            r.phase,
		ActiveModel:      r.model,
		Thinking:         r.thinking,
		SessionID:        r.sessionID,
		TurnNumber:       r.turn,
		TranscriptLen:    len(r.transcript),
		PendingToolCalls: pending,
		StartedAt:        r.startedAt,
		LastActivityAt:   r.lastActivityAt,
	}
}

// transcriptCopy returns a copy of the current transcript.
func (r *promptRun) transcriptCopy() []Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Message, len(r.transcript))
	copy(out, r.transcript)
	return out
}

func (r *promptRun) setPhase(p AgentPhase) {
	r.mu.Lock()
	r.phase = p
	r.lastActivityAt = time.Now()
	r.mu.Unlock()
}

func (r *promptRun) emit(ev AgentEvent) {
	r.mu.Lock()
	r.lastEvent = ev
	r.lastActivityAt = time.Now()
	r.mu.Unlock()
	select {
	case r.events <- ev:
	case <-time.After(100 * time.Millisecond):
		// Drop event if buffer is full to avoid blocking.
	}
}

func (r *promptRun) loadTranscript(ctx context.Context) error {
	msgs, err := r.agent.session.Load(ctx, r.sessionID)
	if err != nil {
		return err
	}
	summaries, err := r.agent.session.LoadBranchSummaries(ctx, r.sessionID)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.transcript = msgs
	r.branchSummaries = summaries
	r.mu.Unlock()
	return nil
}

func (r *promptRun) appendTranscript(ctx context.Context, msgs ...Message) error {
	if err := r.agent.session.Append(ctx, r.sessionID, msgs...); err != nil {
		return err
	}
	r.mu.Lock()
	r.transcript = append(r.transcript, msgs...)
	r.mu.Unlock()
	return nil
}

type steerMsg struct {
	msg      Message
	injected chan struct{}
}

// flushPendingActiveTools drains active-tools changes queued by SetActiveTools
// during this prompt and appends each as an active_tools_change entry. Routing
// the append through appendTranscript keeps the session and the in-memory
// transcript in sync; it runs at the turn-end safe point so the entry lands on a
// turn boundary rather than mid-stream.
func (r *promptRun) flushPendingActiveTools(ctx context.Context) error {
	r.agent.mu.Lock()
	pending := r.agent.pendingActiveTools
	r.agent.pendingActiveTools = nil
	r.agent.mu.Unlock()
	for _, names := range pending {
		if err := r.appendTranscript(ctx, NewActiveToolsChange(names)); err != nil {
			return err
		}
	}
	return nil
}

// loop is the main prompt loop.
func (r *promptRun) loop(ctx context.Context) {
	defer close(r.events)
	defer close(r.done)
	// Drain the deferral queue on every loop-exit path (success, error,
	// cancellation), guaranteeing no entry is stranded or leaked into the next
	// prompt's session. Registered before running.Add(-1) so it runs after the
	// flag is cleared (defers run LIFO): once cleared, SetActiveTools persists
	// immediately instead of enqueuing, so nothing can race in behind this final
	// drain. Best-effort — the prompt has already concluded.
	defer func() { _ = r.flushPendingActiveTools(ctx) }()
	defer r.agent.running.Add(-1)

	r.emit(AgentStartEvent{})

	if err := r.loadTranscript(ctx); err != nil {
		r.emit(AgentEndEvent{Messages: r.transcriptCopy()})
		r.done <- PromptResult{Err: fmt.Errorf("loading transcript: %w", err)}
		return
	}

	// Emit MessageStart for the user prompt that was appended before the loop.
	if len(r.transcript) > 0 {
		lastMsg := r.transcript[len(r.transcript)-1]
		if lastMsg.Role == "user" {
			r.emit(MessageStartEvent{Role: "user", MessageType: "text"})
		}
	}

	for {
		hadToolCalls, err := r.runOneTurn(ctx)
		if err != nil {
			if errors.Is(err, ErrAgentStopped) || errors.Is(err, ErrPromptCancelled) {
				r.setPhase(PhaseShuttingDown)
				time.After(r.agent.config.ShutdownTimeout)
				r.setPhase(PhaseDone)
				r.emit(AgentEndEvent{Messages: r.transcriptCopy()})
				r.done <- PromptResult{Messages: r.transcriptCopy(), Err: err}
				return
			}
			r.emit(LLMErrorEvent{Error: err, Transient: false})
			r.setPhase(PhaseDone)
			r.emit(AgentEndEvent{Messages: r.transcriptCopy()})
			r.done <- PromptResult{Messages: r.transcriptCopy(), Err: err}
			return
		}

		if err := r.flushPendingActiveTools(ctx); err != nil {
			r.setPhase(PhaseDone)
			r.emit(AgentEndEvent{Messages: r.transcriptCopy()})
			r.done <- PromptResult{Messages: r.transcriptCopy(), Err: err}
			return
		}

		// Evaluate the predicate on every completed turn, including terminate
		// turns, matching upstream shouldStopAfterTurn side-effect semantics.
		// The return value is only acted on for non-terminate turns; terminate
		// exits unconditionally regardless of the predicate's answer.
		stopAfterTurn := false
		if r.agent.hooks.ShouldStopAfterTurn != nil {
			stopAfterTurn = r.agent.hooks.ShouldStopAfterTurn(ctx, AfterTurnCtx{
				Turn:         r.turn,
				HadToolCalls: hadToolCalls,
			})
		}

		if r.terminated {
			r.setPhase(PhaseDone)
			r.emit(AgentEndEvent{Messages: r.transcriptCopy()})
			r.done <- PromptResult{Messages: r.transcriptCopy(), Err: nil}
			return
		}

		if stopAfterTurn {
			r.setPhase(PhaseDone)
			r.emit(AgentEndEvent{Messages: r.transcriptCopy()})
			r.done <- PromptResult{Messages: r.transcriptCopy(), Err: nil}
			return
		}

		// A queued steer lands here, at the post-batch seam: the current turn's
		// tool batch has fully finished. Drain it non-blocking so it precedes any
		// auto-continue, driving the next LLM call with the steer in context and
		// never skipping pending tool calls mid-batch.
		select {
		case sm := <-r.steerCh:
			if err := r.appendTranscript(ctx, sm.msg); err != nil {
				r.emit(AgentEndEvent{Messages: r.transcriptCopy()})
				r.done <- PromptResult{Messages: r.transcriptCopy(), Err: err}
				return
			}
			r.emit(MessageStartEvent{Role: "user", MessageType: "text"})
			r.emit(SteerInjectedEvent{Message: sm.msg})
			close(sm.injected)
			continue
		default:
		}

		// A turn that called tools auto-continues: the Prompt contract spans one
		// or more LLM turns until the model stops calling tools, so the model must
		// see its tool results on the next call. Honor cancellation first so a
		// stopped prompt does not issue another LLM call.
		if hadToolCalls {
			if ctx.Err() != nil {
				r.setPhase(PhaseDone)
				r.emit(AgentEndEvent{Messages: r.transcriptCopy()})
				r.done <- PromptResult{Messages: r.transcriptCopy(), Err: context.Cause(ctx)}
				return
			}
			continue
		}

		// The model produced a text-only turn. Drain a queued follow-up, honor
		// cancellation, or finish.
		select {
		case fu := <-r.followUpCh:
			if err := r.appendTranscript(ctx, fu); err != nil {
				r.emit(AgentEndEvent{Messages: r.transcriptCopy()})
				r.done <- PromptResult{Messages: r.transcriptCopy(), Err: err}
				return
			}
			r.emit(MessageStartEvent{Role: "user", MessageType: "text"})
			r.emit(FollowUpInjectedEvent{Message: fu})
			continue
		case <-ctx.Done():
			r.setPhase(PhaseDone)
			r.emit(AgentEndEvent{Messages: r.transcriptCopy()})
			r.done <- PromptResult{Messages: r.transcriptCopy(), Err: context.Cause(ctx)}
			return
		default:
			r.setPhase(PhaseDone)
			r.emit(AgentEndEvent{Messages: r.transcriptCopy()})
			r.done <- PromptResult{Messages: r.transcriptCopy(), Err: nil}
			return
		}
	}
}

// runOneTurn executes a single LLM → tools → result turn. It returns whether
// this turn produced any tool calls, which the loop passes to ShouldStopAfterTurn.
func (r *promptRun) runOneTurn(ctx context.Context) (bool, error) {
	r.setPhase(PhaseWaitingLLM)
	r.turn++
	r.emit(TurnStartEvent{Turn: r.turn})

	if ctx.Err() != nil {
		return false, context.Cause(ctx)
	}

	// Take the turn snapshot from the Agent's current config, overlaid with any
	// per-prompt overrides. Setters called after this point affect the next
	// turn, never this one.
	snap := r.agent.snapshot()
	modelRef := r.optModel
	if modelRef == "" {
		modelRef = snap.model
	}
	providerName, modelID, err := parseModelRef(modelRef)
	if err != nil {
		return false, err
	}
	provider, err := r.agent.providerByName(providerName)
	if err != nil {
		return false, err
	}
	thinking := r.optThinking
	if thinking == llm.ThinkingOff {
		thinking = snap.thinkingLevel
	}
	r.mu.Lock()
	r.model = modelID
	r.thinking = thinking
	r.mu.Unlock()

	contextMsgs := BuildLLMContext(r.transcriptCopy(), r.branchSummaries)
	msgs := r.agent.config.ConvertToLLM(contextMsgs)
	if r.agent.hooks.TransformContext != nil {
		transformed, err := r.agent.hooks.TransformContext(ctx, TransformContextCtx{Messages: contextMsgs})
		if err != nil {
			return false, fmt.Errorf("transform context hook: %w", err)
		}
		msgs = r.agent.config.ConvertToLLM(transformed)
	}

	tools := make([]llm.ToolDef, len(snap.tools))
	for i, t := range snap.tools {
		tools[i] = llm.ToolDef{
			Name:        t.Name(),
			Description: t.Description(),
			Schema:      t.Schema(),
		}
	}

	caps := provider.Capabilities(modelID)
	if thinking != llm.ThinkingOff && !caps.Thinking {
		r.emit(ThinkingUnsupportedEvent{
			Requested: fmt.Sprintf("%v", thinking),
			Provider:  provider.Name(),
			Model:     modelID,
			Reason:    "model does not support thinking",
		})
		thinking = llm.ThinkingOff
	}

	req := llm.LLMRequest{
		Model:           modelID,
		Messages:        msgs,
		Tools:           tools,
		Thinking:        thinking,
		ThinkingBudgets: snap.thinkingBudgets,
		ProviderHints:   r.providerHints,
		SessionID:       string(r.sessionID),
		Transport:       snap.transport,
	}

	if r.agent.hooks.BeforeProviderRequest != nil {
		req.OnBeforeRequest = func(headers map[string]string) error {
			return r.agent.hooks.BeforeProviderRequest(ctx, BeforeProviderRequestCtx{
				Provider: provider.Name(),
				Model:    modelID,
				Headers:  headers,
			})
		}
	}
	if r.agent.hooks.AfterProviderResponse != nil {
		req.OnAfterResponse = func(statusCode int, headers map[string]string) {
			r.agent.hooks.AfterProviderResponse(ctx, AfterProviderResponseCtx{
				Provider:   provider.Name(),
				Model:      modelID,
				StatusCode: statusCode,
				Headers:    headers,
			})
		}
	}

	stream := provider.Stream(ctx, req)

	var toolCalls []llm.ToolCallContent
	var assistantText strings.Builder
	var messageStarted bool
	var assistantMsg Message

	for ev := range stream.Events {
		switch e := ev.(type) {
		case llm.TextDeltaEvent:
			if !messageStarted {
				messageStarted = true
				r.emit(MessageStartEvent{Role: "assistant", MessageType: "text"})
			}
			assistantText.WriteString(e.Delta)
			r.emit(TextDeltaEvent{Delta: e.Delta})
		case llm.ThinkingDeltaEvent:
			if !messageStarted {
				messageStarted = true
				r.emit(MessageStartEvent{Role: "assistant", MessageType: "thinking"})
			}
			r.emit(ThinkingDeltaEvent{Delta: e.Delta})
		case llm.ToolCallStartEvent:
			toolCalls = append(toolCalls, llm.ToolCallContent{
				CallID:   e.CallID,
				ToolName: e.ToolName,
				Args:     e.Args,
			})
			r.emit(ToolCallStartEvent{CallID: e.CallID, ToolName: e.ToolName, Args: e.Args})
		case llm.ToolCallEndEvent:
			r.emit(ToolCallEndEvent{CallID: e.CallID})
		case llm.LLMErrorEvent:
			if e.Transient {
				r.emit(LLMErrorEvent{Error: e.Error, Transient: true})
			} else {
				return false, fmt.Errorf("llm error: %w", e.Error)
			}
		case llm.LLMRetryEvent:
			r.emit(LLMRetryEvent{
				Provider:   e.Provider,
				Model:      e.Model,
				Attempt:    e.Attempt,
				NextDelay:  int64(e.NextDelay / time.Millisecond),
				Reason:     e.Reason,
				ServerHint: e.ServerHint,
			})
		case llm.UsageEvent:
			// Track usage if needed
		case llm.MessageEndEvent:
			if assistantText.Len() > 0 {
				assistantMsg = NewText("assistant", assistantText.String())
			}
			r.emit(MessageEndEvent{Message: assistantMsg})
		}
	}

	result := <-stream.Done
	if result.Err != nil {
		return false, fmt.Errorf("llm stream: %w", result.Err)
	}

	if assistantText.Len() > 0 {
		assistantMsg = NewText("assistant", assistantText.String())
		if err := r.appendTranscript(ctx, assistantMsg); err != nil {
			return false, err
		}
	}
	for _, tc := range toolCalls {
		assistantMsg = NewToolCall("assistant", tc.CallID, tc.ToolName, tc.Args)
		if err := r.appendTranscript(ctx, assistantMsg); err != nil {
			return false, err
		}
	}

	r.emit(TurnEndEvent{Turn: r.turn})

	hadToolCalls := len(toolCalls) > 0
	if hadToolCalls {
		if err := r.executeTools(ctx, toolCalls, snap.tools); err != nil {
			return false, err
		}
	}
	return hadToolCalls, nil
}

// preparedCall is the outcome of preflighting one tool call. When immediate is
// true the call is not executed and err carries the per-call failure (unknown
// tool or BeforeToolCall rejection); otherwise tool and args are ready to run.
type preparedCall struct {
	callID    string
	name      string
	args      json.RawMessage
	tool      RegisteredTool
	immediate bool
	err       error
}

// toolOutcome collects one tool call's result. It is indexed by the call's
// position in the assistant message so results persist in source order
// regardless of completion order.
type toolOutcome struct {
	callID string
	name   string
	result ToolResult
	err    error
}

func (r *promptRun) executeTools(ctx context.Context, toolCalls []llm.ToolCallContent, tools []RegisteredTool) error {
	r.setPhase(PhaseExecutingTools)
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}

	// Preflight (sequential, source order): resolve each tool and run its
	// BeforeToolCall hook. Once the prompt is aborted — e.g. a hook stops it —
	// stop preparing the remaining siblings; no preflight or work proceeds after
	// cancellation.
	preps := make([]preparedCall, len(toolCalls))
	for i, tc := range toolCalls {
		if ctx.Err() != nil {
			break
		}
		preps[i] = r.prepareToolCall(ctx, tc, tools)
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}

	// Execution (parallel, bounded by MaxParallelTools): runnable calls run
	// concurrently, each emitting its tool-end event as it finalizes (completion
	// order). Immediate outcomes are recorded in place.
	results := make([]toolOutcome, len(toolCalls))
	var wg sync.WaitGroup
	sema := make(chan struct{}, r.agent.config.MaxParallelTools)
	if r.agent.config.MaxParallelTools == 0 {
		sema = nil // unbounded
	}
	for i := range preps {
		p := preps[i]
		if p.immediate {
			results[i] = toolOutcome{callID: p.callID, name: p.name, err: p.err}
			continue
		}
		wg.Add(1)
		go func(idx int, p preparedCall) {
			defer wg.Done()
			if sema != nil {
				sema <- struct{}{}
				defer func() { <-sema }()
			}

			res, err := p.tool.Execute(ctx, p.callID, p.args)
			if err != nil {
				res = ToolResult{Content: err.Error(), IsError: true}
			}
			if r.agent.hooks.AfterToolCall != nil {
				if herr := r.agent.hooks.AfterToolCall(ctx, AfterToolCallCtx{CallID: p.callID, ToolName: p.name, Result: res}); herr != nil {
					res = ToolResult{Content: herr.Error(), IsError: true}
				}
			}

			results[idx] = toolOutcome{callID: p.callID, name: p.name, result: res}
			r.emit(ToolCallEndEvent{CallID: p.callID, ToolName: p.name, Result: res})
		}(i, p)
	}
	wg.Wait()

	if len(results) > 0 {
		allTerminate := true
		for _, res := range results {
			if res.err != nil || !res.result.Terminate {
				allTerminate = false
				break
			}
		}
		if allTerminate {
			r.mu.Lock()
			r.terminated = true
			r.mu.Unlock()
			return nil
		}
	}

	// Persist tool results in assistant source order. Any per-call error — an
	// unknown tool, a BeforeToolCall rejection, an execute failure, or an
	// AfterToolCall failure — lands as an error result carrying the error text,
	// never an empty success.
	for _, res := range results {
		content := res.result.Content
		isErr := res.result.IsError
		if res.err != nil {
			content = res.err.Error()
			isErr = true
		}
		msg := NewToolResult("tool", res.callID, res.name, content, res.result.Data, isErr)
		if err := r.appendTranscript(ctx, msg); err != nil {
			return err
		}
	}

	return nil
}

// prepareToolCall resolves a tool call's handler and runs the BeforeToolCall
// hook. A missing tool or a hook rejection returns an immediate failure (its
// ToolErrorEvent already emitted); otherwise the returned preparedCall is ready
// to execute.
func (r *promptRun) prepareToolCall(ctx context.Context, tc llm.ToolCallContent, tools []RegisteredTool) preparedCall {
	tool, ok := findTool(tools, tc.ToolName)
	if !ok {
		r.emit(ToolErrorEvent{CallID: tc.CallID, ToolName: tc.ToolName, Error: ErrToolNotFound})
		return preparedCall{callID: tc.CallID, name: tc.ToolName, immediate: true, err: ErrToolNotFound}
	}

	args := tc.Args
	if r.agent.hooks.BeforeToolCall != nil {
		hookCtx := BeforeToolCallCtx{CallID: tc.CallID, ToolName: tc.ToolName, Args: args}
		if err := r.agent.hooks.BeforeToolCall(ctx, hookCtx); err != nil {
			r.emit(ToolErrorEvent{CallID: tc.CallID, ToolName: tc.ToolName, Error: err})
			return preparedCall{callID: tc.CallID, name: tc.ToolName, immediate: true, err: err}
		}
		args = hookCtx.Args
	}

	return preparedCall{callID: tc.CallID, name: tc.ToolName, args: args, tool: tool}
}

func findTool(tools []RegisteredTool, name string) (RegisteredTool, bool) {
	for _, t := range tools {
		if t.Name() == name {
			return t, true
		}
	}
	return nil, false
}
