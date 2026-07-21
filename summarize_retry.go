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
