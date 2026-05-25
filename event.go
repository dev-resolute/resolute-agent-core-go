package pi

// AgentEvent is a sealed interface for every event that flows on Run.Events.
type AgentEvent interface {
	isAgentEvent()
}

// TurnStartEvent signals the beginning of a new agent turn.
type TurnStartEvent struct{ Turn int }

func (TurnStartEvent) isAgentEvent() {}

// TurnEndEvent signals the end of an agent turn.
type TurnEndEvent struct{ Turn int }

func (TurnEndEvent) isAgentEvent() {}

// TextDeltaEvent carries a fragment of assistant text.
type TextDeltaEvent struct{ Delta string }

func (TextDeltaEvent) isAgentEvent() {}

// ThinkingDeltaEvent carries a fragment of assistant thinking.
type ThinkingDeltaEvent struct{ Delta string }

func (ThinkingDeltaEvent) isAgentEvent() {}

// ToolCallStartEvent signals that a tool call has started.
type ToolCallStartEvent struct {
	CallID   string
	ToolName string
	Args     []byte
}

func (ToolCallStartEvent) isAgentEvent() {}

// ToolCallEndEvent signals that a tool call has completed.
type ToolCallEndEvent struct {
	CallID   string
	ToolName string
	Result   ToolResult
}

func (ToolCallEndEvent) isAgentEvent() {}

// ToolErrorEvent signals that a tool call errored.
type ToolErrorEvent struct {
	CallID   string
	ToolName string
	Error    error
}

func (ToolErrorEvent) isAgentEvent() {}

// LLMErrorEvent signals an error from the LLM provider.
type LLMErrorEvent struct {
	Error     error
	Transient bool
}

func (LLMErrorEvent) isAgentEvent() {}

// LLMRetryEvent signals a retry attempt.
type LLMRetryEvent struct {
	Provider   string
	Model      string
	Attempt    int
	NextDelay  int64 // milliseconds
	Reason     string
	ServerHint bool
}

func (LLMRetryEvent) isAgentEvent() {}

// ThinkingUnsupportedEvent signals that thinking was requested but unsupported.
type ThinkingUnsupportedEvent struct {
	Requested   string
	Provider    string
	Model       string
	Reason      string
}

func (ThinkingUnsupportedEvent) isAgentEvent() {}

// ToolLeakEvent signals that a tool ignored context cancellation.
type ToolLeakEvent struct {
	ToolName string
	CallID   string
	Duration int64 // milliseconds
}

func (ToolLeakEvent) isAgentEvent() {}

// UserMessageEvent signals a user message being processed.
type UserMessageEvent struct{ Message Message }

func (UserMessageEvent) isAgentEvent() {}

// SteerInjectedEvent signals that a steered message has been injected.
type SteerInjectedEvent struct{ Message Message }

func (SteerInjectedEvent) isAgentEvent() {}

// FollowUpInjectedEvent signals that a follow-up message has been injected.
type FollowUpInjectedEvent struct{ Message Message }

func (FollowUpInjectedEvent) isAgentEvent() {}

// CompactionStartEvent signals the start of compaction.
type CompactionStartEvent struct{}

func (CompactionStartEvent) isAgentEvent() {}

// CompactionEndEvent signals the end of compaction.
type CompactionEndEvent struct{}

func (CompactionEndEvent) isAgentEvent() {}
