package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// shell_output_test.go drives ExecuteShellWithCapture over a real
// NewOSEnv(t.TempDir()), per the task brief's Step 1 test list (no
// dedicated shell-output.test.ts exists upstream; the closest upstream
// evidence is nodejs-env.test.ts's "captures large shell output to a full
// output file through the execution env" case, which this file's
// TestExecuteShellWithCaptureSeq5000LinesSpillsFullOutput generalizes with
// exact byte-for-byte assertions instead of loose bounds). Timing-sensitive
// cases (timeout, cancellation) use generous margins and a select-based
// worst-case bound, matching env_test.go's convention, so they stay stable
// under -race on a loaded machine.

// seqLines returns "from\nfrom+1\n...to\n", matching the stdout of
// `seq from to`.
func seqLines(from, to int) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "%d\n", i)
	}
	return b.String()
}

func TestExecuteShellWithCaptureSmallOutput(t *testing.T) {
	env, _ := newTestOSEnv(t)
	ctx := context.Background()

	capture, err := ExecuteShellWithCapture(ctx, env, "echo hello", ShellCaptureOptions{})
	if err != nil {
		t.Fatalf("ExecuteShellWithCapture error: %v", err)
	}
	if capture.Output != "hello\n" {
		t.Errorf("Output = %q, want %q", capture.Output, "hello\n")
	}
	if capture.Truncation.Truncated {
		t.Errorf("Truncation.Truncated = true, want false")
	}
	if capture.FullOutputPath != "" {
		t.Errorf("FullOutputPath = %q, want empty (no spill for small output)", capture.FullOutputPath)
	}
	if capture.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", capture.ExitCode)
	}
	if capture.Cancelled || capture.TimedOut {
		t.Errorf("Cancelled=%v TimedOut=%v, want both false", capture.Cancelled, capture.TimedOut)
	}
}

func TestExecuteShellWithCaptureSeq5000LinesSpillsFullOutput(t *testing.T) {
	env, _ := newTestOSEnv(t)
	ctx := context.Background()

	capture, err := ExecuteShellWithCapture(ctx, env, "seq 1 5000", ShellCaptureOptions{})
	if err != nil {
		t.Fatalf("ExecuteShellWithCapture error: %v", err)
	}
	if capture.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", capture.ExitCode)
	}
	if !capture.Truncation.Truncated {
		t.Fatalf("Truncation.Truncated = false, want true")
	}
	if capture.Truncation.TruncatedBy != "lines" {
		t.Errorf("Truncation.TruncatedBy = %q, want %q", capture.Truncation.TruncatedBy, "lines")
	}
	if capture.Truncation.TotalLines != 5000 {
		t.Errorf("Truncation.TotalLines = %d, want 5000 (whole-stream count, not buffer)", capture.Truncation.TotalLines)
	}

	// DefaultMaxLines is 2000: Output must hold the LAST 2000 lines,
	// 3001..5000. TruncateTail joins lines with "\n" and does not add a
	// trailing newline after the last one (see truncate.go), so the
	// trailing "\n" that seqLines would add after "5000" is trimmed here.
	wantOutput := strings.TrimSuffix(seqLines(3001, 5000), "\n")
	if capture.Output != wantOutput {
		t.Errorf("Output does not hold the last 2000 lines: got %d bytes, want %d bytes (first got line: %q, first want line: %q)",
			len(capture.Output), len(wantOutput), firstLine(capture.Output), firstLine(wantOutput))
	}

	if capture.FullOutputPath == "" {
		t.Fatal("FullOutputPath = empty, want non-empty (output overflowed, so it must have been spilled)")
	}
	spilled, err := os.ReadFile(capture.FullOutputPath)
	if err != nil {
		t.Fatalf("reading spill file %q: %v", capture.FullOutputPath, err)
	}
	wantFull := seqLines(1, 5000)
	if string(spilled) != wantFull {
		t.Errorf("spill file does not contain all 5000 lines: got %d bytes, want %d bytes", len(spilled), len(wantFull))
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func TestExecuteShellWithCaptureByteLimitTruncatesByBytes(t *testing.T) {
	env, _ := newTestOSEnv(t)
	ctx := context.Background()

	// 3000 lines of 100 'a' characters (101 bytes/line with the newline) =
	// ~303KB total, comfortably over DefaultMaxBytes (50KB) long before
	// 3000 lines would ever cross DefaultMaxLines (2000) on line count
	// alone - the byte limit must be what triggers truncation.
	const cmd = `line=$(printf '%*s' 100 '' | tr ' ' 'a'); for i in $(seq 1 3000); do echo "$line"; done`
	capture, err := ExecuteShellWithCapture(ctx, env, cmd, ShellCaptureOptions{})
	if err != nil {
		t.Fatalf("ExecuteShellWithCapture error: %v", err)
	}
	if capture.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", capture.ExitCode)
	}
	if !capture.Truncation.Truncated {
		t.Fatalf("Truncation.Truncated = false, want true")
	}
	if capture.Truncation.TruncatedBy != "bytes" {
		t.Errorf("Truncation.TruncatedBy = %q, want %q", capture.Truncation.TruncatedBy, "bytes")
	}
	if capture.Truncation.OutputBytes > DefaultMaxBytes {
		t.Errorf("Truncation.OutputBytes = %d, want <= DefaultMaxBytes (%d)", capture.Truncation.OutputBytes, DefaultMaxBytes)
	}
}

func TestExecuteShellWithCaptureSingleHugeLine(t *testing.T) {
	env, _ := newTestOSEnv(t)
	ctx := context.Background()

	capture, err := ExecuteShellWithCapture(ctx, env, `head -c 200000 /dev/zero | tr '\0' 'x'`, ShellCaptureOptions{})
	if err != nil {
		t.Fatalf("ExecuteShellWithCapture error: %v", err)
	}
	if capture.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", capture.ExitCode)
	}
	if !capture.Truncation.LastLinePartial {
		t.Errorf("Truncation.LastLinePartial = false, want true")
	}
	if capture.LastLineBytes != 200000 {
		t.Errorf("LastLineBytes = %d, want 200000 (tracked against the whole stream, not the trimmed tail buffer)", capture.LastLineBytes)
	}
}

func TestExecuteShellWithCaptureExitCode(t *testing.T) {
	env, _ := newTestOSEnv(t)
	ctx := context.Background()

	capture, err := ExecuteShellWithCapture(ctx, env, "echo hello; exit 7", ShellCaptureOptions{})
	if err != nil {
		t.Fatalf("ExecuteShellWithCapture error: %v", err)
	}
	if capture.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", capture.ExitCode)
	}
	if !strings.Contains(capture.Output, "hello") {
		t.Errorf("Output = %q, want it to contain %q (output intact despite non-zero exit)", capture.Output, "hello")
	}
	if capture.Cancelled || capture.TimedOut {
		t.Errorf("Cancelled=%v TimedOut=%v, want both false", capture.Cancelled, capture.TimedOut)
	}
}

func TestExecuteShellWithCaptureTimeout(t *testing.T) {
	env, _ := newTestOSEnv(t)
	ctx := context.Background()

	start := time.Now()
	captureCh := make(chan *ShellCapture, 1)
	errCh := make(chan error, 1)
	go func() {
		capture, err := ExecuteShellWithCapture(ctx, env, "echo partial; sleep 5", ShellCaptureOptions{
			Timeout: 100 * time.Millisecond,
		})
		if err != nil {
			errCh <- err
			return
		}
		captureCh <- capture
	}()

	select {
	case err := <-errCh:
		t.Fatalf("ExecuteShellWithCapture error: %v", err)
	case capture := <-captureCh:
		elapsed := time.Since(start)
		if !capture.TimedOut {
			t.Errorf("TimedOut = false, want true")
		}
		if capture.Cancelled {
			t.Errorf("Cancelled = true, want false")
		}
		if !strings.Contains(capture.Output, "partial") {
			t.Errorf("Output = %q, want it to contain %q (partial output retained)", capture.Output, "partial")
		}
		// Generous margin: proves the timeout fires promptly without
		// pinning an exact duration that could flake on a loaded machine.
		if elapsed > 2*time.Second {
			t.Errorf("ExecuteShellWithCapture with 100ms timeout returned after %v, want < 2s", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ExecuteShellWithCapture did not return within 5s of a 100ms timeout")
	}
}

func TestExecuteShellWithCaptureCancelledContext(t *testing.T) {
	env, _ := newTestOSEnv(t)
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	captureCh := make(chan *ShellCapture, 1)
	errCh := make(chan error, 1)
	go func() {
		capture, err := ExecuteShellWithCapture(cctx, env, "echo partial; sleep 5", ShellCaptureOptions{})
		if err != nil {
			errCh <- err
			return
		}
		captureCh <- capture
	}()

	// Give the process a moment to actually start before cancelling, so
	// this exercises "kill a running process" rather than "never start it".
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		t.Fatalf("ExecuteShellWithCapture error: %v", err)
	case capture := <-captureCh:
		elapsed := time.Since(start)
		if !capture.Cancelled {
			t.Errorf("Cancelled = false, want true")
		}
		if capture.TimedOut {
			t.Errorf("TimedOut = true, want false")
		}
		if !strings.Contains(capture.Output, "partial") {
			t.Errorf("Output = %q, want it to contain %q (partial output retained)", capture.Output, "partial")
		}
		if elapsed > 2*time.Second {
			t.Errorf("ExecuteShellWithCapture with cancelled ctx returned after %v, want < 2s", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ExecuteShellWithCapture did not return within 5s of ctx cancellation")
	}
}

func TestExecuteShellWithCaptureOnChunkProgress(t *testing.T) {
	env, _ := newTestOSEnv(t)
	ctx := context.Background()

	var mu sync.Mutex
	var totalBytesSeen []int
	var totalLinesSeen []int
	var lastProgress func() ShellProgress
	onChunkCalls := 0

	capture, err := ExecuteShellWithCapture(ctx, env, "seq 1 5000", ShellCaptureOptions{
		OnChunk: func(chunk []byte, progress func() ShellProgress) {
			p := progress()
			mu.Lock()
			defer mu.Unlock()
			onChunkCalls++
			totalBytesSeen = append(totalBytesSeen, p.Truncation.TotalBytes)
			totalLinesSeen = append(totalLinesSeen, p.Truncation.TotalLines)
			lastProgress = progress
		},
	})
	if err != nil {
		t.Fatalf("ExecuteShellWithCapture error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if onChunkCalls == 0 {
		t.Fatal("OnChunk was never called")
	}
	for i := 1; i < len(totalBytesSeen); i++ {
		if totalBytesSeen[i] < totalBytesSeen[i-1] {
			t.Errorf("progress().Truncation.TotalBytes not monotonic: snapshot %d = %d < snapshot %d = %d",
				i, totalBytesSeen[i], i-1, totalBytesSeen[i-1])
		}
		if totalLinesSeen[i] < totalLinesSeen[i-1] {
			t.Errorf("progress().Truncation.TotalLines not monotonic: snapshot %d = %d < snapshot %d = %d",
				i, totalLinesSeen[i], i-1, totalLinesSeen[i-1])
		}
	}
	if totalBytesSeen[len(totalBytesSeen)-1] != capture.Truncation.TotalBytes {
		t.Errorf("last OnChunk progress TotalBytes = %d, want it to match final capture.Truncation.TotalBytes = %d",
			totalBytesSeen[len(totalBytesSeen)-1], capture.Truncation.TotalBytes)
	}

	if lastProgress == nil {
		t.Fatal("no progress func was captured")
	}
	// progress() must remain callable (and correct) after
	// ExecuteShellWithCapture has already returned.
	final := lastProgress()
	if final.Truncation.TotalLines != 5000 {
		t.Errorf("post-return progress().Truncation.TotalLines = %d, want 5000", final.Truncation.TotalLines)
	}
	if final.Truncation.TotalBytes != capture.Truncation.TotalBytes {
		t.Errorf("post-return progress().Truncation.TotalBytes = %d, want %d (== final capture)",
			final.Truncation.TotalBytes, capture.Truncation.TotalBytes)
	}
}
