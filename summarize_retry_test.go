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
