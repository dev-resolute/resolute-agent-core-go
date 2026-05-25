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
