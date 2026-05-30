package pi

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/resolute-sh/pi-llm-go"
)

// Agent is the persistent, configurable object that owns tools, hooks,
// the session backend, default options, and the system prompt.
type Agent struct {
	config  AgentConfig
	session SessionRepo
	hooks   Hooks

	running atomic.Int32
	mu      sync.RWMutex

	// Mutable runtime config, guarded by mu. Seeded from AgentConfig and
	// changed thereafter via the setters; each turn snapshot picks up the
	// current values (see snapshot.go).
	model         string
	tools         []RegisteredTool
	systemPrompt  string
	thinkingLevel llm.ThinkingLevel
	skills        []Skill

	current       *promptRun
	lastSessionID SessionID
}

// NewAgent creates an Agent from the given config.
func NewAgent(cfg AgentConfig) (*Agent, error) {
	if cfg.EventBufferSize == 0 {
		cfg.EventBufferSize = 16
	}
	if cfg.SteerBufferSize == 0 {
		cfg.SteerBufferSize = 8
	}
	if cfg.MaxParallelTools == 0 {
		cfg.MaxParallelTools = 32
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 30 * time.Second
	}
	if cfg.ReserveTokens == 0 {
		cfg.ReserveTokens = 16384
	}
	if cfg.KeepRecentTokens == 0 {
		cfg.KeepRecentTokens = 20000
	}
	if cfg.ConvertToLLM == nil {
		cfg.ConvertToLLM = DefaultConvertToLLM
	}

	s := cfg.Session
	if s == nil {
		s = newInternalMemorySession()
	}

	return &Agent{
		config:        cfg,
		session:       s,
		hooks:         cfg.Hooks,
		model:         cfg.DefaultModel,
		tools:         cfg.Tools,
		systemPrompt:  cfg.SystemPrompt,
		thinkingLevel: cfg.DefaultThinking,
	}, nil
}

// Prompt starts a new prompt and returns its EventStream. The user message is
// the second argument; per-prompt overrides are carried on opts.
func (a *Agent) Prompt(ctx context.Context, msg Message, opts PromptOpts) (*EventStream, error) {
	// Single-runner guard: at most one prompt in flight per Agent. Release the
	// slot on any error path before the loop goroutine takes ownership of it.
	if !a.running.CompareAndSwap(0, 1) {
		return nil, ErrAgentBusy
	}
	launched := false
	defer func() {
		if !launched {
			a.running.Store(0)
		}
	}()

	// Resolve the effective model from the current snapshot, overlaid with any
	// per-prompt override. The snapshot is re-taken each turn in the loop so
	// setters take effect on the next turn; this resolution validates upfront.
	snap := a.snapshot()

	modelRef := opts.Model
	if modelRef == "" {
		modelRef = snap.model
	}
	if modelRef == "" {
		return nil, fmt.Errorf("no model specified and no default model: %w", ErrInvalidModelRef)
	}

	providerName, modelID, err := parseModelRef(modelRef)
	if err != nil {
		return nil, err
	}
	if _, err := a.providerByName(providerName); err != nil {
		return nil, err
	}

	// Resolve session
	sid := opts.SessionID
	if sid == "" {
		sid, err = a.session.Create(ctx)
		if err != nil {
			return nil, fmt.Errorf("creating session: %w", err)
		}
		// Append system prompt for new session
		sysPrompt := opts.SystemPrompt
		if sysPrompt == "" {
			sysPrompt = snap.systemPrompt
		}
		if sysPrompt != "" {
			if err := a.session.Append(ctx, sid, NewSystem(sysPrompt)); err != nil {
				return nil, fmt.Errorf("appending system prompt: %w", err)
			}
		}
	} else {
		// Verify session exists
		_, err := a.session.Load(ctx, sid)
		if err != nil {
			return nil, fmt.Errorf("loading session %q: %w", sid, ErrSessionNotFound)
		}
		// Optionally override system prompt
		if opts.SystemPrompt != "" {
			if err := a.overrideSystemPrompt(ctx, sid, opts.SystemPrompt); err != nil {
				return nil, fmt.Errorf("overriding system prompt: %w", err)
			}
		}
	}

	// Append user prompt
	if err := a.session.Append(ctx, sid, msg); err != nil {
		return nil, fmt.Errorf("appending user message: %w", err)
	}

	thinking := opts.Thinking
	if thinking == llm.ThinkingOff {
		thinking = snap.thinkingLevel
	}

	pr := &promptRun{
		agent:         a,
		optModel:      opts.Model,
		optThinking:   opts.Thinking,
		providerHints: opts.ProviderHints,
		model:         modelID,
		thinking:      thinking,
		sessionID:     sid,
		phase:         PhaseIdle,
		startedAt:     time.Now(),
		events:        make(chan AgentEvent, a.config.EventBufferSize),
		done:          make(chan PromptResult, 1),
		steerCh:       make(chan steerMsg, a.config.SteerBufferSize),
		followUpCh:    make(chan Message, a.config.SteerBufferSize),
	}

	if a.hooks.BeforeAgentStart != nil {
		if err := a.hooks.BeforeAgentStart(ctx, BeforeAgentStartCtx{PromptOpts: opts}); err != nil {
			return nil, fmt.Errorf("before agent start hook: %w", err)
		}
	}

	// Set up internal context so Stop() works even before loop() starts.
	innerCtx, cancel := context.WithCancelCause(ctx)
	pr.cancelMu.Lock()
	pr.cancel = cancel
	pr.cancelMu.Unlock()

	a.mu.Lock()
	a.lastSessionID = sid
	a.current = pr
	a.mu.Unlock()

	launched = true
	go pr.loop(innerCtx)

	return &EventStream{Events: pr.events, Done: pr.done}, nil
}

// Stop fire-and-forget cancels the in-flight prompt. Idempotent; a no-op when
// no prompt is in flight.
func (a *Agent) Stop() {
	pr := a.currentPrompt()
	if pr != nil {
		pr.stop()
	}
}

// Steer enqueues a message for injection into the in-flight prompt at the next
// safe point.
func (a *Agent) Steer(ctx context.Context, m Message) error {
	pr := a.currentPrompt()
	if pr == nil {
		return ErrNoPromptInFlight
	}
	return pr.steer(ctx, m)
}

// FollowUp enqueues a message for after the in-flight prompt completes.
func (a *Agent) FollowUp(ctx context.Context, m Message) error {
	pr := a.currentPrompt()
	if pr == nil {
		return ErrNoPromptInFlight
	}
	return pr.followUp(ctx, m)
}

// State returns a snapshot of the current (or most recent) prompt's state.
func (a *Agent) State() AgentState {
	pr := a.currentPrompt()
	if pr == nil {
		return AgentState{}
	}
	return pr.state()
}

// Phase returns the current (or most recent) prompt's phase.
func (a *Agent) Phase() AgentPhase {
	return a.State().Phase
}

// Transcript returns a copy of the current (or most recent) prompt's transcript.
func (a *Agent) Transcript() []Message {
	pr := a.currentPrompt()
	if pr == nil {
		return nil
	}
	return pr.transcriptCopy()
}

// Close stops any in-flight prompt and releases the Agent. Idempotent.
func (a *Agent) Close() error {
	a.Stop()
	return nil
}

func (a *Agent) currentPrompt() *promptRun {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.current
}

func (a *Agent) providerByName(name string) (llm.LLMProvider, error) {
	for _, p := range a.config.Providers {
		if p.Name() == name {
			return p, nil
		}
	}
	return nil, fmt.Errorf("provider %q not found: %w", name, ErrInvalidModelRef)
}

func (a *Agent) isRunning() bool {
	return a.running.Load() > 0
}

func (a *Agent) overrideSystemPrompt(ctx context.Context, sid SessionID, prompt string) error {
	msgs, err := a.session.Load(ctx, sid)
	if err != nil {
		return err
	}
	if len(msgs) > 0 && msgs[0].Role == "system" {
		msgs[0] = NewSystem(prompt)
		// MemorySession doesn't support overwrite; JSONLSession doesn't either.
		// For v0.1.0, append a new system message and let ConvertToLLM handle extraction.
		return a.session.Append(ctx, sid, NewSystem(prompt))
	}
	return nil
}

func parseModelRef(ref string) (providerName, modelID string, err error) {
	idx := 0
	for i, c := range ref {
		if c == '/' {
			idx = i
			break
		}
	}
	if idx == 0 {
		return "", "", fmt.Errorf("model reference %q missing provider prefix: %w", ref, ErrInvalidModelRef)
	}
	return ref[:idx], ref[idx+1:], nil
}

// DefaultConvertToLLM converts the built-in agent message types to llm.Message.
func DefaultConvertToLLM(messages []Message) []llm.Message {
	var out []llm.Message
	for _, msg := range messages {
		switch msg.Type {
		case "text":
			out = append(out, llm.Message{
				Role:    msg.Role,
				Content: llm.TextContent{Text: msg.Text()},
			})
		case "tool_call":
			callID, toolName, args, ok := msg.ToolCall()
			if ok {
				out = append(out, llm.Message{
					Role: "assistant",
					Content: llm.ToolCallContent{
						CallID:   callID,
						ToolName: toolName,
						Args:     args,
					},
				})
			}
		case "tool_result":
			callID, toolName, content, data, isError, ok := msg.ToolResult()
			if ok {
				out = append(out, llm.Message{
					Role: "tool",
					Content: llm.ToolResultContent{
						CallID:   callID,
						ToolName: toolName,
						Content:  content,
						Data:     data,
						IsError:  isError,
					},
				})
			}
		case "thinking":
			out = append(out, llm.Message{
				Role:    msg.Role,
				Content: llm.ThinkingContent{Text: msg.Text()},
			})
		case "branch_summary":
			out = append(out, llm.Message{
				Role:    msg.Role,
				Content: llm.TextContent{Text: "<summary>" + msg.Text() + "</summary>"},
			})
		default:
			// User-defined types: pass through as text for v0.1.0.
			out = append(out, llm.Message{
				Role:    msg.Role,
				Content: llm.TextContent{Text: string(msg.Body)},
			})
		}
	}
	return out
}

// internalMemorySession is a minimal in-memory session backend used as the default.
// It intentionally duplicates session.MemorySession because the root pi package
// cannot import the session subpackage (session imports pi, creating a cycle).
// A behavioral equivalence test ensures the two implementations do not drift.
type internalMemorySession struct {
	mu        sync.Mutex
	sessions  map[SessionID][]Message
	summaries map[SessionID][]BranchSummary
	meta      map[SessionID]SessionMeta
}

func newInternalMemorySession() *internalMemorySession {
	return &internalMemorySession{
		sessions:  make(map[SessionID][]Message),
		summaries: make(map[SessionID][]BranchSummary),
		meta:      make(map[SessionID]SessionMeta),
	}
}

func (m *internalMemorySession) Create(ctx context.Context) (SessionID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := SessionID(NewSessionID())
	m.sessions[id] = nil
	m.meta[id] = SessionMeta{ID: id, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	return id, nil
}

func (m *internalMemorySession) Append(ctx context.Context, id SessionID, msgs ...Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[id] = append(m.sessions[id], msgs...)
	if meta, ok := m.meta[id]; ok {
		meta.UpdatedAt = time.Now()
		m.meta[id] = meta
	}
	return nil
}

func (m *internalMemorySession) Load(ctx context.Context, id SessionID) ([]Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	msgs, ok := m.sessions[id]
	if !ok {
		return nil, nil
	}
	out := make([]Message, len(msgs))
	copy(out, msgs)
	return out, nil
}

func (m *internalMemorySession) List(ctx context.Context) ([]SessionMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SessionMeta
	for _, meta := range m.meta {
		out = append(out, meta)
	}
	return out, nil
}

func (m *internalMemorySession) AppendBranchSummary(ctx context.Context, id SessionID, summary BranchSummary) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.summaries[id] = append(m.summaries[id], summary)
	return nil
}

func (m *internalMemorySession) LoadBranchSummaries(ctx context.Context, id SessionID) ([]BranchSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]BranchSummary, len(m.summaries[id]))
	copy(out, m.summaries[id])
	return out, nil
}

func (m *internalMemorySession) Delete(ctx context.Context, id SessionID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	delete(m.summaries, id)
	delete(m.meta, id)
	return nil
}
