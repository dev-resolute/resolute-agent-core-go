package pi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/resolute-sh/pi-llm-go"
)

var errStubBoom = errors.New("stub provider boom")

// stubProvider is a deterministic, hermetic LLM provider for unit tests. The
// emit func writes the per-call event sequence; the provider then closes the
// events channel and delivers an empty terminal result.
type stubProvider struct {
	name string
	emit func(events chan<- llm.LLMEvent)
}

func (p *stubProvider) Name() string { return p.name }

func (p *stubProvider) Capabilities(model string) llm.ProviderCapabilities {
	return llm.ProviderCapabilities{Streaming: true, ToolCalling: true}
}

func (p *stubProvider) Stream(ctx context.Context, req llm.LLMRequest) llm.EventStream {
	events := make(chan llm.LLMEvent)
	done := make(chan llm.StreamResult, 1)
	go func() {
		p.emit(events)
		close(events)
		done <- llm.StreamResult{}
	}()
	return llm.NewEventStream(events, done)
}

func toolNamed(name string) RegisteredTool {
	return NewTool(Tool[struct{}]{
		Name:        name,
		Description: name,
		Execute:     func(ctx context.Context, p struct{}) (ToolResult, error) { return ToolResult{}, nil },
	})
}

func snapshotToolNames(a *Agent) []string {
	snap := a.snapshot()
	names := make([]string, len(snap.tools))
	for i, t := range snap.tools {
		names[i] = t.Name()
	}
	return names
}

func TestActiveToolsChangeMessageCodec(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		names []string
	}{
		{name: "single", names: []string{"a"}},
		{name: "multiple", names: []string{"a", "b", "c"}},
		{name: "empty non-nil means none", names: []string{}},
		{name: "nil means all", names: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// given a bookkeeping entry
			m := NewActiveToolsChange(tt.names)
			if m.Type != "active_tools_change" {
				t.Fatalf("Type = %q, want active_tools_change", m.Type)
			}

			// when serialized through the Go-native Message codec
			line, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var probe struct {
				Role string
				Type string
				Body json.RawMessage
			}
			if err := json.Unmarshal(line, &probe); err != nil {
				t.Fatalf("unmarshal go-shape: %v", err)
			}
			if probe.Type != "active_tools_change" || probe.Role != "system" {
				t.Fatalf("on-disk shape = %s, want Role=system Type=active_tools_change", line)
			}

			// then it roundtrips through the accessor
			var back Message
			if err := json.Unmarshal(line, &back); err != nil {
				t.Fatalf("unmarshal message: %v", err)
			}
			got, ok := back.ActiveToolNames()
			if !ok {
				t.Fatal("ActiveToolNames() not ok")
			}
			if len(got) != len(tt.names) || !sameStringSet(got, tt.names) {
				t.Fatalf("roundtrip names = %v, want %v", got, tt.names)
			}
		})
	}
}

func TestActiveToolNamesRejectsOtherTypes(t *testing.T) {
	t.Parallel()
	if _, ok := NewText("user", "hi").ActiveToolNames(); ok {
		t.Fatal("ActiveToolNames() must return ok=false for non-active_tools_change messages")
	}
}

func TestValidateToolConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		tools   []RegisteredTool
		active  []string
		wantErr error
	}{
		{name: "ok nil active", tools: []RegisteredTool{toolNamed("a"), toolNamed("b")}, active: nil},
		{name: "ok subset", tools: []RegisteredTool{toolNamed("a"), toolNamed("b")}, active: []string{"a"}},
		{name: "ok empty active", tools: []RegisteredTool{toolNamed("a")}, active: []string{}},
		{name: "duplicate tool", tools: []RegisteredTool{toolNamed("a"), toolNamed("a")}, active: nil, wantErr: ErrDuplicateToolName},
		{name: "unknown active", tools: []RegisteredTool{toolNamed("a")}, active: []string{"c"}, wantErr: ErrUnknownActiveTool},
		{name: "duplicate active", tools: []RegisteredTool{toolNamed("a"), toolNamed("b")}, active: []string{"a", "a"}, wantErr: ErrDuplicateToolName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateToolConfig(tt.tools, tt.active)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("validateToolConfig() = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateToolConfig() = %v, want nil", err)
			}
		})
	}
}

func TestNewAgentValidatesTools(t *testing.T) {
	t.Parallel()
	if _, err := NewAgent(AgentConfig{
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{toolNamed("a"), toolNamed("a")},
	}); !errors.Is(err, ErrDuplicateToolName) {
		t.Fatalf("NewAgent with duplicate tools = %v, want ErrDuplicateToolName", err)
	}
	if _, err := NewAgent(AgentConfig{
		DefaultModel:    "test/model",
		Tools:           []RegisteredTool{toolNamed("a")},
		ActiveToolNames: []string{"x"},
	}); !errors.Is(err, ErrUnknownActiveTool) {
		t.Fatalf("NewAgent with unknown active tool = %v, want ErrUnknownActiveTool", err)
	}
}

func TestSetToolsRevalidatesActiveSet(t *testing.T) {
	t.Parallel()
	a, err := NewAgent(AgentConfig{
		DefaultModel:    "test/model",
		Tools:           []RegisteredTool{toolNamed("a"), toolNamed("b")},
		ActiveToolNames: []string{"a"},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	if err := a.SetTools([]RegisteredTool{toolNamed("b")}); !errors.Is(err, ErrUnknownActiveTool) {
		t.Fatalf("SetTools dropping active tool = %v, want ErrUnknownActiveTool", err)
	}
	if err := a.SetTools([]RegisteredTool{toolNamed("b"), toolNamed("b")}); !errors.Is(err, ErrDuplicateToolName) {
		t.Fatalf("SetTools with duplicates = %v, want ErrDuplicateToolName", err)
	}
	if err := a.SetTools([]RegisteredTool{toolNamed("a"), toolNamed("c")}); err != nil {
		t.Fatalf("SetTools valid replacement = %v, want nil", err)
	}
}

func TestActiveToolsGating(t *testing.T) {
	t.Parallel()
	a, err := NewAgent(AgentConfig{
		DefaultModel:    "test/model",
		Tools:           []RegisteredTool{toolNamed("a"), toolNamed("b")},
		ActiveToolNames: []string{"a"},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	// The snapshot is the single source for the provider request's Tools (see
	// prompt.go runOneTurn), so an inactive tool absent here is never offered.
	if got := snapshotToolNames(a); !sameStringSet(got, []string{"a"}) {
		t.Fatalf("snapshot tools = %v, want only [a] (b inactive)", got)
	}

	all, err := NewAgent(AgentConfig{
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{toolNamed("a"), toolNamed("b")},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if got := snapshotToolNames(all); !sameStringSet(got, []string{"a", "b"}) {
		t.Fatalf("nil active set snapshot tools = %v, want [a b]", got)
	}
}

func TestSetActiveToolsAffectsSnapshot(t *testing.T) {
	t.Parallel()
	a, err := NewAgent(AgentConfig{
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{toolNamed("a"), toolNamed("b")},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if err := a.SetActiveTools(context.Background(), []string{"b"}); err != nil {
		t.Fatalf("SetActiveTools: %v", err)
	}
	if got := snapshotToolNames(a); !sameStringSet(got, []string{"b"}) {
		t.Fatalf("after SetActiveTools([b]) snapshot = %v, want [b]", got)
	}
	if err := a.SetActiveTools(context.Background(), nil); err != nil {
		t.Fatalf("SetActiveTools(nil): %v", err)
	}
	if got := snapshotToolNames(a); !sameStringSet(got, []string{"a", "b"}) {
		t.Fatalf("after SetActiveTools(nil) snapshot = %v, want [a b]", got)
	}
}

func TestSetActiveToolsRejectsUnknown(t *testing.T) {
	t.Parallel()
	a, err := NewAgent(AgentConfig{
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{toolNamed("a")},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if err := a.SetActiveTools(context.Background(), []string{"nope"}); !errors.Is(err, ErrUnknownActiveTool) {
		t.Fatalf("SetActiveTools unknown = %v, want ErrUnknownActiveTool", err)
	}
}

func TestResumeRestoresActiveSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newInternalMemorySession()
	sid, err := repo.Create(ctx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.AppendActiveToolsChange(ctx, sid, []string{"a"}); err != nil {
		t.Fatalf("AppendActiveToolsChange: %v", err)
	}
	// last-entry-wins
	if err := repo.AppendActiveToolsChange(ctx, sid, []string{"b"}); err != nil {
		t.Fatalf("AppendActiveToolsChange: %v", err)
	}

	a, err := NewAgent(AgentConfig{
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{toolNamed("a"), toolNamed("b")},
		Session:      repo,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if got := snapshotToolNames(a); !sameStringSet(got, []string{"a", "b"}) {
		t.Fatalf("before restore snapshot = %v, want all active", got)
	}

	if err := a.restoreActiveToolsFromSession(ctx, sid); err != nil {
		t.Fatalf("restoreActiveToolsFromSession: %v", err)
	}
	if got := snapshotToolNames(a); !sameStringSet(got, []string{"b"}) {
		t.Fatalf("after restore snapshot = %v, want [b] (last entry wins)", got)
	}

	// absent entry => all tools active
	empty, err := repo.Create(ctx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := a.restoreActiveToolsFromSession(ctx, empty); err != nil {
		t.Fatalf("restoreActiveToolsFromSession: %v", err)
	}
	if got := snapshotToolNames(a); !sameStringSet(got, []string{"a", "b"}) {
		t.Fatalf("restore from session without entry = %v, want all active", got)
	}
}

func TestRecordActiveToolsOnCreate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newInternalMemorySession()

	restricted, err := NewAgent(AgentConfig{
		DefaultModel:    "test/model",
		Tools:           []RegisteredTool{toolNamed("a"), toolNamed("b")},
		ActiveToolNames: []string{"a"},
		Session:         repo,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	sid, err := repo.Create(ctx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := restricted.recordActiveToolsOnCreate(ctx, sid); err != nil {
		t.Fatalf("recordActiveToolsOnCreate: %v", err)
	}
	msgs, err := repo.Load(ctx, sid)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	names, ok := activeToolNamesFromTranscript(msgs)
	if !ok || !sameStringSet(names, []string{"a"}) {
		t.Fatalf("recorded active set = %v ok=%v, want [a]", names, ok)
	}

	// all-active set records nothing
	all, err := NewAgent(AgentConfig{
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{toolNamed("a"), toolNamed("b")},
		Session:      repo,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	sid2, err := repo.Create(ctx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := all.recordActiveToolsOnCreate(ctx, sid2); err != nil {
		t.Fatalf("recordActiveToolsOnCreate: %v", err)
	}
	msgs2, err := repo.Load(ctx, sid2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := activeToolNamesFromTranscript(msgs2); ok {
		t.Fatal("all-active set must not record an active_tools_change entry")
	}
}

func TestSetActiveToolsPersistsWhenBoundIdle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newInternalMemorySession()
	sid, err := repo.Create(ctx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	a, err := NewAgent(AgentConfig{
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{toolNamed("a"), toolNamed("b")},
		Session:      repo,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	a.mu.Lock()
	a.lastSessionID = sid
	a.mu.Unlock()

	if err := a.SetActiveTools(ctx, []string{"a"}); err != nil {
		t.Fatalf("SetActiveTools: %v", err)
	}
	msgs, err := repo.Load(ctx, sid)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	names, ok := activeToolNamesFromTranscript(msgs)
	if !ok || !sameStringSet(names, []string{"a"}) {
		t.Fatalf("bound idle setter persisted = %v ok=%v, want [a]", names, ok)
	}
}

func TestFlushPendingActiveTools(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newInternalMemorySession()
	sid, err := repo.Create(ctx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	a, err := NewAgent(AgentConfig{
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{toolNamed("a"), toolNamed("b")},
		Session:      repo,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	a.mu.Lock()
	a.pendingActiveTools = append(a.pendingActiveTools, []string{"a"})
	a.mu.Unlock()

	pr := &promptRun{agent: a, sessionID: sid}
	if err := pr.flushPendingActiveTools(ctx); err != nil {
		t.Fatalf("flushPendingActiveTools: %v", err)
	}

	msgs, err := repo.Load(ctx, sid)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	names, ok := activeToolNamesFromTranscript(msgs)
	if !ok || !sameStringSet(names, []string{"a"}) {
		t.Fatalf("flushed entry = %v ok=%v, want [a]", names, ok)
	}

	a.mu.Lock()
	pending := len(a.pendingActiveTools)
	a.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pendingActiveTools = %d, want 0 (drained)", pending)
	}
}

func TestEmptyActiveSetRoundTripsThroughBind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newInternalMemorySession()
	provider := &stubProvider{
		name: "test",
		emit: func(events chan<- llm.LLMEvent) {
			events <- llm.TextDeltaEvent{Delta: "ok"}
			events <- llm.MessageEndEvent{}
		},
	}

	// given an agent whose active set is empty-but-non-nil (NO tools active)
	a, err := NewAgent(AgentConfig{
		Providers:       []llm.LLMProvider{provider},
		DefaultModel:    "test/model",
		Tools:           []RegisteredTool{toolNamed("a"), toolNamed("b")},
		ActiveToolNames: []string{},
		Session:         repo,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	// when a prompt binds the session and records the active set at bind time
	stream, err := a.Prompt(ctx, NewText("user", "hi"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	_, result := drain(t, stream)
	if result.Err != nil {
		t.Fatalf("prompt error: %v", result.Err)
	}
	sid := a.State().SessionID
	if sid == "" {
		t.Fatal("expected a bound session id")
	}

	// then resuming in a fresh agent restores zero active tools (NONE, not ALL)
	b, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{toolNamed("a"), toolNamed("b")},
		Session:      repo,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if err := b.restoreActiveToolsFromSession(ctx, sid); err != nil {
		t.Fatalf("restoreActiveToolsFromSession: %v", err)
	}
	if got := snapshotToolNames(b); len(got) != 0 {
		t.Fatalf("resumed active tools = %v, want none (empty set must not resume as all)", got)
	}
}

func TestErrorExitPathFlushesPendingAndNothingSurvives(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newInternalMemorySession()
	provider := &stubProvider{
		name: "test",
		emit: func(events chan<- llm.LLMEvent) {
			events <- llm.LLMErrorEvent{Error: errStubBoom, Transient: false}
		},
	}
	a, err := NewAgent(AgentConfig{
		Providers:    []llm.LLMProvider{provider},
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{toolNamed("a"), toolNamed("b")},
		Session:      repo,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	// given an active-tools change queued for flush at the turn-end safe point
	a.mu.Lock()
	a.pendingActiveTools = append(a.pendingActiveTools, []string{"a"})
	a.mu.Unlock()

	// when the prompt exits via the error path (provider non-transient error)
	stream, err := a.Prompt(ctx, NewText("user", "hi"), PromptOpts{})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	_, result := drain(t, stream)
	if !errors.Is(result.Err, errStubBoom) {
		t.Fatalf("prompt err = %v, want stub boom", result.Err)
	}

	// then nothing is stranded in the deferral queue
	a.mu.Lock()
	pending := len(a.pendingActiveTools)
	a.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pendingActiveTools = %d after error exit, want 0", pending)
	}

	// and the queued entry was still flushed to the bound session
	sid := a.State().SessionID
	msgs, err := repo.Load(ctx, sid)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	names, ok := activeToolNamesFromTranscript(msgs)
	if !ok || !sameStringSet(names, []string{"a"}) {
		t.Fatalf("error-path flush = %v ok=%v, want [a]", names, ok)
	}
}

func TestSetActiveToolsCopiesCallerSlice(t *testing.T) {
	t.Parallel()
	a, err := NewAgent(AgentConfig{
		DefaultModel: "test/model",
		Tools:        []RegisteredTool{toolNamed("a"), toolNamed("b")},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	names := []string{"a"}
	if err := a.SetActiveTools(context.Background(), names); err != nil {
		t.Fatalf("SetActiveTools: %v", err)
	}
	names[0] = "b" // caller mutates its slice after the call

	if got := snapshotToolNames(a); !sameStringSet(got, []string{"a"}) {
		t.Fatalf("snapshot = %v, want [a] (caller mutation must not leak into the agent)", got)
	}
}

func TestActiveToolsChangeNeverCutPoint(t *testing.T) {
	t.Parallel()
	if isValidCutPoint(NewActiveToolsChange([]string{"a"})) {
		t.Fatal("active_tools_change must never be a valid compaction cut point")
	}
}

func TestActiveToolsChangeExcludedFromLLMContext(t *testing.T) {
	t.Parallel()
	msgs := []Message{
		NewText("user", "hi"),
		NewActiveToolsChange([]string{"a"}),
		NewText("assistant", "yo"),
	}

	out := DefaultConvertToLLM(msgs)
	if len(out) != 2 {
		t.Fatalf("DefaultConvertToLLM len = %d, want 2 (bookkeeping excluded)", len(out))
	}

	ctxMsgs := BuildLLMContext(msgs, nil)
	if len(ctxMsgs) != 2 {
		t.Fatalf("BuildLLMContext len = %d, want 2 (bookkeeping excluded)", len(ctxMsgs))
	}
	for _, m := range ctxMsgs {
		if m.Type == "active_tools_change" {
			t.Fatal("BuildLLMContext leaked active_tools_change to the model")
		}
	}

	summarized := BuildLLMContext(msgs, []BranchSummary{{StartIdx: 0, EndIdx: 2, Summary: "s"}})
	for _, m := range summarized {
		if m.Type == "active_tools_change" {
			t.Fatal("BuildLLMContext with summary leaked active_tools_change")
		}
	}
}
