package pi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/resolute-sh/pi-llm-go"
)

// RunResult is the terminal value delivered on Run.Done.
type RunResult struct {
	Messages []Message
	Err      error
}

// Run represents a single live agent invocation.
type Run struct {
	agent         *Agent
	provider      llm.LLMProvider
	model         string
	thinking      llm.ThinkingLevel
	providerHints llm.ProviderHints
	sessionID     SessionID

	mu       sync.Mutex
	phase    RunPhase
	turn     int
	transcript []Message
	lastEvent  AgentEvent

	events     chan AgentEvent
	done       chan RunResult
	steerCh    chan steerMsg
	followUpCh chan Message

	startedAt      time.Time
	lastActivityAt time.Time

	// Internal cancellation
	cancel   context.CancelCauseFunc
	cancelMu sync.Mutex
}

// Events returns the event stream.
func (r *Run) Events() <-chan AgentEvent { return r.events }

// Done returns the terminal result channel.
func (r *Run) Done() <-chan RunResult { return r.done }

// Stop fire-and-forget cancels the run.
func (r *Run) Stop() {
	r.cancelMu.Lock()
	if r.cancel != nil {
		r.cancel(ErrRunStopped)
	}
	r.cancelMu.Unlock()
}

// Steer enqueues a message for injection at the next safe point.
func (r *Run) Steer(ctx context.Context, m Message) error {
	select {
	case r.steerCh <- steerMsg{msg: m, injected: make(chan struct{})}:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// FollowUp enqueues a message for after the current run completes.
func (r *Run) FollowUp(ctx context.Context, m Message) error {
	select {
	case r.followUpCh <- m:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// State returns a snapshot of the run's current state.
func (r *Run) State() RunState {
	r.mu.Lock()
	defer r.mu.Unlock()
	var pending []PendingToolCall
	// Pending tool calls are tracked during execution; simplified for v0.1.0.
	return RunState{
		Phase:          r.phase,
		ActiveModel:    r.model,
		Thinking:       r.thinking,
		SessionID:      r.sessionID,
		TurnNumber:     r.turn,
		TranscriptLen:  len(r.transcript),
		PendingToolCalls: pending,
		StartedAt:      r.startedAt,
		LastActivityAt: r.lastActivityAt,
	}
}

// Transcript returns a copy of the current transcript.
func (r *Run) Transcript() []Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Message, len(r.transcript))
	copy(out, r.transcript)
	return out
}

func (r *Run) setPhase(p RunPhase) {
	r.mu.Lock()
	r.phase = p
	r.lastActivityAt = time.Now()
	r.mu.Unlock()
}

func (r *Run) emit(ev AgentEvent) {
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

func (r *Run) loadTranscript(ctx context.Context) error {
	msgs, err := r.agent.session.Load(ctx, r.sessionID)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.transcript = msgs
	r.mu.Unlock()
	return nil
}

func (r *Run) appendTranscript(ctx context.Context, msgs ...Message) error {
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

// loop is the main run loop.
func (r *Run) loop(ctx context.Context) {
	defer close(r.events)
	defer close(r.done)
	defer r.agent.running.Add(-1)

	// Load existing transcript
	if err := r.loadTranscript(ctx); err != nil {
		r.done <- RunResult{Err: fmt.Errorf("loading transcript: %w", err)}
		return
	}

	// Main follow-up loop
	for {
		if err := r.runOneTurn(ctx); err != nil {
			if errors.Is(err, ErrRunStopped) || errors.Is(err, ErrRunCancelled) {
				r.setPhase(PhaseShuttingDown)
				// Wait for tools with shutdown timeout
				time.After(r.agent.config.ShutdownTimeout)
				r.setPhase(PhaseDone)
				r.done <- RunResult{Messages: r.Transcript(), Err: err}
				return
			}
			r.emit(LLMErrorEvent{Error: err, Transient: false})
			r.setPhase(PhaseDone)
			r.done <- RunResult{Messages: r.Transcript(), Err: err}
			return
		}

		// Check for follow-up messages
		select {
		case fu := <-r.followUpCh:
			if err := r.appendTranscript(ctx, fu); err != nil {
				r.done <- RunResult{Messages: r.Transcript(), Err: err}
				return
			}
			r.emit(FollowUpInjectedEvent{Message: fu})
			continue
		case <-ctx.Done():
			r.setPhase(PhaseDone)
			r.done <- RunResult{Messages: r.Transcript(), Err: context.Cause(ctx)}
			return
		default:
			r.setPhase(PhaseDone)
			r.done <- RunResult{Messages: r.Transcript(), Err: nil}
			return
		}
	}
}

// runOneTurn executes a single LLM → tools → result turn.
func (r *Run) runOneTurn(ctx context.Context) error {
	r.setPhase(PhaseWaitingLLM)
	r.turn++
	r.emit(TurnStartEvent{Turn: r.turn})

	// Check for steer messages before LLM call
	select {
	case sm := <-r.steerCh:
		if err := r.appendTranscript(ctx, sm.msg); err != nil {
			return err
		}
		r.emit(SteerInjectedEvent{Message: sm.msg})
		close(sm.injected)
	case <-ctx.Done():
		return context.Cause(ctx)
	default:
	}

	// Build LLM request
	msgs := r.agent.config.ConvertToLLM(r.Transcript())
	if r.agent.hooks.TransformContext != nil {
		transformed, err := r.agent.hooks.TransformContext(ctx, TransformContextCtx{Messages: r.Transcript()})
		if err != nil {
			return fmt.Errorf("transform context hook: %w", err)
		}
		msgs = r.agent.config.ConvertToLLM(transformed)
	}

	tools := make([]llm.ToolDef, len(r.agent.config.Tools))
	for i, t := range r.agent.config.Tools {
		tools[i] = llm.ToolDef{
			Name:        t.Name(),
			Description: t.Description(),
			Schema:      t.Schema(),
		}
	}

	caps := r.provider.Capabilities(r.model)
	thinking := r.thinking
	if thinking != llm.ThinkingOff && !caps.Thinking {
		r.emit(ThinkingUnsupportedEvent{
			Requested: fmt.Sprintf("%v", thinking),
			Provider:  r.provider.Name(),
			Model:     r.model,
			Reason:    "model does not support thinking",
		})
		thinking = llm.ThinkingOff
	}

	req := llm.LLMRequest{
		Model:         r.model,
		Messages:      msgs,
		Tools:         tools,
		Thinking:      thinking,
		ProviderHints: r.providerHints,
	}

	stream := r.provider.Stream(ctx, req)

	// Consume stream
	var toolCalls []llm.ToolCallContent
	var assistantText strings.Builder

	for ev := range stream.Events {
		switch e := ev.(type) {
		case llm.TextDeltaEvent:
			assistantText.WriteString(e.Delta)
			r.emit(TextDeltaEvent{Delta: e.Delta})
		case llm.ThinkingDeltaEvent:
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
				return fmt.Errorf("llm error: %w", e.Error)
			}
		case llm.LLMRetryEvent:
			r.emit(LLMRetryEvent{
				Provider:   e.Provider,
				Model:      e.Model,
				Attempt:    e.Attempt,
				NextDelay:   int64(e.NextDelay / time.Millisecond),
				Reason:     e.Reason,
				ServerHint: e.ServerHint,
			})
		case llm.UsageEvent:
			// Track usage if needed
		case llm.MessageEndEvent:
			// End of assistant message
		}
	}

	result := <-stream.Done
	if result.Err != nil {
		return fmt.Errorf("llm stream: %w", result.Err)
	}

	// Append assistant response to transcript
	if assistantText.Len() > 0 {
		msg := NewText("assistant", assistantText.String())
		if err := r.appendTranscript(ctx, msg); err != nil {
			return err
		}
	}
	for _, tc := range toolCalls {
		msg := NewToolCall("assistant", tc.CallID, tc.ToolName, tc.Args)
		if err := r.appendTranscript(ctx, msg); err != nil {
			return err
		}
	}

	r.emit(TurnEndEvent{Turn: r.turn})

	// Execute tools if any
	if len(toolCalls) > 0 {
		if err := r.executeTools(ctx, toolCalls); err != nil {
			return err
		}
		// After tools, loop for next LLM turn
		return nil
	}

	return nil
}

func (r *Run) executeTools(ctx context.Context, toolCalls []llm.ToolCallContent) error {
	r.setPhase(PhaseExecutingTools)

	// Check for steer messages before tool execution
	select {
	case sm := <-r.steerCh:
		if err := r.appendTranscript(ctx, sm.msg); err != nil {
			return err
		}
		r.emit(SteerInjectedEvent{Message: sm.msg})
		close(sm.injected)
		return nil // Skip tool execution, go back to LLM
	case <-ctx.Done():
		return context.Cause(ctx)
	default:
	}

	var wg sync.WaitGroup
	sema := make(chan struct{}, r.agent.config.MaxParallelTools)
	if r.agent.config.MaxParallelTools == 0 {
		sema = nil // unbounded
	}
	results := make([]struct {
		callID string
		name   string
		result ToolResult
		err    error
	}, len(toolCalls))

	for i, tc := range toolCalls {
		wg.Add(1)
		go func(idx int, call llm.ToolCallContent) {
			defer wg.Done()
			if sema != nil {
				sema <- struct{}{}
				defer func() { <-sema }()
			}

			tool, ok := r.findTool(call.ToolName)
			if !ok {
				results[idx] = struct {
					callID string
					name   string
					result ToolResult
					err    error
				}{callID: call.CallID, name: call.ToolName, err: ErrToolNotFound}
				r.emit(ToolErrorEvent{CallID: call.CallID, ToolName: call.ToolName, Error: ErrToolNotFound})
				return
			}

			if r.agent.hooks.BeforeToolCall != nil {
				args := call.Args
				hookCtx := BeforeToolCallCtx{CallID: call.CallID, ToolName: call.ToolName, Args: args}
				if err := r.agent.hooks.BeforeToolCall(ctx, hookCtx); err != nil {
					results[idx] = struct {
						callID string
						name   string
						result ToolResult
						err    error
					}{callID: call.CallID, name: call.ToolName, err: err}
					r.emit(ToolErrorEvent{CallID: call.CallID, ToolName: call.ToolName, Error: err})
					return
				}
				call.Args = hookCtx.Args
			}

			res, err := tool.Execute(ctx, call.CallID, call.Args)
			if err != nil {
				res = ToolResult{Content: err.Error(), IsError: true}
			}

			if r.agent.hooks.AfterToolCall != nil {
				r.agent.hooks.AfterToolCall(ctx, AfterToolCallCtx{CallID: call.CallID, ToolName: call.ToolName, Result: res})
			}

			results[idx] = struct {
				callID string
				name   string
				result ToolResult
				err    error
			}{callID: call.CallID, name: call.ToolName, result: res}
			r.emit(ToolCallEndEvent{CallID: call.CallID, ToolName: call.ToolName, Result: res})
		}(i, tc)
	}

	wg.Wait()

	// Append tool results to transcript in call-ID order
	for _, res := range results {
		msg := NewToolResult("tool", res.callID, res.name, res.result.Content, res.result.Data, res.result.IsError)
		if err := r.appendTranscript(ctx, msg); err != nil {
			return err
		}
	}

	return nil
}

func (r *Run) findTool(name string) (RegisteredTool, bool) {
	for _, t := range r.agent.config.Tools {
		if t.Name() == name {
			return t, true
		}
	}
	return nil, false
}
