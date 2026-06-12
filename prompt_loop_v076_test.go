package pi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resolute-sh/pi-llm-go"
)

// echoParams is the argument shape for the echo tool used by the v0.76 loop
// regression fixtures.
type echoParams struct {
	Value string `json:"value"`
}

func echoArgs(value string) json.RawMessage {
	b, _ := json.Marshal(echoParams{Value: value})
	return b
}

// loopProvider is a hermetic provider whose per-call event sequence is chosen by
// callNum (1-based). It records each call's request so tests can assert on the
// context the loop assembled for later turns.
type loopProvider struct {
	emit func(call int, req llm.LLMRequest, events chan<- llm.LLMEvent)

	mu    sync.Mutex
	calls int
	reqs  []llm.LLMRequest
}

func (p *loopProvider) Name() string { return "test" }

func (p *loopProvider) Capabilities(string) llm.ProviderCapabilities {
	return llm.ProviderCapabilities{Streaming: true, ToolCalling: true}
}

func (p *loopProvider) Stream(ctx context.Context, req llm.LLMRequest) llm.EventStream {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()

	events := make(chan llm.LLMEvent)
	done := make(chan llm.StreamResult, 1)
	go func() {
		p.emit(call, req, events)
		close(events)
		done <- llm.StreamResult{}
	}()
	return llm.NewEventStream(events, done)
}

func (p *loopProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *loopProvider) requestForCall(call int) (llm.LLMRequest, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if call < 1 || call > len(p.reqs) {
		return llm.LLMRequest{}, false
	}
	return p.reqs[call-1], true
}

func requestMentions(req llm.LLMRequest, needle string) bool {
	for _, m := range req.Messages {
		if tc, ok := m.Content.(llm.TextContent); ok && strings.Contains(tc.Text, needle) {
			return true
		}
	}
	return false
}

func requestHasToolResult(req llm.LLMRequest, callID, content string) bool {
	for _, m := range req.Messages {
		if tr, ok := m.Content.(llm.ToolResultContent); ok && tr.CallID == callID && tr.Content == content {
			return true
		}
	}
	return false
}

func toolResults(msgs []Message) map[string]struct {
	content string
	isError bool
} {
	out := make(map[string]struct {
		content string
		isError bool
	})
	for _, m := range msgs {
		if m.Type != "tool_result" {
			continue
		}
		callID, _, content, _, isError, ok := m.ToolResult()
		if !ok {
			continue
		}
		out[callID] = struct {
			content string
			isError bool
		}{content: content, isError: isError}
	}
	return out
}

// Fix A (v0.76 core-loop parity): a turn whose response contained tool calls
// auto-continues — the model gets a second LLM call carrying the tool results,
// with no external steer or follow-up. The prompt ends once the model stops
// calling tools. This is the Prompt contract: "spans one or more LLM turns until
// the model stops calling tools".
func TestToolCallTurnAutoContinues_v076(t *testing.T) {
	t.Parallel()

	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			if call == 1 {
				events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: "echo", Args: echoArgs("ping")}
				events <- llm.ToolCallEndEvent{CallID: "c1"}
				events <- llm.MessageEndEvent{}
				return
			}
			events <- llm.TextDeltaEvent{Delta: "done"}
			events <- llm.MessageEndEvent{}
		},
	}

	echo := NewTool(Tool[echoParams]{
		Name:        "echo",
		Description: "echo",
		Execute: func(ctx context.Context, p echoParams) (ToolResult, error) {
			return ToolResult{Content: "echoed:" + p.Value}, nil
		},
	})

	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{echo},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	_, result := drain(t, stream)
	if result.Err != nil {
		t.Fatalf("result.Err = %v, want nil", result.Err)
	}

	if got := provider.callCount(); got != 2 {
		t.Fatalf("provider called %d times, want 2 (tool-call turn must auto-continue, then stop)", got)
	}

	req, ok := provider.requestForCall(2)
	if !ok {
		t.Fatal("no second request recorded")
	}
	if !requestHasToolResult(req, "c1", "echoed:ping") {
		t.Errorf("second LLM request did not carry the tool_result for c1: %+v", req.Messages)
	}
}

// Fix B (v0.76 cancellation parity): a tool that ignores its cancelled ctx must
// not hang the prompt. On Stop the loop waits up to ShutdownTimeout for the
// in-flight batch, then unblocks — delivering the terminal result and emitting a
// ToolLeakEvent per still-running call. The leaked goroutine keeps running and
// must not corrupt the transcript or panic when it finally returns.
func TestToolLeakOnShutdownTimeout_v076(t *testing.T) {
	t.Parallel()

	toolStarted := make(chan struct{})
	release := make(chan struct{})
	toolReturned := make(chan struct{})
	var releaseOnce sync.Once
	releaseTool := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		releaseTool()
		<-toolReturned
	})

	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			events <- llm.ToolCallStartEvent{CallID: "leak-1", ToolName: "hang", Args: echoArgs("x")}
			events <- llm.ToolCallEndEvent{CallID: "leak-1"}
			events <- llm.MessageEndEvent{}
		},
	}

	var startOnce sync.Once
	hang := NewTool(Tool[echoParams]{
		Name:        "hang",
		Description: "ignores ctx",
		Execute: func(ctx context.Context, p echoParams) (ToolResult, error) {
			startOnce.Do(func() { close(toolStarted) })
			<-release // deliberately ignores ctx cancellation
			close(toolReturned)
			return ToolResult{Content: "late:" + p.Value}, nil
		},
	})

	a, err := NewAgent(AgentConfig{
		Providers:       []llm.LLMProvider{provider},
		DefaultModel:    "test/model",
		Tools:           []RegisteredTool{hang},
		ShutdownTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	var (
		mu        sync.Mutex
		leakEvent *ToolLeakEvent
	)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for ev := range stream.Events {
			if le, ok := ev.(ToolLeakEvent); ok {
				cp := le
				mu.Lock()
				leakEvent = &cp
				mu.Unlock()
			}
		}
	}()

	select {
	case <-toolStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("tool never started")
	}

	start := time.Now()
	a.Stop()

	var result PromptResult
	select {
	case result = <-stream.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not deliver after Stop — prompt hung on a ctx-ignoring tool")
	}
	elapsed := time.Since(start)
	<-drained

	if elapsed > 2*time.Second {
		t.Errorf("Done delivered after %v, want within the shutdown window (~50ms)", elapsed)
	}
	if !errors.Is(result.Err, ErrAgentStopped) {
		t.Errorf("result.Err = %v, want ErrAgentStopped", result.Err)
	}

	mu.Lock()
	le := leakEvent
	mu.Unlock()
	if le == nil {
		t.Fatal("no ToolLeakEvent emitted for the ctx-ignoring tool")
	}
	if le.ToolName != "hang" || le.CallID != "leak-1" {
		t.Errorf("ToolLeakEvent = {ToolName:%q CallID:%q}, want {hang leak-1}", le.ToolName, le.CallID)
	}

	for _, m := range result.Messages {
		if m.Type != "tool_result" {
			continue
		}
		if _, _, content, _, _, ok := m.ToolResult(); ok && strings.Contains(content, "late:") {
			t.Errorf("leaked tool result corrupted the transcript: %q", content)
		}
	}

	// Release the leaked tool and let it run its post-return code (results write,
	// guarded emit) so -race exercises the late-write guard.
	releaseTool()
	<-toolReturned
}

// Companion to Fix B: when the tool honors its cancelled ctx promptly, the loop
// unblocks immediately and emits no ToolLeakEvent.
func TestToolHonorsCtxEmitsNoLeak_v076(t *testing.T) {
	t.Parallel()

	toolStarted := make(chan struct{})
	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: "wait", Args: echoArgs("x")}
			events <- llm.ToolCallEndEvent{CallID: "c1"}
			events <- llm.MessageEndEvent{}
		},
	}

	var startOnce sync.Once
	wait := NewTool(Tool[echoParams]{
		Name:        "wait",
		Description: "honors ctx",
		Execute: func(ctx context.Context, _ echoParams) (ToolResult, error) {
			startOnce.Do(func() { close(toolStarted) })
			<-ctx.Done()
			return ToolResult{}, ctx.Err()
		},
	})

	a, err := NewAgent(AgentConfig{
		Providers:       []llm.LLMProvider{provider},
		DefaultModel:    "test/model",
		Tools:           []RegisteredTool{wait},
		ShutdownTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	var sawLeak atomic.Bool
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for ev := range stream.Events {
			if _, ok := ev.(ToolLeakEvent); ok {
				sawLeak.Store(true)
			}
		}
	}()

	select {
	case <-toolStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("tool never started")
	}

	start := time.Now()
	a.Stop()

	var result PromptResult
	select {
	case result = <-stream.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("Done did not deliver after Stop")
	}
	elapsed := time.Since(start)
	<-drained

	if elapsed > 2*time.Second {
		t.Errorf("Done delivered after %v; a ctx-honoring tool must not wait out ShutdownTimeout", elapsed)
	}
	if sawLeak.Load() {
		t.Error("ToolLeakEvent emitted though the tool honored ctx promptly")
	}
	if !errors.Is(result.Err, ErrAgentStopped) {
		t.Errorf("result.Err = %v, want ErrAgentStopped", result.Err)
	}
}

// Fix #1 (upstream 0.75.4): tool-call preflight stops preparing sibling tool
// calls once the prompt is aborted. A BeforeToolCall hook aborts the prompt on
// the first call; the second sibling's preflight must never run and no tool may
// execute after cancellation.
func TestPreflightStopsSiblingsAfterAbort_v076(t *testing.T) {
	t.Parallel()

	var (
		preflightCalls atomic.Int32
		executeCalls   atomic.Int32
		a              *Agent
	)

	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: "echo", Args: echoArgs("first")}
			events <- llm.ToolCallEndEvent{CallID: "c1"}
			events <- llm.ToolCallStartEvent{CallID: "c2", ToolName: "echo", Args: echoArgs("second")}
			events <- llm.ToolCallEndEvent{CallID: "c2"}
			events <- llm.MessageEndEvent{}
		},
	}

	echo := NewTool(Tool[echoParams]{
		Name:        "echo",
		Description: "echo",
		Execute: func(ctx context.Context, p echoParams) (ToolResult, error) {
			executeCalls.Add(1)
			return ToolResult{Content: p.Value}, nil
		},
	})

	var err error
	a, err = NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{echo},
		Hooks: Hooks{
			BeforeToolCall: func(ctx context.Context, c BeforeToolCallCtx) error {
				if preflightCalls.Add(1) == 1 {
					a.Stop()
				}
				return nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	_, result := drain(t, stream)

	if got := preflightCalls.Load(); got != 1 {
		t.Errorf("BeforeToolCall invoked %d times, want 1 (sibling preflight must stop after abort)", got)
	}
	if got := executeCalls.Load(); got != 0 {
		t.Errorf("tool executed %d times, want 0 (no work may proceed after cancellation)", got)
	}
	if !errors.Is(result.Err, ErrAgentStopped) {
		t.Errorf("result.Err = %v, want ErrAgentStopped", result.Err)
	}
}

// Fix #2 (upstream 0.58.4): steering waits until the current assistant message's
// tool-call batch fully finishes instead of skipping pending tool calls. A steer
// is queued before the batch runs; every tool must still execute, the steer must
// land after the tool results (the post-batch seam, never mid-batch), and it must
// be in context for the next LLM call.
func TestSteeringDefersUntilBatchFinishes_v076(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		executed []string
	)
	steerReady := make(chan struct{})

	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			if call == 1 {
				events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: "echo", Args: echoArgs("first")}
				events <- llm.ToolCallEndEvent{CallID: "c1"}
				events <- llm.ToolCallStartEvent{CallID: "c2", ToolName: "echo", Args: echoArgs("second")}
				events <- llm.ToolCallEndEvent{CallID: "c2"}
				<-steerReady
				events <- llm.MessageEndEvent{}
				return
			}
			events <- llm.TextDeltaEvent{Delta: "done"}
			events <- llm.MessageEndEvent{}
		},
	}

	echo := NewTool(Tool[echoParams]{
		Name:        "echo",
		Description: "echo",
		Execute: func(ctx context.Context, p echoParams) (ToolResult, error) {
			mu.Lock()
			executed = append(executed, p.Value)
			mu.Unlock()
			return ToolResult{Content: p.Value}, nil
		},
	})

	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{echo},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// Queue the steer while the batch is pending, then let turn 1 finish. The
	// steer is in the queue before the tool batch executes.
	if err := a.Steer(context.Background(), NewText("user", "interrupt")); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	close(steerReady)

	_, result := drain(t, stream)
	if result.Err != nil {
		t.Fatalf("result.Err = %v, want nil", result.Err)
	}

	mu.Lock()
	gotExecuted := append([]string(nil), executed...)
	mu.Unlock()
	if len(gotExecuted) != 2 {
		t.Errorf("executed tools = %v, want both to run (steering must not skip pending tool calls)", gotExecuted)
	}

	// The steer message lands after both tool results, never mid-batch.
	var lastToolResult, steerIdx = -1, -1
	for i, m := range result.Messages {
		if m.Type == "tool_result" {
			lastToolResult = i
		}
		if m.Type == "text" && m.Role == "user" && m.Text() == "interrupt" {
			steerIdx = i
		}
	}
	if steerIdx < 0 {
		t.Fatalf("steer message not found in transcript: %+v", result.Messages)
	}
	if lastToolResult < 0 || steerIdx < lastToolResult {
		t.Errorf("steer at idx %d must land after the last tool result at idx %d", steerIdx, lastToolResult)
	}

	if provider.callCount() != 2 {
		t.Errorf("provider called %d times, want 2 (steer must drive a follow-up LLM call)", provider.callCount())
	}
	if req, ok := provider.requestForCall(2); !ok || !requestMentions(req, "interrupt") {
		t.Errorf("second LLM call did not see the steered message in context")
	}
}

// Fix #3 (upstream 0.67.67): an AfterToolCall hook error yields an error tool
// result instead of aborting the batch; sibling tools still complete.
func TestAfterToolCallErrorYieldsErrorResult_v076(t *testing.T) {
	t.Parallel()

	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			if call == 1 {
				events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: "echo", Args: echoArgs("first")}
				events <- llm.ToolCallEndEvent{CallID: "c1"}
				events <- llm.ToolCallStartEvent{CallID: "c2", ToolName: "echo", Args: echoArgs("second")}
				events <- llm.ToolCallEndEvent{CallID: "c2"}
				events <- llm.MessageEndEvent{}
				return
			}
			events <- llm.TextDeltaEvent{Delta: "done"}
			events <- llm.MessageEndEvent{}
		},
	}

	var executed atomic.Int32
	echo := NewTool(Tool[echoParams]{
		Name:        "echo",
		Description: "echo",
		Execute: func(ctx context.Context, p echoParams) (ToolResult, error) {
			executed.Add(1)
			return ToolResult{Content: "ok:" + p.Value}, nil
		},
	})

	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{echo},
		Hooks: Hooks{
			AfterToolCall: func(ctx context.Context, c AfterToolCallCtx) error {
				if c.CallID == "c1" {
					return errors.New("after hook boom")
				}
				return nil
			},
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	_, result := drain(t, stream)
	if result.Err != nil {
		t.Fatalf("result.Err = %v, want nil (a hook error must not abort the prompt)", result.Err)
	}

	if got := executed.Load(); got != 2 {
		t.Errorf("tool executed %d times, want 2 (sibling must complete despite the hook error)", got)
	}

	got := toolResults(result.Messages)
	c1, ok := got["c1"]
	if !ok {
		t.Fatalf("no tool_result persisted for c1: %+v", result.Messages)
	}
	if !c1.isError || !strings.Contains(c1.content, "after hook boom") {
		t.Errorf("c1 result = {content:%q isError:%v}, want isError=true with the hook error text", c1.content, c1.isError)
	}
	c2, ok := got["c2"]
	if !ok {
		t.Fatalf("no tool_result persisted for c2: %+v", result.Messages)
	}
	if c2.isError {
		t.Errorf("c2 result = {content:%q isError:%v}, want a clean success (sibling unaffected)", c2.content, c2.isError)
	}
}

// Companion to fix #3: every per-call failure mode must persist as an error tool
// result carrying the error text, never a silent empty success. Covers the
// pre-existing unknown-tool and BeforeToolCall-rejection paths.
func TestPerCallErrorsPersistAsErrorResult_v076(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		toolName    string
		hooks       Hooks
		wantExecute int32
		wantSubstr  string
	}{
		{
			name:       "unknown tool",
			toolName:   "ghost",
			wantSubstr: "tool not found",
		},
		{
			name:     "before hook rejection",
			toolName: "echo",
			hooks: Hooks{
				BeforeToolCall: func(ctx context.Context, c BeforeToolCallCtx) error {
					return errors.New("before hook veto")
				},
			},
			wantSubstr: "before hook veto",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			provider := &loopProvider{
				emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
					if call == 1 {
						events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: tt.toolName, Args: echoArgs("x")}
						events <- llm.ToolCallEndEvent{CallID: "c1"}
						events <- llm.MessageEndEvent{}
						return
					}
					events <- llm.TextDeltaEvent{Delta: "done"}
					events <- llm.MessageEndEvent{}
				},
			}
			var executed atomic.Int32
			echo := NewTool(Tool[echoParams]{
				Name:        "echo",
				Description: "echo",
				Execute: func(ctx context.Context, p echoParams) (ToolResult, error) {
					executed.Add(1)
					return ToolResult{Content: "ok"}, nil
				},
			})
			a, err := NewAgent(AgentConfig{
				Providers:    []llm.LLMProvider{provider},
				DefaultModel: "test/model",
				Tools:        []RegisteredTool{echo},
				Hooks:        tt.hooks,
			})
			if err != nil {
				t.Fatalf("NewAgent: %v", err)
			}
			stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
			if err != nil {
				t.Fatalf("Prompt: %v", err)
			}
			_, result := drain(t, stream)
			if result.Err != nil {
				t.Fatalf("result.Err = %v, want nil", result.Err)
			}

			if got := executed.Load(); got != tt.wantExecute {
				t.Errorf("tool executed %d times, want %d", got, tt.wantExecute)
			}
			got := toolResults(result.Messages)
			c1, ok := got["c1"]
			if !ok {
				t.Fatalf("no tool_result persisted for c1: %+v", result.Messages)
			}
			if !c1.isError || !strings.Contains(c1.content, tt.wantSubstr) {
				t.Errorf("c1 result = {content:%q isError:%v}, want isError=true containing %q", c1.content, c1.isError, tt.wantSubstr)
			}
		})
	}
}

// Fix #4 (upstream 0.68.1): parallel tool execution emits tool-end events as each
// tool finalizes (completion order) while persisting tool-result messages in
// assistant source order. tool-1 blocks until tool-2's end event is observed, so
// completion order is deterministically [c2, c1] while source order stays [c1, c2].
func TestParallelToolEndCompletionOrderResultsSourceOrder_v076(t *testing.T) {
	t.Parallel()

	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			if call == 1 {
				events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: "echo", Args: echoArgs("first")}
				events <- llm.ToolCallEndEvent{CallID: "c1"}
				events <- llm.ToolCallStartEvent{CallID: "c2", ToolName: "echo", Args: echoArgs("second")}
				events <- llm.ToolCallEndEvent{CallID: "c2"}
				events <- llm.MessageEndEvent{}
				return
			}
			events <- llm.TextDeltaEvent{Delta: "done"}
			events <- llm.MessageEndEvent{}
		},
	}

	releaseFirst := make(chan struct{})
	var parallelObserved atomic.Bool
	var secondDone atomic.Bool
	echo := NewTool(Tool[echoParams]{
		Name:        "echo",
		Description: "echo",
		Execute: func(ctx context.Context, p echoParams) (ToolResult, error) {
			switch p.Value {
			case "first":
				<-releaseFirst
			case "second":
				if !secondDone.Load() {
					parallelObserved.Store(true)
				}
				secondDone.Store(true)
			}
			return ToolResult{Content: p.Value}, nil
		},
	})

	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{echo},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// Read events directly so we can release tool-1 the instant tool-2 finalizes,
	// pinning completion order to [c2, c1].
	var toolEndOrder []string
	var releaseOnce sync.Once
	collected := make(chan struct{})
	go func() {
		defer close(collected)
		for ev := range stream.Events {
			// runOneTurn relays the provider's llm.ToolCallEndEvent with an empty
			// ToolName; the execution-end events fix #4 governs carry ToolName and
			// Result. Filter to the latter.
			if te, ok := ev.(ToolCallEndEvent); ok && te.ToolName != "" {
				toolEndOrder = append(toolEndOrder, te.CallID)
				if te.CallID == "c2" {
					releaseOnce.Do(func() { close(releaseFirst) })
				}
			}
		}
	}()

	var result PromptResult
	select {
	case result = <-stream.Done:
	case <-time.After(10 * time.Second):
		releaseOnce.Do(func() { close(releaseFirst) })
		t.Fatal("timeout waiting for Done")
	}
	<-collected
	if result.Err != nil {
		t.Fatalf("result.Err = %v, want nil", result.Err)
	}

	if !parallelObserved.Load() {
		t.Error("tools did not run in parallel")
	}

	wantEnd := []string{"c2", "c1"}
	if len(toolEndOrder) != 2 || toolEndOrder[0] != wantEnd[0] || toolEndOrder[1] != wantEnd[1] {
		t.Errorf("tool-end order = %v, want %v (completion order)", toolEndOrder, wantEnd)
	}

	var resultOrder []string
	for _, m := range result.Messages {
		if m.Type != "tool_result" {
			continue
		}
		callID, _, _, _, _, _ := m.ToolResult()
		resultOrder = append(resultOrder, callID)
	}
	wantResults := []string{"c1", "c2"}
	if len(resultOrder) != 2 || resultOrder[0] != wantResults[0] || resultOrder[1] != wantResults[1] {
		t.Errorf("tool-result order = %v, want %v (assistant source order)", resultOrder, wantResults)
	}
}

// Fix #5 (upstream 0.32.0): Prompt rejects with ErrAgentBusy while a prompt is
// already streaming. The guard exists from AGENT-1; this hermetic fixture pins it.
func TestPromptRejectsWhenAlreadyStreaming_v076(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	provider := &stubProvider{
		name: "test",
		emit: func(events chan<- llm.LLMEvent) {
			<-release
			events <- llm.TextDeltaEvent{Delta: "ok"}
			events <- llm.MessageEndEvent{}
		},
	}
	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	stream, err := a.Prompt(context.Background(), NewText("user", "first"), PromptOpts{})
	if err != nil {
		t.Fatalf("first Prompt: %v", err)
	}

	_, err = a.Prompt(context.Background(), NewText("user", "second"), PromptOpts{})
	if !errors.Is(err, ErrAgentBusy) {
		t.Fatalf("second Prompt while streaming = %v, want ErrAgentBusy", err)
	}

	close(release)
	_, result := drain(t, stream)
	if result.Err != nil {
		t.Fatalf("first prompt result.Err = %v, want nil", result.Err)
	}
}
