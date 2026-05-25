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
	mu      sync.Mutex

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
		config:  cfg,
		session: s,
		hooks:   cfg.Hooks,
	}, nil
}

// Run initiates a new agent run.
func (a *Agent) Run(ctx context.Context, opts RunOpts) (*Run, error) {
	// Resolve model and provider
	modelRef := opts.Model
	if modelRef == "" {
		modelRef = a.config.DefaultModel
	}
	if modelRef == "" {
		return nil, fmt.Errorf("no model specified and no default model: %w", ErrInvalidModelRef)
	}

	providerName, modelID, err := parseModelRef(modelRef)
	if err != nil {
		return nil, err
	}

	var provider llm.LLMProvider
	for _, p := range a.config.Providers {
		if p.Name() == providerName {
			provider = p
			break
		}
	}
	if provider == nil {
		return nil, fmt.Errorf("provider %q not found: %w", providerName, ErrInvalidModelRef)
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
			sysPrompt = a.config.SystemPrompt
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
	if err := a.session.Append(ctx, sid, opts.Prompt); err != nil {
		return nil, fmt.Errorf("appending user message: %w", err)
	}

	a.mu.Lock()
	a.lastSessionID = sid
	a.mu.Unlock()

	// Create run
	thinking := opts.Thinking
	if thinking == llm.ThinkingOff && a.config.DefaultThinking != llm.ThinkingOff {
		thinking = a.config.DefaultThinking
	}

	run := &Run{
		agent:        a,
		provider:     provider,
		model:        modelID,
		thinking:     thinking,
		providerHints: opts.ProviderHints,
		sessionID:    sid,
		phase:        PhaseIdle,
		startedAt:    time.Now(),
		events:       make(chan AgentEvent, a.config.EventBufferSize),
		done:         make(chan RunResult, 1),
		steerCh:      make(chan steerMsg, a.config.SteerBufferSize),
		followUpCh:   make(chan Message, a.config.SteerBufferSize),
	}

	if a.hooks.BeforeAgentStart != nil {
		if err := a.hooks.BeforeAgentStart(ctx, BeforeAgentStartCtx{RunOpts: opts}); err != nil {
			return nil, fmt.Errorf("before agent start hook: %w", err)
		}
	}

	// Set up internal context so Stop() works even before loop() starts.
	innerCtx, cancel := context.WithCancelCause(ctx)
	run.cancelMu.Lock()
	run.cancel = cancel
	run.cancelMu.Unlock()

	a.running.Add(1)
	go run.loop(innerCtx)

	return run, nil
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
