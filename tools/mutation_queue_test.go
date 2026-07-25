package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pi "github.com/dev-resolute/resolute-agent-core-go"
)

// mutation_queue_test.go exercises mutation_queue.go, the port of
// packages/agent/src/harness/tools/file-mutation-queue.ts @0.82.0.

func TestMutationQueueKey(t *testing.T) {
	ctx := context.Background()

	t.Run("existing file resolves to its canonical (symlink-resolved) path", func(t *testing.T) {
		env, dir := newTestOSEnv(t)
		target := filepath.Join(dir, "target.txt")
		if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
			t.Fatalf("os.WriteFile error: %v", err)
		}
		link := filepath.Join(dir, "link.txt")
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("os.Symlink error: %v", err)
		}

		gotViaLink, err := mutationQueueKey(ctx, env, "link.txt")
		if err != nil {
			t.Fatalf("mutationQueueKey(link.txt) error: %v", err)
		}
		gotViaTarget, err := mutationQueueKey(ctx, env, "target.txt")
		if err != nil {
			t.Fatalf("mutationQueueKey(target.txt) error: %v", err)
		}
		if gotViaLink != gotViaTarget {
			t.Errorf("mutationQueueKey(link.txt) = %q, mutationQueueKey(target.txt) = %q, want equal - a mutation via the symlink must queue against the same key as one via the target", gotViaLink, gotViaTarget)
		}
	})

	t.Run("nonexistent file falls back to its absolute path", func(t *testing.T) {
		env, dir := newTestOSEnv(t)
		got, err := mutationQueueKey(ctx, env, "does-not-exist.txt")
		if err != nil {
			t.Fatalf("mutationQueueKey(missing) error: %v", err)
		}
		want := filepath.Join(dir, "does-not-exist.txt")
		if got != want {
			t.Errorf("mutationQueueKey(missing) = %q, want %q", got, want)
		}
	})
}

func TestWithFileMutationQueuePropagatesFnResultAndError(t *testing.T) {
	env, dir := newTestOSEnv(t)
	ctx := context.Background()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}

	t.Run("success", func(t *testing.T) {
		result, err := withFileMutationQueue(ctx, env, path, func() (pi.ToolResult, error) {
			return pi.ToolResult{Content: "done"}, nil
		})
		if err != nil {
			t.Fatalf("withFileMutationQueue error: %v", err)
		}
		if result.Content != "done" {
			t.Errorf("result.Content = %q, want %q", result.Content, "done")
		}
	})

	t.Run("error", func(t *testing.T) {
		wantErr := errors.New("boom")
		_, err := withFileMutationQueue(ctx, env, path, func() (pi.ToolResult, error) {
			return pi.ToolResult{}, wantErr
		})
		if !errors.Is(err, wantErr) {
			t.Errorf("withFileMutationQueue error = %v, want %v", err, wantErr)
		}
	})
}

// TestWithFileMutationQueueKeyFallsBackToAbsolutePathWhenNotFound pins the
// brief-mandated fallback: queuing a mutation against a path that doesn't
// exist yet (e.g. a write tool creating a new file) must not fail just
// because env.CanonicalPath can't resolve a nonexistent path.
func TestWithFileMutationQueueKeyFallsBackToAbsolutePathWhenNotFound(t *testing.T) {
	env, dir := newTestOSEnv(t)
	ctx := context.Background()
	path := filepath.Join(dir, "new-file.txt")

	result, err := withFileMutationQueue(ctx, env, path, func() (pi.ToolResult, error) {
		if err := env.WriteFile(ctx, path, []byte("created")); err != nil {
			return pi.ToolResult{}, err
		}
		return pi.ToolResult{Content: "ok"}, nil
	})
	if err != nil {
		t.Fatalf("withFileMutationQueue error: %v", err)
	}
	if result.Content != "ok" {
		t.Errorf("result.Content = %q, want %q", result.Content, "ok")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile error: %v", err)
	}
	if string(got) != "created" {
		t.Errorf("file content = %q, want %q", got, "created")
	}
}

// TestWithFileMutationQueueSerializesSamePath is the brief's core
// serialization proof: N=50 goroutines each read-modify-write the SAME file
// (append one line) through withFileMutationQueue. Run under -race (see
// go test -race ./tools/), this both proves the queue implementation itself
// has no data race and - via the final line count and a direct in-critical-
// section reentrancy guard - that mutations against the same file are
// never interleaved. Without correct serialization, concurrent
// read-then-write cycles would lose updates and the final file would have
// FEWER than 50 lines.
func TestWithFileMutationQueueSerializesSamePath(t *testing.T) {
	const n = 50

	env, dir := newTestOSEnv(t)
	ctx := context.Background()
	path := filepath.Join(dir, "shared.txt")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}

	var inCriticalSection int32 // reentrancy guard: must never exceed 1

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := withFileMutationQueue(ctx, env, path, func() (pi.ToolResult, error) {
				if atomic.AddInt32(&inCriticalSection, 1) != 1 {
					errs <- fmt.Errorf("goroutine %d observed inCriticalSection > 1 - two mutations ran concurrently against the same path", i)
				}
				defer atomic.AddInt32(&inCriticalSection, -1)

				data, err := env.ReadFile(ctx, path)
				if err != nil {
					return pi.ToolResult{}, fmt.Errorf("ReadFile: %w", err)
				}
				data = append(data, fmt.Appendf(nil, "line-%d\n", i)...)
				if err := env.WriteFile(ctx, path, data); err != nil {
					return pi.ToolResult{}, fmt.Errorf("WriteFile: %w", err)
				}
				return pi.ToolResult{}, nil
			})
			if err != nil {
				errs <- fmt.Errorf("goroutine %d: withFileMutationQueue error: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile error: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(content), "\n"), "\n")
	if len(lines) != n {
		t.Errorf("final file has %d lines, want exactly %d (lost updates indicate the queue failed to serialize same-path mutations)", len(lines), n)
	}
}

// TestWithFileMutationQueueDifferentPathsConcurrent proves the converse of
// the serialization test: two DIFFERENT paths must not block each other.
// It uses a sync barrier (channels), not timestamps, to deterministically
// hold path A's mutation open while proving path B's mutation completes
// without waiting for it.
func TestWithFileMutationQueueDifferentPathsConcurrent(t *testing.T) {
	env, dir := newTestOSEnv(t)
	ctx := context.Background()

	pathA := filepath.Join(dir, "a.txt")
	pathB := filepath.Join(dir, "b.txt")
	for _, p := range []string{pathA, pathB} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatalf("os.WriteFile(%q) error: %v", p, err)
		}
	}

	aStarted := make(chan struct{})
	releaseA := make(chan struct{})
	aDone := make(chan struct{})

	go func() {
		defer close(aDone)
		_, err := withFileMutationQueue(ctx, env, pathA, func() (pi.ToolResult, error) {
			close(aStarted)
			<-releaseA
			return pi.ToolResult{}, nil
		})
		if err != nil {
			t.Errorf("withFileMutationQueue(pathA) error: %v", err)
		}
	}()

	select {
	case <-aStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("path A's mutation never started")
	}

	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		_, err := withFileMutationQueue(ctx, env, pathB, func() (pi.ToolResult, error) {
			return pi.ToolResult{}, nil
		})
		if err != nil {
			t.Errorf("withFileMutationQueue(pathB) error: %v", err)
		}
	}()

	// B targets a different path than A, so it must complete promptly even
	// though A's mutation is still blocked (holding its lock, waiting on
	// releaseA). The bound here is generous slack for scheduling, not a
	// pinned exact duration - see env_test.go for the same idiom.
	select {
	case <-bDone:
	case <-time.After(2 * time.Second):
		t.Fatal("path B's mutation did not complete promptly while path A's mutation was held open - cross-path blocking bug")
	}

	// Confirm A genuinely was still blocked when B finished, so the above
	// is proof of concurrency and not a lucky ordering.
	select {
	case <-aDone:
		t.Fatal("path A's mutation completed before being released via releaseA - test setup issue, the concurrency assertion above is not meaningful")
	default:
	}

	close(releaseA)
	select {
	case <-aDone:
	case <-time.After(2 * time.Second):
		t.Fatal("path A's mutation did not complete after being released")
	}
}
