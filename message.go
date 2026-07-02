// Package pi provides a stateful agent loop built on pi-llm-go.
package pi

import "encoding/json"

// Message is the agent-side unit of transcript content.
// The framework treats Body as opaque bytes for user-defined custom message types.
type Message struct {
	Role string
	Type string
	Body json.RawMessage
}

// NewText creates a text message.
func NewText(role, text string) Message {
	body, _ := json.Marshal(text)
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

// NewThinking creates a thinking message.
func NewThinking(role, text string) Message {
	body, _ := json.Marshal(text)
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
func (m Message) Text() string {
	if m.Type != "text" && m.Type != "branch_summary" {
		return ""
	}
	var s string
	_ = json.Unmarshal(m.Body, &s)
	return s
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
