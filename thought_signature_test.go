package pi

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/dev-resolute/resolute-llm-go"
)

func TestToolCallMessageCarriesThoughtSignature(t *testing.T) {
	t.Parallel()
	// given a tool call persisted with its provider thought signature
	sig := []byte("opaque-signature-bytes")
	msg := NewToolCallWithSignature("assistant", "c1", "echo", echoArgs("ping"), sig)

	// then the classic accessor still extracts the call fields
	callID, toolName, args, ok := msg.ToolCall()
	if !ok || callID != "c1" || toolName != "echo" {
		t.Fatalf("ToolCall() = (%q, %q, %s, %v), want (c1, echo, args, true)", callID, toolName, args, ok)
	}

	// and the signature round-trips through the transcript body
	if got := msg.ToolCallThoughtSignature(); !bytes.Equal(got, sig) {
		t.Errorf("ToolCallThoughtSignature() = %q, want %q", got, sig)
	}
}

func TestDefaultConvertToLLMCarriesThoughtSignature(t *testing.T) {
	t.Parallel()
	// given a transcript whose tool call carries a thought signature
	sig := []byte("opaque-signature-bytes")
	msgs := []Message{
		NewText("user", "go"),
		NewToolCallWithSignature("assistant", "c1", "echo", echoArgs("ping"), sig),
	}

	// when the transcript is converted for the LLM
	out := DefaultConvertToLLM(msgs)

	// then the replayed ToolCallContent carries the signature verbatim
	var found bool
	for _, m := range out {
		if tc, ok := m.Content.(llm.ToolCallContent); ok {
			found = true
			if !bytes.Equal(tc.ThoughtSignature, sig) {
				t.Errorf("ToolCallContent.ThoughtSignature = %q, want %q", tc.ThoughtSignature, sig)
			}
		}
	}
	if !found {
		t.Fatal("no ToolCallContent in converted messages")
	}
}

// The Gemini 3 contract: a thought signature received on a tool call must come
// back verbatim on that tool call in the auto-continued turn, or the provider
// rejects the whole request (400 INVALID_ARGUMENT).
func TestToolCallThoughtSignatureRoundTripsThroughLoop(t *testing.T) {
	t.Parallel()
	sig := []byte("opaque-signature-bytes")

	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			if call == 1 {
				events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: "echo", Args: echoArgs("ping"), ThoughtSignature: sig}
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

	// when a tool-call turn auto-continues
	stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	_, result := drain(t, stream)
	if result.Err != nil {
		t.Fatalf("result.Err = %v, want nil", result.Err)
	}

	// then the transcript's tool_call message persisted the signature
	var persisted bool
	for _, m := range result.Messages {
		if m.Type == "tool_call" {
			persisted = true
			if got := m.ToolCallThoughtSignature(); !bytes.Equal(got, sig) {
				t.Errorf("transcript tool_call ThoughtSignature = %q, want %q", got, sig)
			}
		}
	}
	if !persisted {
		t.Fatal("no tool_call message in transcript")
	}

	// and the second LLM request replays the tool call with the signature
	req, ok := provider.requestForCall(2)
	if !ok {
		t.Fatal("no second request recorded (tool-call turn must auto-continue)")
	}
	var replayed bool
	for _, m := range req.Messages {
		if tc, ok := m.Content.(llm.ToolCallContent); ok && tc.CallID == "c1" {
			replayed = true
			if !bytes.Equal(tc.ThoughtSignature, sig) {
				t.Errorf("replayed ToolCallContent.ThoughtSignature = %q, want %q", tc.ThoughtSignature, sig)
			}
		}
	}
	if !replayed {
		t.Fatal("second LLM request has no ToolCallContent for c1")
	}
}

// Live gate: the full agent tool loop on a Gemini 3 model. Without the
// thought-signature round trip the auto-continued turn is rejected with
// 400 INVALID_ARGUMENT ("Function call is missing a thought_signature").
func TestLiveGemini3AgentToolLoop(t *testing.T) {
	// given a live Gemini 3 agent with a weather tool
	type weatherParams struct {
		City string `json:"city"`
	}
	weather := NewTool(Tool[weatherParams]{
		Name:        "get_weather",
		Description: "Get the current weather for a city.",
		Execute: func(ctx context.Context, p weatherParams) (ToolResult, error) {
			return ToolResult{Content: `{"temperature_c": 22, "condition": "sunny"}`}, nil
		},
	})
	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{liveProvider(t)},
		DefaultModel: "gemini/gemini-3.1-pro-preview",
		Tools:        []RegisteredTool{weather},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	// when one prompt spans the tool-call turn and its auto-continuation
	stream, err := a.Prompt(context.Background(), NewText("user", "What is the weather in Paris right now? Use the get_weather tool."), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	_, result := drain(t, stream)
	if result.Err != nil {
		if strings.Contains(result.Err.Error(), "NOT_FOUND") || strings.Contains(result.Err.Error(), "PERMISSION_DENIED") {
			t.Skipf("gemini-3.1-pro-preview not available to this key: %v", result.Err)
		}
		t.Fatalf("prompt failed (thought signature not round-tripped?): %v", result.Err)
	}

	// then the transcript tool call carries a real signature
	var sawToolCall bool
	for _, m := range result.Messages {
		if m.Type == "tool_call" {
			sawToolCall = true
			if len(m.ToolCallThoughtSignature()) == 0 {
				t.Error("live Gemini 3 tool_call persisted without a thought signature")
			}
		}
	}
	if !sawToolCall {
		t.Fatal("agent made no tool call")
	}

	// and the final answer uses the tool result
	final := result.Messages[len(result.Messages)-1]
	if final.Type != "text" || !strings.Contains(final.Text(), "22") {
		t.Errorf("final message does not use the tool result (want mention of 22): type=%s text=%q", final.Type, final.Text())
	}
}

func TestToolCallMessageWithoutSignatureYieldsNil(t *testing.T) {
	t.Parallel()
	// given a tool call persisted without a signature (pre-existing transcripts)
	msg := NewToolCall("assistant", "c1", "echo", echoArgs("ping"))

	// then no signature is fabricated
	if got := msg.ToolCallThoughtSignature(); len(got) != 0 {
		t.Errorf("ToolCallThoughtSignature() = %q, want empty", got)
	}

	// and non-tool_call messages yield nil
	if got := NewText("assistant", "hi").ToolCallThoughtSignature(); got != nil {
		t.Errorf("ToolCallThoughtSignature() on text = %q, want nil", got)
	}
}
