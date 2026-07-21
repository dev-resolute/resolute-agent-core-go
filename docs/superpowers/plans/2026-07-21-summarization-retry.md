# Resilient Summarization (Compaction Retry) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port upstream pi 0.81.1 (pi#6901) — compaction and branch-summary LLM calls retry transient provider failures under a configured retry policy, with retry lifecycle reporting.

**Architecture:** Today `Agent.summarizeWithLLM` (`compact.go`) issues a single bare `provider.Stream`; one transient 429/5xx/network failure fails the whole `Compact()` call. All three summarization paths — first summary, summary update, and the split-turn pair — funnel through `summarizeWithLLM`, so it is the single choke point. The plan renames the current body to `summarizeOnce` and wraps it in a bounded retry loop with exponential backoff, driven by a new `AgentConfig.SummarizationRetry` policy (zero value = disabled, preserving current behavior). Retry lifecycle is delivered via a new `Hooks.OnSummarizationRetry` hook with scheduled / attempt-start / finished phases — the Go-shaped equivalent of upstream's `summarization_retry_*` RPC events. `Compact` has no EventStream, so a hook is the delivery path (ADR-0007 precedent).

**Design context (verified, not assumed):** `llm.RetryPolicy` in pi-llm-go is currently dead config — no provider consumes it and no provider emits `llm.LLMRetryEvent`; retries in the Go stack live in resolute-harness-go's submission ladder. This plan therefore keeps the retry loop inside pi-core-agent-go (single-repo change, no pi-llm-go release dependency). Classification uses the existing sentinel taxonomy: `llm.ErrProviderFatal` (LLM-11) and `llm.ErrContextOverflow` (LLM-8 — the harness's compact-and-retry ladder owns those) fail fast; caller cancellation is never retried; everything else (including per-request `DeadlineExceeded`) is transient.

**Tech Stack:** Go, `github.com/dev-resolute/resolute-agent-core-go` (package `pi`), depends on `github.com/dev-resolute/resolute-llm-go`.

**Repo:** `/Users/maikeffi/playground-ai/pi-research/pi-core-agent-go`. All commands run from this directory.

**Coding rules:** `docs/go-rules/golang.md` applies — table-driven tests (T-1), `-race` (T-2/G-3), `go vet` (G-1), ctx first param (CTX-1), wrap errors with `%w` (ERR-1), `errors.Is` for control flow (ERR-2), document exported items (API-1), goroutines tied to ctx (CC-2).

---

### Task 1: Retry policy type, backoff, transient classifier, ctx-aware sleep

**Files:**
- Create: `summarize_retry.go`
- Test: `summarize_retry_test.go`

- [ ] **Step 1: Write the failing test**

Create `summarize_retry_test.go`:

```go
package pi

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/dev-resolute/resolute-llm-go"
)

func TestSummarizationRetryBackoff(t *testing.T) {
	policy := SummarizationRetryPolicy{MaxRetries: 5, BaseDelay: 100 * time.Millisecond, MaxDelay: 250 * time.Millisecond}

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 100 * time.Millisecond},
		{2, 200 * time.Millisecond},
		{3, 250 * time.Millisecond}, // capped
		{4, 250 * time.Millisecond},
	}
	for _, tt := range tests {
		if got := policy.backoff(tt.attempt); got != tt.want {
			t.Errorf("backoff(%d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

func TestSummarizationRetryNormalizedDefaults(t *testing.T) {
	got := (SummarizationRetryPolicy{MaxRetries: 3}).normalized()
	if got.BaseDelay != DefaultSummarizationRetryBaseDelay {
		t.Errorf("BaseDelay = %v, want %v", got.BaseDelay, DefaultSummarizationRetryBaseDelay)
	}
	if got.MaxDelay != DefaultSummarizationRetryMaxDelay {
		t.Errorf("MaxDelay = %v, want %v", got.MaxDelay, DefaultSummarizationRetryMaxDelay)
	}
}

func TestIsTransientSummarizationError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"fatal sentinel", llm.ErrProviderFatal, false},
		{"wrapped fatal", fmt.Errorf("gemini: %w", llm.ErrProviderFatal), false},
		{"context overflow", llm.ErrContextOverflow, false},
		{"caller cancellation", context.Canceled, false},
		{"wrapped cancellation", fmt.Errorf("stream: %w", context.Canceled), false},
		{"network error", errors.New("connection reset by peer"), true},
		{"per-request timeout", context.DeadlineExceeded, true},
		{"wrapped transient", fmt.Errorf("stream: %w", errors.New("429 too many requests")), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientSummarizationError(tt.err); got != tt.want {
				t.Errorf("isTransientSummarizationError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestSleepCtx(t *testing.T) {
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Errorf("sleepCtx with live ctx: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("sleepCtx with cancelled ctx = %v, want context.Canceled", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run 'TestSummarizationRetry|TestIsTransient|TestSleepCtx' -v`
Expected: FAIL — `undefined: SummarizationRetryPolicy`

- [ ] **Step 3: Implement `summarize_retry.go`**

Create `summarize_retry.go`:

```go
package pi

import (
	"context"
	"errors"
	"time"

	"github.com/dev-resolute/resolute-llm-go"
)

// SummarizationRetryPolicy configures bounded retries with exponential backoff
// for the summarization calls made by Compact (first summary, summary update,
// and the split-turn pair). The zero value disables retries, matching upstream
// when no retry policy is configured. Ported from upstream 0.81.1 (resilient
// compaction and branch summaries, pi#6901).
type SummarizationRetryPolicy struct {
	// MaxRetries bounds retry attempts; the initial call never counts as a
	// retry. 0 disables retries.
	MaxRetries int
	// BaseDelay is the first retry delay; attempt n waits BaseDelay * 2^(n-1),
	// capped at MaxDelay. <= 0 uses DefaultSummarizationRetryBaseDelay.
	BaseDelay time.Duration
	// MaxDelay caps the per-attempt delay. <= 0 uses DefaultSummarizationRetryMaxDelay.
	MaxDelay time.Duration
}

// Defaults for SummarizationRetryPolicy zero fields.
const (
	DefaultSummarizationRetryBaseDelay = time.Second
	DefaultSummarizationRetryMaxDelay  = 60 * time.Second
)

// normalized fills zero delay fields with their defaults.
func (p SummarizationRetryPolicy) normalized() SummarizationRetryPolicy {
	if p.BaseDelay <= 0 {
		p.BaseDelay = DefaultSummarizationRetryBaseDelay
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = DefaultSummarizationRetryMaxDelay
	}
	return p
}

// backoff returns the delay before retry attempt n (1-indexed):
// BaseDelay * 2^(n-1), capped at MaxDelay. Matches upstream's
// baseDelayMs * 2^(attempt-1).
func (p SummarizationRetryPolicy) backoff(attempt int) time.Duration {
	d := p.BaseDelay
	for i := 1; i < attempt && d < p.MaxDelay; i++ {
		d *= 2
	}
	if d > p.MaxDelay {
		d = p.MaxDelay
	}
	return d
}

// isTransientSummarizationError reports whether a failed summarization call is
// worth retrying. Deterministic failures fail fast: fatal provider errors
// (llm.ErrProviderFatal, LLM-11), context overflow (llm.ErrContextOverflow —
// the harness's compact-and-retry ladder owns those, LLM-8), and caller
// cancellation. Per-request timeouts (context.DeadlineExceeded from a
// request-scoped deadline) stay transient.
func isTransientSummarizationError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, llm.ErrProviderFatal) ||
		errors.Is(err, llm.ErrContextOverflow) ||
		errors.Is(err, context.Canceled) {
		return false
	}
	return true
}

// sleepCtx waits d or returns the context's error, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test . -run 'TestSummarizationRetry|TestIsTransient|TestSleepCtx' -v -race`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add summarize_retry.go summarize_retry_test.go
git commit -m "feat: add summarization retry policy, backoff, and transient classifier"
```

---

### Task 2: Hook types and config field

**Files:**
- Modify: `hooks.go`
- Modify: `config.go`

No behavioral change yet — this task adds the types Task 3 wires up. Verified by build + vet.

- [ ] **Step 1: Add the hook, phase enum, and ctx struct to `hooks.go`**

In `hooks.go`, add `"time"` to the import block, then add the hook field to the `Hooks` struct, immediately after the `AfterProviderResponse` field:

```go
	// OnSummarizationRetry is called at each retry-lifecycle point when a
	// summarization call made by Compact fails transiently and
	// AgentConfig.SummarizationRetry allows a retry. It is never called when
	// the policy disables retries or the first call succeeds. It may be called
	// concurrently: split-turn summarization runs two retried calls in
	// parallel goroutines. It must not call back into the Agent. Nil is a
	// no-op.
	OnSummarizationRetry func(ctx context.Context, c SummarizationRetryCtx)
```

Then add the types at the end of `hooks.go`:

```go
// SummarizationRetryPhase identifies which point of the retry lifecycle an
// OnSummarizationRetry call reports. Mirrors upstream 0.81.1's
// summarization_retry_scheduled / _attempt_start / _finished events.
type SummarizationRetryPhase int

const (
	_ SummarizationRetryPhase = iota // zero value is invalid
	// SummarizationRetryScheduled fires before the backoff sleep of each retry.
	SummarizationRetryScheduled
	// SummarizationRetryAttemptStart fires after the backoff sleep, immediately
	// before the retried call starts.
	SummarizationRetryAttemptStart
	// SummarizationRetryFinished fires once when the retry loop ends, whether
	// it succeeded, exhausted its budget, or was aborted during backoff.
	SummarizationRetryFinished
)

// SummarizationRetryCtx describes one retry-lifecycle point of a failed
// summarization call (Compact's first summary, summary update, or split-turn
// pair).
type SummarizationRetryCtx struct {
	Phase SummarizationRetryPhase
	// Attempt is the 1-indexed retry attempt this point belongs to.
	Attempt int
	// MaxAttempts is the configured MaxRetries. Set on Scheduled.
	MaxAttempts int
	// Delay is the backoff about to be slept. Set on Scheduled.
	Delay time.Duration
	// Success reports the loop outcome. Set on Finished.
	Success bool
	// Err is the error that triggered this retry on Scheduled, and the final
	// error on an unsuccessful Finished. Nil on AttemptStart and on a
	// successful Finished.
	Err error
}
```

- [ ] **Step 2: Add the config field to `config.go`**

In `config.go`, add to the `AgentConfig` struct, immediately after the `KeepRecentTokens` field:

```go
	// SummarizationRetry configures bounded retries with exponential backoff
	// for the summarization calls made by Compact. The zero value disables
	// retries, matching pre-0.7.0 behavior. Retry lifecycle is reported
	// through Hooks.OnSummarizationRetry. Ported from upstream 0.81.1.
	SummarizationRetry SummarizationRetryPolicy
```

- [ ] **Step 3: Verify build and vet**

Run: `go build ./... && go vet ./...`
Expected: clean (no output, exit 0)

- [ ] **Step 4: Commit**

```bash
git add hooks.go config.go
git commit -m "feat: add OnSummarizationRetry hook and SummarizationRetry config field"
```

---

### Task 3: Retry loop in `summarizeWithLLM`

**Files:**
- Modify: `compact.go:458-477` (the `summarizeWithLLM` function)
- Test: `compact_retry_test.go` (create)

- [ ] **Step 1: Write the failing tests**

Create `compact_retry_test.go`:

```go
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
```

Note: `newInternalMemorySession` already exists in this package's test files (used by `compact_sessionid_test.go`) — reuse it, do not redefine.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test . -run 'TestCompactSummarization' -v`
Expected: FAIL — `TestCompactSummarizationRetrySucceedsAfterTransientFailures` fails with "Compact: summarization failed: 503 service unavailable" (no retry yet); the disabled/fatal/overflow tests may already pass (current behavior).

- [ ] **Step 3: Implement the retry loop**

In `compact.go`, replace the entire `summarizeWithLLM` function with:

```go
// summarizeWithLLM calls the provider to produce a summary from the given
// messages, retrying transient failures per AgentConfig.SummarizationRetry
// (upstream 0.81.1 parity). Retry lifecycle is reported through the
// OnSummarizationRetry hook; the hook never fires when the policy disables
// retries or the first call succeeds.
func (a *Agent) summarizeWithLLM(ctx context.Context, provider llm.LLMProvider, modelID string, msgs []Message) (string, error) {
	policy := a.config.SummarizationRetry.normalized()

	summary, err := a.summarizeOnce(ctx, provider, modelID, msgs)
	if err == nil || policy.MaxRetries == 0 || !isTransientSummarizationError(err) {
		return summary, err
	}

	for attempt := 1; ; attempt++ {
		delay := policy.backoff(attempt)
		a.notifySummarizationRetry(ctx, SummarizationRetryCtx{
			Phase:       SummarizationRetryScheduled,
			Attempt:     attempt,
			MaxAttempts: policy.MaxRetries,
			Delay:       delay,
			Err:         err,
		})
		if sleepErr := sleepCtx(ctx, delay); sleepErr != nil {
			a.notifySummarizationRetry(ctx, SummarizationRetryCtx{
				Phase:   SummarizationRetryFinished,
				Attempt: attempt,
				Err:     err,
			})
			return "", fmt.Errorf("summarization retry wait: %w", sleepErr)
		}
		a.notifySummarizationRetry(ctx, SummarizationRetryCtx{
			Phase:   SummarizationRetryAttemptStart,
			Attempt: attempt,
		})

		summary, err = a.summarizeOnce(ctx, provider, modelID, msgs)
		if err == nil {
			a.notifySummarizationRetry(ctx, SummarizationRetryCtx{
				Phase:   SummarizationRetryFinished,
				Attempt: attempt,
				Success: true,
			})
			return summary, nil
		}
		if attempt >= policy.MaxRetries || !isTransientSummarizationError(err) {
			a.notifySummarizationRetry(ctx, SummarizationRetryCtx{
				Phase:   SummarizationRetryFinished,
				Attempt: attempt,
				Err:     err,
			})
			return "", err
		}
	}
}

// summarizeOnce performs a single summarization call and collects the streamed
// text. Callers wanting retry behavior should call summarizeWithLLM.
func (a *Agent) summarizeOnce(ctx context.Context, provider llm.LLMProvider, modelID string, msgs []Message) (string, error) {
	llmMsgs := DefaultConvertToLLM(msgs)
	req := llm.LLMRequest{
		Model:    modelID,
		Messages: llmMsgs,
	}

	stream := provider.Stream(ctx, req)
	var summary strings.Builder
	for ev := range stream.Events {
		if td, ok := ev.(llm.TextDeltaEvent); ok {
			summary.WriteString(td.Delta)
		}
	}
	result := <-stream.Done
	if result.Err != nil {
		return "", result.Err
	}
	return strings.TrimSpace(summary.String()), nil
}

// notifySummarizationRetry fires the OnSummarizationRetry hook if configured.
func (a *Agent) notifySummarizationRetry(ctx context.Context, c SummarizationRetryCtx) {
	if a.hooks.OnSummarizationRetry != nil {
		a.hooks.OnSummarizationRetry(ctx, c)
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test . -run 'TestCompactSummarization' -v -race`
Expected: PASS — all 6 tests

- [ ] **Step 5: Commit**

```bash
git add compact.go compact_retry_test.go
git commit -m "feat: retry transient summarization failures per SummarizationRetry (upstream 0.81.1, pi#6901)"
```

---

### Task 4: Split-turn concurrent retry coverage

**Files:**
- Test: `compact_retry_test.go` (append)

The split-turn path runs two `summarizeWithLLM` calls concurrently; each must retry independently and the hook must tolerate concurrent firing.

- [ ] **Step 1: Write the failing test**

Append to `compact_retry_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it passes (implementation already exists)**

This test pins existing behavior from Task 3 rather than driving new code — the split-turn path already funnels through `summarizeWithLLM`. If it fails, the bug is in Task 3's implementation; fix there, not here.

Run: `go test . -run 'TestSplitTurnSummarizeRetriesBothCalls' -v -race`
Expected: PASS (`-race` is load-bearing here — it validates the hook's concurrent-use claim)

- [ ] **Step 3: Commit**

```bash
git add compact_retry_test.go
git commit -m "test: cover concurrent split-turn summarization retries"
```

---

### Task 5: Docs — CHANGELOG, CONTEXT, ADR-0003 addendum

**Files:**
- Modify: `CHANGELOG.md`
- Modify: `CONTEXT.md`
- Modify: `docs/adr/0003-compaction-gaps.md`

- [ ] **Step 1: Add the CHANGELOG entry**

Prepend to `CHANGELOG.md`, after the `# Changelog` heading:

```markdown
## [0.7.0] - 2026-07-21

### Added

- **Resilient summarization (port of upstream 0.81.1, pi#6901).** Compact's
  summarization calls (first summary, summary update, and the split-turn pair)
  now retry transient provider failures per the new
  `AgentConfig.SummarizationRetry` policy — bounded attempts with exponential
  backoff (`BaseDelay * 2^(attempt-1)`, capped at `MaxDelay`). Fatal provider
  errors (`llm.ErrProviderFatal`), context overflow (`llm.ErrContextOverflow`),
  and caller cancellation fail fast without retries. The zero policy disables
  retries, matching pre-0.7.0 behavior. Retry lifecycle is reported through
  the new `Hooks.OnSummarizationRetry` hook (scheduled / attempt-start /
  finished phases) — the Go-shaped equivalent of upstream's
  `summarization_retry_*` events; Compact has no EventStream, so a hook is the
  delivery path (ADR-0007 precedent). The hook may fire concurrently from the
  split-turn path's two goroutines.
```

- [ ] **Step 2: Update the CONTEXT.md glossary**

In `CONTEXT.md`, add to the hooks section (near the `OnConfigUpdate` entry):

```markdown
**OnSummarizationRetry**:
Optional `Hooks` field fired at each retry-lifecycle point (scheduled, attempt-start, finished) when a Compact summarization call fails transiently and `AgentConfig.SummarizationRetry` allows a retry. May fire concurrently from split-turn summarization's two goroutines. The Go-shaped equivalent of upstream 0.81.1's `summarization_retry_*` events — Compact has no EventStream, so a hook is the delivery path (ADR-0007 precedent).
_Avoid_: SummarizationRetryEvent (ours is a hook, not an AgentEvent)
```

And add near the compaction terms:

```markdown
**SummarizationRetryPolicy**:
`AgentConfig` field configuring bounded retries with exponential backoff (`BaseDelay * 2^(attempt-1)`, capped at `MaxDelay`) for Compact's summarization calls. Zero value disables retries. Ported from upstream 0.81.1.
_Avoid_: RetryConfig, CompactionRetry
```

- [ ] **Step 3: Add the ADR-0003 addendum**

Append to `docs/adr/0003-compaction-gaps.md`:

```markdown
## Addendum (2026-07-21)

Beyond the four gaps above: upstream 0.81.1 (pi#6901) made compaction and
branch summarization resilient to transient provider failures under the
configured retry policy, with retry lifecycle events. Ported in v0.7.0 as
`AgentConfig.SummarizationRetry` + `Hooks.OnSummarizationRetry` (hook instead
of events, per the ADR-0007 precedent — Compact has no EventStream). The four
gaps above are otherwise unchanged.
```

- [ ] **Step 4: Final gate**

Run: `go build ./... && go vet ./... && go test ./... -race`
Expected: clean build, all tests pass

- [ ] **Step 5: Commit**

```bash
git add CHANGELOG.md CONTEXT.md docs/adr/0003-compaction-gaps.md
git commit -m "docs: changelog, glossary, and ADR-0003 addendum for resilient summarization (0.7.0)"
```

---

## Self-Review Notes (completed by plan author)

- **Spec coverage:** Upstream 0.81.1 = (a) summarization follows configured retry policy → Tasks 1+3; (b) retry lifecycle events for consumers → Task 2 hook (Go-shaped, since Compact has no event stream); (c) all three summarization paths covered → single choke point `summarizeWithLLM` (verified: `summarize`, `splitTurnSummarize` ×2 all call it). Upstream's `onRetryFinished(success)` semantics preserved: hook never fires when the policy is disabled or the first call succeeds.
- **Follow-ups deliberately out of scope:** wiring `SummarizationRetry` in resolute-harness-go's agent construction (separate repo, one-line change once 0.7.0 is tagged); exporting a shared `llm.IsTransientError` classifier in pi-llm-go (candidate once a second consumer exists — the harness ladder currently has its own); upstream's quota/billing *string* patterns in the classifier (Go uses typed sentinels instead — providers are expected to wrap deterministic failures in `ErrProviderFatal`, the LLM-11 convention).
- **Type consistency:** `SummarizationRetryPolicy` / `normalized` / `backoff` / `isTransientSummarizationError` / `sleepCtx` / `SummarizationRetryCtx` / `SummarizationRetryPhase` (+ `…Scheduled` / `…AttemptStart` / `…Finished`) / `OnSummarizationRetry` / `notifySummarizationRetry` / `summarizeOnce` used identically across tasks.
- **Placeholders:** none — all code complete.
