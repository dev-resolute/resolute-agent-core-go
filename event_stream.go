package pi

// EventStream is the shared return shape for an Agent prompt. It carries a
// stream of typed events on Events (closed by the sender when the prompt
// completes) and exactly one terminal PromptResult on Done.
type EventStream struct {
	Events <-chan AgentEvent
	Done   <-chan PromptResult
}
