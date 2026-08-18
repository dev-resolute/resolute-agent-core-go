// Package pi provides a stateful agent loop built on pi-llm-go.
package resolute

import (
	"encoding/json"

	"github.com/dev-resolute/resolute-llm-go"
)

// Message is the agent-side unit of transcript content.
// The framework treats Body as opaque bytes for user-defined custom message types.
type Message struct {
	Role string
	Type string
	Body json.RawMessage
	// Images carries attachments outside Body so Body stays text-only and token estimation can count images at a flat rate.
	Images []llm.ImageContent `json:",omitempty"`
}

// NewText creates a text message.
func NewText(role, text string) Message {
	body, _ := json.Marshal(text)
	return Message{Role: role, Type: "text", Body: body}
}

// NewTextWithSignature creates a text message carrying the provider's opaque
// thought signature (Gemini). With an empty signature it is exactly NewText,
// so transcripts without signatures keep the plain-string body shape.
func NewTextWithSignature(role, text string, thoughtSignature []byte) Message {
	if len(thoughtSignature) == 0 {
		return NewText(role, text)
	}
	body, _ := json.Marshal(map[string]any{
		"text":              text,
		"thought_signature": thoughtSignature,
	})
	return Message{Role: role, Type: "text", Body: body}
}

// NewToolCall creates a tool call message.
func NewToolCall(role string, callID, toolName string, args json.RawMessage) Message {
	body, _ := json.Marshal(map[string]any{
		"call_id":   callID,
		"tool_name": toolName,
		"args":      args,
	})
	return Message{Role: role, Type: "tool_call", Body: body}
}

// NewToolCallWithSignature creates a tool call message that also persists the
// provider's opaque thought signature (Gemini 3), so replaying the transcript
// carries it back verbatim. A nil signature is equivalent to NewToolCall.
func NewToolCallWithSignature(role string, callID, toolName string, args json.RawMessage, thoughtSignature []byte) Message {
	if len(thoughtSignature) == 0 {
		return NewToolCall(role, callID, toolName, args)
	}
	body, _ := json.Marshal(map[string]any{
		"call_id":           callID,
		"tool_name":         toolName,
		"args":              args,
		"thought_signature": thoughtSignature,
	})
	return Message{Role: role, Type: "tool_call", Body: body}
}

// NewToolResult creates a tool result message.
func NewToolResult(role string, callID, toolName, content string, data json.RawMessage, isError bool) Message {
	body, _ := json.Marshal(map[string]any{
		"call_id":   callID,
		"tool_name": toolName,
		"content":   content,
		"data":      data,
		"is_error":  isError,
	})
	return Message{Role: role, Type: "tool_result", Body: body}
}

// NewToolResultMsg is the ToolResult-aware variant of NewToolResult: it takes
// the tool's ToolResult directly and copies result.Images to Message.Images
// (images ride the Message, never the JSON Body). Tool results are always
// authored with role "tool".
func NewToolResultMsg(callID, toolName string, result ToolResult) Message {
	body, _ := json.Marshal(map[string]any{
		"call_id":   callID,
		"tool_name": toolName,
		"content":   result.Content,
		"data":      result.Data,
		"is_error":  result.IsError,
	})
	return Message{Role: "tool", Type: "tool_result", Body: body, Images: result.Images}
}

// NewThinking creates a thinking message.
func NewThinking(role, text string) Message {
	body, _ := json.Marshal(text)
	return Message{Role: role, Type: "thinking", Body: body}
}

// NewThinkingWithSignature creates a thinking message carrying the provider's
// opaque thought signature (Gemini). With an empty signature it is exactly
// NewThinking, so transcripts without signatures keep the plain-string body
// shape.
func NewThinkingWithSignature(role, text string, thoughtSignature []byte) Message {
	if len(thoughtSignature) == 0 {
		return NewThinking(role, text)
	}
	body, _ := json.Marshal(map[string]any{
		"text":              text,
		"thought_signature": thoughtSignature,
	})
	return Message{Role: role, Type: "thinking", Body: body}
}

// NewSystem creates a system prompt message.
func NewSystem(text string) Message {
	return NewText("system", text)
}

// NewBranchSummaryMessage creates a branch_summary message.
func NewBranchSummaryMessage(summary string) Message {
	body, _ := json.Marshal(summary)
	return Message{Role: "system", Type: "branch_summary", Body: body}
}

// NewActiveToolsChange creates an active_tools_change bookkeeping entry recording
// the set of active tool names (nil means all registered tools are active). It is
// never sent to the model — DefaultConvertToLLM and BuildLLMContext both exclude
// it — and is never chosen as a compaction cut point. On resume, the active set
// is restored by scanning the transcript for the last such entry.
func NewActiveToolsChange(names []string) Message {
	body, _ := json.Marshal(map[string][]string{"activeToolNames": names})
	return Message{Role: "system", Type: "active_tools_change", Body: body}
}

// ActiveToolNames extracts the recorded active tool names from an
// active_tools_change message. The second return is false for other types.
func (m Message) ActiveToolNames() (names []string, ok bool) {
	if m.Type != "active_tools_change" {
		return nil, false
	}
	var v struct {
		ActiveToolNames []string `json:"activeToolNames"`
	}
	if err := json.Unmarshal(m.Body, &v); err != nil {
		return nil, false
	}
	return v.ActiveToolNames, true
}

// Text extracts the text from a text-typed or branch_summary message.
// Signature-carrying bodies (NewTextWithSignature) store an object; both
// shapes read back.
func (m Message) Text() string {
	if m.Type != "text" && m.Type != "branch_summary" {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Body, &s); err == nil {
		return s
	}
	var v struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(m.Body, &v)
	return v.Text
}

// TextThoughtSignature extracts the provider's opaque thought signature from a
// text message. Nil when absent (pre-existing transcripts, providers without
// signatures) or when the message is not a text.
func (m Message) TextThoughtSignature() []byte {
	if m.Type != "text" {
		return nil
	}
	var v struct {
		ThoughtSignature []byte `json:"thought_signature"`
	}
	if err := json.Unmarshal(m.Body, &v); err != nil {
		return nil
	}
	return v.ThoughtSignature
}

// ThinkingText extracts the text from a thinking message. Signature-carrying
// bodies (NewThinkingWithSignature) store an object; both shapes read back.
func (m Message) ThinkingText() string {
	if m.Type != "thinking" {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Body, &s); err == nil {
		return s
	}
	var v struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(m.Body, &v)
	return v.Text
}

// ThinkingThoughtSignature extracts the provider's opaque thought signature
// from a thinking message. Nil when absent or when the message is not a
// thinking.
func (m Message) ThinkingThoughtSignature() []byte {
	if m.Type != "thinking" {
		return nil
	}
	var v struct {
		ThoughtSignature []byte `json:"thought_signature"`
	}
	if err := json.Unmarshal(m.Body, &v); err != nil {
		return nil
	}
	return v.ThoughtSignature
}

// ToolCall extracts fields from a tool_call message.
func (m Message) ToolCall() (callID, toolName string, args json.RawMessage, ok bool) {
	if m.Type != "tool_call" {
		return "", "", nil, false
	}
	var v struct {
		CallID   string          `json:"call_id"`
		ToolName string          `json:"tool_name"`
		Args     json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(m.Body, &v); err != nil {
		return "", "", nil, false
	}
	return v.CallID, v.ToolName, v.Args, true
}

// ToolCallThoughtSignature extracts the provider's opaque thought signature
// from a tool_call message. Nil when absent (pre-existing transcripts, providers
// without signatures) or when the message is not a tool_call.
func (m Message) ToolCallThoughtSignature() []byte {
	if m.Type != "tool_call" {
		return nil
	}
	var v struct {
		ThoughtSignature []byte `json:"thought_signature"`
	}
	if err := json.Unmarshal(m.Body, &v); err != nil {
		return nil
	}
	return v.ThoughtSignature
}

// ToolResult extracts fields from a tool_result message.
func (m Message) ToolResult() (callID, toolName, content string, data json.RawMessage, isError bool, ok bool) {
	if m.Type != "tool_result" {
		return "", "", "", nil, false, false
	}
	var v struct {
		CallID   string          `json:"call_id"`
		ToolName string          `json:"tool_name"`
		Content  string          `json:"content"`
		Data     json.RawMessage `json:"data"`
		IsError  bool            `json:"is_error"`
	}
	if err := json.Unmarshal(m.Body, &v); err != nil {
		return "", "", "", nil, false, false
	}
	return v.CallID, v.ToolName, v.Content, v.Data, v.IsError, true
}

// WithUsage returns a copy of the message with provider token usage recorded
// under the body's usage key, preserving existing fields. Plain-string bodies
// (unsigned text/thinking) convert to the object form; the Usage is metadata
// and is never sent to a provider.
func (m Message) WithUsage(u Usage) Message {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(m.Body, &obj); err != nil || obj == nil {
		obj = map[string]json.RawMessage{"text": m.Body}
	}
	raw, _ := json.Marshal(u)
	obj["usage"] = raw
	body, _ := json.Marshal(obj)
	m.Body = body
	return m
}

// Usage extracts the provider token usage recorded on the message (WithUsage).
// Nil when absent — older transcripts, providers that report none — so
// callers fall back to EstimateTokens.
func (m Message) Usage() *Usage {
	var v struct {
		Usage *Usage `json:"usage"`
	}
	if err := json.Unmarshal(m.Body, &v); err != nil {
		return nil
	}
	return v.Usage
}
