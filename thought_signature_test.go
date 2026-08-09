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
				events <- llm.ToolCallEndEvent{CallID: "c1", ToolName: "echo", Args: echoArgs("ping"), ThoughtSignature: sig}
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

func TestTextMessageCarriesThoughtSignature(t *testing.T) {
	t.Parallel()
	// given a text message persisted with its provider thought signature
	sig := []byte("opaque-signature-bytes")
	msg := NewTextWithSignature("assistant", "hello", sig)

	// then the text and signature both round-trip through the transcript body
	if got := msg.Text(); got != "hello" {
		t.Errorf("Text() = %q, want %q", got, "hello")
	}
	if got := msg.TextThoughtSignature(); !bytes.Equal(got, sig) {
		t.Errorf("TextThoughtSignature() = %q, want %q", got, sig)
	}
}

func TestTextMessageWithoutSignatureKeepsStringBody(t *testing.T) {
	t.Parallel()
	// given a text message authored without a signature
	msg := NewTextWithSignature("assistant", "hello", nil)

	// then the body keeps the classic plain-string shape (no format drift for
	// the common case) and no signature is fabricated
	if string(msg.Body) != `"hello"` {
		t.Errorf("Body = %s, want the plain-string shape %s", msg.Body, `"hello"`)
	}
	if got := NewText("assistant", "hello").TextThoughtSignature(); got != nil {
		t.Errorf("TextThoughtSignature() = %q, want nil", got)
	}
}

func TestThinkingMessageCarriesThoughtSignature(t *testing.T) {
	t.Parallel()
	// given a thinking message persisted with its provider thought signature
	sig := []byte("think-sig")
	msg := NewThinkingWithSignature("assistant", "let me reason", sig)

	// then text and signature both round-trip
	if got := msg.ThinkingText(); got != "let me reason" {
		t.Errorf("ThinkingText() = %q, want %q", got, "let me reason")
	}
	if got := msg.ThinkingThoughtSignature(); !bytes.Equal(got, sig) {
		t.Errorf("ThinkingThoughtSignature() = %q, want %q", got, sig)
	}

	// and a classic thinking message still reads its text (previously lost:
	// Text() rejects the thinking type) while yielding no signature
	classic := NewThinking("assistant", "reasoning")
	if got := classic.ThinkingText(); got != "reasoning" {
		t.Errorf("ThinkingText() on classic body = %q, want %q", got, "reasoning")
	}
	if got := classic.ThinkingThoughtSignature(); got != nil {
		t.Errorf("ThinkingThoughtSignature() = %q, want nil", got)
	}
}

func TestDefaultConvertToLLMCarriesTextAndThinkingSignatures(t *testing.T) {
	t.Parallel()
	// given a transcript whose text and thinking messages carry signatures
	textSig := []byte("text-sig")
	thinkSig := []byte("think-sig")
	msgs := []Message{
		NewTextWithSignature("assistant", "answer", textSig),
		NewThinkingWithSignature("assistant", "reasoning", thinkSig),
	}

	// when the transcript is converted for the LLM
	out := DefaultConvertToLLM(msgs)

	// then both contents replay text and signature verbatim
	var sawText, sawThinking bool
	for _, m := range out {
		switch c := m.Content.(type) {
		case llm.TextContent:
			sawText = true
			if c.Text != "answer" || !bytes.Equal(c.ThoughtSignature, textSig) {
				t.Errorf("TextContent = {%q, %q}, want {%q, %q}", c.Text, c.ThoughtSignature, "answer", textSig)
			}
		case llm.ThinkingContent:
			sawThinking = true
			if c.Text != "reasoning" || !bytes.Equal(c.ThoughtSignature, thinkSig) {
				t.Errorf("ThinkingContent = {%q, %q}, want {%q, %q}", c.Text, c.ThoughtSignature, "reasoning", thinkSig)
			}
		}
	}
	if !sawText || !sawThinking {
		t.Fatalf("converted messages missing content: sawText=%v sawThinking=%v", sawText, sawThinking)
	}
}

// The text half of the Gemini replay contract (upstream #7362): a thought
// signature received on text deltas must persist onto the transcript's text
// message and come back verbatim on the next prompt's replayed history.
func TestTextThoughtSignatureRoundTripsAcrossPrompts(t *testing.T) {
	t.Parallel()
	sig := []byte("opaque-signature-bytes")

	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			if call == 1 {
				// signature arrives on the FIRST delta; later deltas omit it
				events <- llm.TextDeltaEvent{Delta: "hello", ThoughtSignature: sig}
				events <- llm.TextDeltaEvent{Delta: " world"}
				events <- llm.MessageEndEvent{}
				return
			}
			events <- llm.TextDeltaEvent{Delta: "done"}
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

	// when a first prompt produces signed text and a queued follow-up replays it
	stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	// the follow-up drives a second LLM call in the same session, so its
	// request replays the signed history
	if err := a.FollowUp(context.Background(), NewText("user", "again")); err != nil {
		t.Fatalf("FollowUp: %v", err)
	}
	events, result := drain(t, stream)
	if result.Err != nil {
		t.Fatalf("result.Err = %v, want nil", result.Err)
	}

	// then the forwarded agent event carries the signature for durable-log
	// consumers (harness), and the transcript persists it
	var sawEventSig bool
	for _, ev := range events {
		if td, ok := ev.(TextDeltaEvent); ok && len(td.ThoughtSignature) > 0 {
			sawEventSig = true
			if !bytes.Equal(td.ThoughtSignature, sig) {
				t.Errorf("agent TextDeltaEvent.ThoughtSignature = %q, want %q", td.ThoughtSignature, sig)
			}
		}
	}
	if !sawEventSig {
		t.Error("no agent TextDeltaEvent carried the thought signature")
	}

	var persisted bool
	for _, m := range result.Messages {
		if m.Type == "text" && m.Role == "assistant" && m.Text() == "hello world" {
			persisted = true
			if got := m.TextThoughtSignature(); !bytes.Equal(got, sig) {
				t.Errorf("transcript text ThoughtSignature = %q, want %q", got, sig)
			}
		}
	}
	if !persisted {
		t.Fatal("no signed assistant text message in transcript")
	}

	// and the follow-up call's request replays the signed text verbatim
	req, ok := provider.requestForCall(2)
	if !ok {
		t.Fatal("no second request recorded (queued follow-up must drive another call)")
	}
	var replayed bool
	for _, m := range req.Messages {
		if tc, ok := m.Content.(llm.TextContent); ok && tc.Text == "hello world" {
			replayed = true
			if !bytes.Equal(tc.ThoughtSignature, sig) {
				t.Errorf("replayed TextContent.ThoughtSignature = %q, want %q", tc.ThoughtSignature, sig)
			}
		}
	}
	if !replayed {
		t.Fatal("second LLM request has no TextContent for the signed text")
	}
}

// Gemini can attach the signature to a part whose visible text is empty; the
// signed empty block must persist and replay or the reasoning chain breaks
// (upstream #7362).
func TestSignedEmptyAssistantTextPersistsAndReplays(t *testing.T) {
	t.Parallel()
	sig := []byte("empty-part-sig")

	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			if call == 1 {
				// a signature-only delta: no visible text at all
				events <- llm.TextDeltaEvent{Delta: "", ThoughtSignature: sig}
				events <- llm.MessageEndEvent{}
				return
			}
			events <- llm.TextDeltaEvent{Delta: "done"}
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

	stream, err := a.Prompt(context.Background(), NewText("user", "go"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	// a follow-up drives the second LLM call in the same session
	if err := a.FollowUp(context.Background(), NewText("user", "again")); err != nil {
		t.Fatalf("FollowUp: %v", err)
	}
	_, result := drain(t, stream)
	if result.Err != nil {
		t.Fatalf("result.Err = %v, want nil", result.Err)
	}

	// then the signed empty text block persisted despite having no visible text
	var persisted bool
	for _, m := range result.Messages {
		if m.Type == "text" && m.Role == "assistant" && m.Text() == "" {
			persisted = true
			if got := m.TextThoughtSignature(); !bytes.Equal(got, sig) {
				t.Errorf("empty text ThoughtSignature = %q, want %q", got, sig)
			}
		}
	}
	if !persisted {
		t.Fatal("signed empty assistant text was not persisted to the transcript")
	}

	// and it replays on the follow-up call's request
	req, ok := provider.requestForCall(2)
	if !ok {
		t.Fatal("no second request recorded (queued follow-up must drive another call)")
	}
	var replayed bool
	for _, m := range req.Messages {
		if tc, ok := m.Content.(llm.TextContent); ok && m.Role == "assistant" && tc.Text == "" && bytes.Equal(tc.ThoughtSignature, sig) {
			replayed = true
		}
	}
	if !replayed {
		t.Fatal("second LLM request did not replay the signed empty text block")
	}
}

// The harness consumes agent events (not the transcript) to author durable
// records, so the agent-level ToolCallStartEvent must carry the provider
// thought signature too (HARNESS-11 contingency).
func TestToolCallStartEventCarriesThoughtSignature(t *testing.T) {
	t.Parallel()
	sig := []byte("opaque-signature-bytes")

	provider := &loopProvider{
		emit: func(call int, _ llm.LLMRequest, events chan<- llm.LLMEvent) {
			if call == 1 {
				events <- llm.ToolCallStartEvent{CallID: "c1", ToolName: "echo", Args: echoArgs("ping"), ThoughtSignature: sig}
				events <- llm.ToolCallEndEvent{CallID: "c1", ToolName: "echo", Args: echoArgs("ping"), ThoughtSignature: sig}
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
	events, result := drain(t, stream)
	if result.Err != nil {
		t.Fatalf("result.Err = %v, want nil", result.Err)
	}

	var seen bool
	for _, ev := range events {
		if tc, ok := ev.(ToolCallStartEvent); ok {
			seen = true
			if !bytes.Equal(tc.ThoughtSignature, sig) {
				t.Errorf("ToolCallStartEvent.ThoughtSignature = %q, want %q", tc.ThoughtSignature, sig)
			}
		}
	}
	if !seen {
		t.Fatal("no ToolCallStartEvent in agent event stream")
	}
}
