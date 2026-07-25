package pi

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/dev-resolute/resolute-llm-go"
)

func TestNewToolRequiresExactlyOneExecute(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewTool with both Execute and ExecuteStream must panic")
		}
	}()
	NewTool(Tool[struct{}]{
		Name:          "bad",
		Execute:       func(context.Context, struct{}) (ToolResult, error) { return ToolResult{}, nil },
		ExecuteStream: func(context.Context, struct{}, func(ToolResult)) (ToolResult, error) { return ToolResult{}, nil },
	})
}

func TestStreamingToolEmitsThroughCapability(t *testing.T) {
	rt := NewTool(Tool[struct{}]{
		Name: "streamer",
		ExecuteStream: func(ctx context.Context, _ struct{}, emit func(ToolResult)) (ToolResult, error) {
			emit(ToolResult{Content: "partial 1"})
			emit(ToolResult{Content: "partial 2"})
			return ToolResult{Content: "final"}, nil
		},
	})
	st, ok := rt.(streamingTool)
	if !ok {
		t.Fatal("registered streaming tool does not implement streamingTool")
	}
	var got []string
	res, err := st.ExecuteStream(context.Background(), "c1", json.RawMessage(`{}`), func(r ToolResult) {
		got = append(got, r.Content)
	})
	if err != nil || res.Content != "final" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(got) != 2 || got[0] != "partial 1" {
		t.Errorf("emitted = %v", got)
	}
}

func TestNonStreamingToolLacksCapability(t *testing.T) {
	rt := NewTool(Tool[struct{}]{
		Name:    "plain",
		Execute: func(context.Context, struct{}) (ToolResult, error) { return ToolResult{}, nil },
	})
	if _, ok := rt.(streamingTool); ok {
		t.Fatal("plain tool must not implement streamingTool")
	}
}

// TestStreamOnlyToolSupportsPlainExecute pins the fix for the nil-pointer
// landmine: a stream-only tool's embedded typedTool[P].execute field is nil,
// so a caller that reaches RegisteredTool.Execute directly — without first
// checking the streamingTool capability, e.g. a harness that only knows the
// public interface — must not panic. Execute runs the ExecuteStream path
// with a no-op emit, so partial updates are silently discarded.
func TestStreamOnlyToolSupportsPlainExecute(t *testing.T) {
	rt := NewTool(Tool[struct{}]{
		Name: "streamer",
		ExecuteStream: func(ctx context.Context, _ struct{}, emit func(ToolResult)) (ToolResult, error) {
			emit(ToolResult{Content: "partial 1"})
			emit(ToolResult{Content: "partial 2"})
			return ToolResult{Content: "final"}, nil
		},
	})

	res, err := rt.Execute(context.Background(), "c1", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute returned err = %v, want nil", err)
	}
	if res.Content != "final" {
		t.Errorf("Execute result.Content = %q, want %q", res.Content, "final")
	}
}

// TestNewToolRequiresAtLeastOneExecute pins the other half of the "exactly
// one" contract: neither Execute nor ExecuteStream set must also panic.
func TestNewToolRequiresAtLeastOneExecute(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewTool with neither Execute nor ExecuteStream must panic")
		}
	}()
	NewTool(Tool[struct{}]{Name: "empty"})
}

// TestStreamingToolLoopEmitsToolUpdateEvents drives a full prompt through the
// hermetic loopProvider (see prompt_loop_v076_test.go) with a streaming tool,
// asserting ToolUpdateEvents land on the EventStream between ToolCallStartEvent
// and ToolCallEndEvent, and are never persisted to the transcript.
func TestStreamingToolLoopEmitsToolUpdateEvents(t *testing.T) {
	t.Parallel()

	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			if call == 1 {
				events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: "streamer", Args: echoArgs("go")}
				events <- llm.ToolCallEndEvent{CallID: "c1"}
				events <- llm.MessageEndEvent{}
				return
			}
			events <- llm.TextDeltaEvent{Delta: "done"}
			events <- llm.MessageEndEvent{}
		},
	}

	streamer := NewTool(Tool[echoParams]{
		Name:        "streamer",
		Description: "streams partial results",
		ExecuteStream: func(ctx context.Context, p echoParams, emit func(ToolResult)) (ToolResult, error) {
			emit(ToolResult{Content: "partial 1"})
			emit(ToolResult{Content: "partial 2"})
			return ToolResult{Content: "final"}, nil
		},
	})

	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{streamer},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	var order []string
	var updates []ToolUpdateEvent
	for ev := range stream.Events {
		switch e := ev.(type) {
		case ToolCallStartEvent:
			order = append(order, "start:"+e.CallID)
		case ToolUpdateEvent:
			order = append(order, "update:"+e.CallID)
			updates = append(updates, e)
		case ToolCallEndEvent:
			order = append(order, "end:"+e.CallID)
		}
	}

	var result PromptResult
	select {
	case result = <-stream.Done:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for Done")
	}
	if result.Err != nil {
		t.Fatalf("result.Err = %v, want nil", result.Err)
	}

	wantOrder := []string{"start:c1", "update:c1", "update:c1", "end:c1"}
	if len(order) != len(wantOrder) {
		t.Fatalf("event order = %v, want %v", order, wantOrder)
	}
	for i, w := range wantOrder {
		if order[i] != w {
			t.Fatalf("event order = %v, want %v", order, wantOrder)
		}
	}

	if len(updates) != 2 {
		t.Fatalf("got %d ToolUpdateEvents, want 2", len(updates))
	}
	if updates[0].Name != "streamer" || updates[0].Result.Content != "partial 1" {
		t.Errorf("updates[0] = %+v", updates[0])
	}
	if updates[1].Name != "streamer" || updates[1].Result.Content != "partial 2" {
		t.Errorf("updates[1] = %+v", updates[1])
	}

	// Ephemeral: partial results must never land in the persisted transcript.
	for _, m := range result.Messages {
		if m.Type != "tool_result" {
			continue
		}
		_, _, content, _, _, ok := m.ToolResult()
		if !ok {
			continue
		}
		if content == "partial 1" || content == "partial 2" {
			t.Errorf("partial result %q leaked into persisted transcript", content)
		}
		if content != "final" {
			t.Errorf("persisted tool result content = %q, want %q", content, "final")
		}
	}
}

// TestStreamingToolLoopConcurrentEmitRace exercises two concurrent streaming
// tools emitting simultaneously under -race, pinning that r.emit's shared
// state (guarded by promptRun.emitMu) tolerates concurrent senders from
// multiple tool goroutines.
func TestStreamingToolLoopConcurrentEmitRace(t *testing.T) {
	t.Parallel()

	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			if call == 1 {
				events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: "streamer", Args: echoArgs("one")}
				events <- llm.ToolCallEndEvent{CallID: "c1"}
				events <- llm.ToolCallStartEvent{CallID: "c2", ToolName: "streamer", Args: echoArgs("two")}
				events <- llm.ToolCallEndEvent{CallID: "c2"}
				events <- llm.MessageEndEvent{}
				return
			}
			events <- llm.TextDeltaEvent{Delta: "done"}
			events <- llm.MessageEndEvent{}
		},
	}

	var wg sync.WaitGroup
	wg.Add(2)
	streamer := NewTool(Tool[echoParams]{
		Name:        "streamer",
		Description: "streams partial results concurrently",
		ExecuteStream: func(ctx context.Context, p echoParams, emit func(ToolResult)) (ToolResult, error) {
			wg.Done()
			wg.Wait() // barrier: force both calls' emit goroutines to overlap
			for i := 0; i < 5; i++ {
				emit(ToolResult{Content: p.Value})
			}
			return ToolResult{Content: p.Value + "-final"}, nil
		},
	})

	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{streamer},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	updateCount := 0
	for ev := range stream.Events {
		if _, ok := ev.(ToolUpdateEvent); ok {
			updateCount++
		}
	}

	var result PromptResult
	select {
	case result = <-stream.Done:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for Done")
	}
	if result.Err != nil {
		t.Fatalf("result.Err = %v, want nil", result.Err)
	}
	if updateCount != 10 {
		t.Errorf("got %d ToolUpdateEvents, want 10", updateCount)
	}
}
