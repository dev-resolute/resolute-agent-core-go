package pi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dev-resolute/resolute-llm-go"
)

// flakySummarizingProvider fails its first `failures` Stream calls with
// failErr, then answers with a fixed summary. Safe for the concurrent
// split-turn calls.
type flakySummarizingProvider struct {
	mu       sync.Mutex
	failures int
	failErr  error
	calls    int
}

func (*flakySummarizingProvider) Name() string { return "test" }

func (*flakySummarizingProvider) Capabilities(string) llm.ProviderCapabilities {
	return llm.ProviderCapabilities{Streaming: true}
}

func (p *flakySummarizingProvider) Stream(ctx context.Context, req llm.LLMRequest) llm.EventStream {
	p.mu.Lock()
	p.calls++
	fail := p.failures > 0
	if fail {
		p.failures--
	}
	p.mu.Unlock()

	events := make(chan llm.LLMEvent, 2)
	done := make(chan llm.StreamResult, 1)
	if fail {
		close(events)
		done <- llm.StreamResult{Messages: req.Messages, Err: p.failErr}
	} else {
		events <- llm.TextDeltaEvent{Delta: "## Goal\nsummarized"}
		close(events)
		done <- llm.StreamResult{Messages: append(req.Messages, llm.Message{
			Role:    "assistant",
			Content: llm.TextContent{Text: "## Goal\nsummarized"},
		})}
	}
	close(done)
	return llm.NewEventStream(events, done)
}

func (p *flakySummarizingProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// retryHookRecorder records OnSummarizationRetry calls. Safe for concurrent use.
type retryHookRecorder struct {
	mu     sync.Mutex
	events []SummarizationRetryCtx
}

func (r *retryHookRecorder) record(_ context.Context, c SummarizationRetryCtx) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, c)
}

func (r *retryHookRecorder) phases() []SummarizationRetryPhase {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SummarizationRetryPhase, len(r.events))
	for i, e := range r.events {
		out[i] = e.Phase
	}
	return out
}

func (r *retryHookRecorder) last() (SummarizationRetryCtx, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return SummarizationRetryCtx{}, false
	}
	return r.events[len(r.events)-1], true
}

// newCompactableAgent builds an Agent with a session long enough to compact.
func newCompactableAgent(t *testing.T, provider llm.LLMProvider, policy SummarizationRetryPolicy, rec *retryHookRecorder) (*Agent, SessionID) {
	t.Helper()
	session := newInternalMemorySession()
	ctx := context.Background()
	sid, err := session.Create(ctx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	long := strings.Repeat("history that should be summarized away. ", 40)
	for i := 0; i < 6; i++ {
		if err := session.Append(ctx, sid, NewText("user", long), NewText("assistant", long)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	cfg := AgentConfig{
		Providers:          []llm.LLMProvider{provider},
		DefaultModel:       "test/model",
		Session:            session,
		KeepRecentTokens:   50,
		SummarizationRetry: policy,
	}
	if rec != nil {
		cfg.Hooks.OnSummarizationRetry = rec.record
	}
	agent, err := NewAgent(cfg)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	return agent, sid
}

func TestCompactSummarizationRetrySucceedsAfterTransientFailures(t *testing.T) {
	provider := &flakySummarizingProvider{failures: 2, failErr: errors.New("503 service unavailable")}
	rec := &retryHookRecorder{}
	agent, sid := newCompactableAgent(t, provider, SummarizationRetryPolicy{
		MaxRetries: 3,
		BaseDelay:  time.Millisecond,
		MaxDelay:   10 * time.Millisecond,
	}, rec)

	res, err := agent.Compact(context.Background(), CompactOpts{SessionID: sid})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.RemovedCount == 0 {
		t.Error("expected messages to be compacted")
	}
	if got := provider.callCount(); got != 3 {
		t.Errorf("provider calls = %d, want 3 (1 initial + 2 retries)", got)
	}

	wantPhases := []SummarizationRetryPhase{
		SummarizationRetryScheduled, SummarizationRetryAttemptStart,
		SummarizationRetryScheduled, SummarizationRetryAttemptStart,
		SummarizationRetryFinished,
	}
	got := rec.phases()
	if len(got) != len(wantPhases) {
		t.Fatalf("hook phases = %v, want %v", got, wantPhases)
	}
	for i, p := range wantPhases {
		if got[i] != p {
			t.Fatalf("hook phases = %v, want %v", got, wantPhases)
		}
	}
	last, _ := rec.last()
	if !last.Success {
		t.Error("final Finished event should report Success")
	}
}

func TestCompactSummarizationRetryExhausted(t *testing.T) {
	provider := &flakySummarizingProvider{failures: 99, failErr: errors.New("503 service unavailable")}
	rec := &retryHookRecorder{}
	agent, sid := newCompactableAgent(t, provider, SummarizationRetryPolicy{
		MaxRetries: 2,
		BaseDelay:  time.Millisecond,
	}, rec)

	_, err := agent.Compact(context.Background(), CompactOpts{SessionID: sid})
	if err == nil {
		t.Fatal("Compact should fail when retries are exhausted")
	}
	if got := provider.callCount(); got != 3 {
		t.Errorf("provider calls = %d, want 3 (1 initial + 2 retries)", got)
	}
	last, ok := rec.last()
	if !ok || last.Phase != SummarizationRetryFinished || last.Success {
		t.Errorf("last hook event = %+v, want unsuccessful Finished", last)
	}
}

func TestCompactSummarizationNoRetryOnFatal(t *testing.T) {
	provider := &flakySummarizingProvider{failures: 99, failErr: fmt.Errorf("gemini: %w", llm.ErrProviderFatal)}
	rec := &retryHookRecorder{}
	agent, sid := newCompactableAgent(t, provider, SummarizationRetryPolicy{
		MaxRetries: 3,
		BaseDelay:  time.Millisecond,
	}, rec)

	_, err := agent.Compact(context.Background(), CompactOpts{SessionID: sid})
	if err == nil {
		t.Fatal("Compact should fail on fatal provider error")
	}
	if got := provider.callCount(); got != 1 {
		t.Errorf("provider calls = %d, want 1 (fatal errors fail fast)", got)
	}
	if phases := rec.phases(); len(phases) != 0 {
		t.Errorf("hook fired %d times, want 0 for non-retried failure", len(phases))
	}
}

func TestCompactSummarizationNoRetryOnContextOverflow(t *testing.T) {
	provider := &flakySummarizingProvider{failures: 99, failErr: llm.ErrContextOverflow}
	agent, sid := newCompactableAgent(t, provider, SummarizationRetryPolicy{
		MaxRetries: 3,
		BaseDelay:  time.Millisecond,
	}, nil)

	_, err := agent.Compact(context.Background(), CompactOpts{SessionID: sid})
	if err == nil {
		t.Fatal("Compact should fail on context overflow")
	}
	if got := provider.callCount(); got != 1 {
		t.Errorf("provider calls = %d, want 1 (overflow is the harness ladder's job, LLM-8)", got)
	}
}

func TestCompactSummarizationRetryDisabledByDefault(t *testing.T) {
	provider := &flakySummarizingProvider{failures: 99, failErr: errors.New("503 service unavailable")}
	rec := &retryHookRecorder{}
	agent, sid := newCompactableAgent(t, provider, SummarizationRetryPolicy{}, rec)

	_, err := agent.Compact(context.Background(), CompactOpts{SessionID: sid})
	if err == nil {
		t.Fatal("Compact should fail when summarization fails and retries are disabled")
	}
	if got := provider.callCount(); got != 1 {
		t.Errorf("provider calls = %d, want 1 (zero policy disables retries)", got)
	}
	if phases := rec.phases(); len(phases) != 0 {
		t.Errorf("hook fired %d times, want 0 when retries disabled", len(phases))
	}
}

func TestCompactSummarizationRetryAbortDuringBackoff(t *testing.T) {
	provider := &flakySummarizingProvider{failures: 99, failErr: errors.New("503 service unavailable")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel the parent context the moment the first retry is scheduled; the
	// hour-long backoff must be interrupted immediately.
	rec := &retryHookRecorder{}
	cancelOnSchedule := func(_ context.Context, c SummarizationRetryCtx) {
		rec.record(ctx, c)
		if c.Phase == SummarizationRetryScheduled {
			cancel()
		}
	}

	session := newInternalMemorySession()
	sid, err := session.Create(context.Background())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	long := strings.Repeat("history that should be summarized away. ", 40)
	for i := 0; i < 6; i++ {
		if err := session.Append(context.Background(), sid, NewText("user", long), NewText("assistant", long)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	agent, err := NewAgent(AgentConfig{
		Providers:          []llm.LLMProvider{provider},
		DefaultModel:       "test/model",
		Session:            session,
		KeepRecentTokens:   50,
		SummarizationRetry: SummarizationRetryPolicy{MaxRetries: 3, BaseDelay: time.Hour},
		Hooks:              Hooks{OnSummarizationRetry: cancelOnSchedule},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	start := time.Now()
	_, err = agent.Compact(ctx, CompactOpts{SessionID: sid})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Compact error = %v, want wrapped context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("abort during backoff took %v, want prompt return", elapsed)
	}
}

// TestSplitTurnSummarizeRetriesBothCalls drives the split-turn path directly:
// both concurrent summarization calls fail once, then succeed.
func TestSplitTurnSummarizeRetriesBothCalls(t *testing.T) {
	provider := &flakySummarizingProvider{failures: 2, failErr: errors.New("429 too many requests")}
	rec := &retryHookRecorder{}
	agent, err := NewAgent(AgentConfig{
		Providers:          []llm.LLMProvider{provider},
		DefaultModel:       "test/model",
		Session:            newInternalMemorySession(),
		SummarizationRetry: SummarizationRetryPolicy{MaxRetries: 2, BaseDelay: time.Millisecond},
		Hooks:              Hooks{OnSummarizationRetry: rec.record},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	prep := &compactionPrep{
		cutIdx:   2,
		prefix:   []Message{NewText("user", "do the thing"), NewText("assistant", "working on it")},
		keptTail: []Message{NewText("user", "recent work kept verbatim")},
	}
	got, err := agent.splitTurnSummarize(context.Background(), provider, "model", prep)
	if err != nil {
		t.Fatalf("splitTurnSummarize: %v", err)
	}
	if !strings.Contains(got, "summarized") {
		t.Errorf("summary = %q, want it to contain the provider's summary text", got)
	}
	if calls := provider.callCount(); calls != 4 {
		t.Errorf("provider calls = %d, want 4 (2 concurrent calls, 1 retry each)", calls)
	}

	// Each side fires Scheduled, AttemptStart, Finished — order between the two
	// concurrent sides is interleaved, so count phases instead of comparing sequences.
	counts := map[SummarizationRetryPhase]int{}
	for _, p := range rec.phases() {
		counts[p]++
	}
	if counts[SummarizationRetryScheduled] != 2 || counts[SummarizationRetryAttemptStart] != 2 || counts[SummarizationRetryFinished] != 2 {
		t.Errorf("phase counts = %v, want 2 of each", counts)
	}
}
