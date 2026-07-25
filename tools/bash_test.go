package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pi "github.com/dev-resolute/resolute-agent-core-go"
)

// bash_test.go exercises bash.go, the port of
// packages/agent/src/harness/tools/bash.ts @0.82.0.

// streamingTool locally mirrors the root package's unexported streamingTool
// capability interface (tool.go, Task 4) so this package's tests can drive
// NewBashTool's ExecuteStream path directly without a persisted transcript
// or a full agent loop. Go interfaces are satisfied structurally: since the
// method signature below is identical to pi's own (same imported
// pi.ToolResult type), any pi.RegisteredTool built from Tool.ExecuteStream
// (concretely *streamingTypedTool[P], unexported in package pi) satisfies
// this local interface too.
type streamingTool interface {
	ExecuteStream(ctx context.Context, callID string, args json.RawMessage, emit func(pi.ToolResult)) (pi.ToolResult, error)
}

func mustMarshalBashParams(t *testing.T, p bashParams) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("json.Marshal(bashParams) error: %v", err)
	}
	return raw
}

func TestNewBashToolNameAndDescription(t *testing.T) {
	env, _ := newTestOSEnv(t)
	tool := NewBashTool(BashToolOptions{Env: env})

	if got, want := tool.Name(), "bash"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}

	// Verbatim upstream bash.ts description with DEFAULT_MAX_LINES=2000 and
	// DEFAULT_MAX_BYTES/1024=50 interpolated.
	want := "Execute a bash command in the current working directory. Returns stdout and stderr. Output is truncated to last 2000 lines or 50KB (whichever is hit first). If truncated, full output is saved to a temp file. Optionally provide a timeout in seconds."
	if got := tool.Description(); got != want {
		t.Errorf("Description() = %q, want %q", got, want)
	}
}

func TestNewBashToolIsStreaming(t *testing.T) {
	env, _ := newTestOSEnv(t)
	tool := NewBashTool(BashToolOptions{Env: env})
	if _, ok := tool.(streamingTool); !ok {
		t.Fatal("bash tool must be registered via ExecuteStream (implement the streaming capability)")
	}
}

// runBash calls the bash tool's plain Execute entry point (partial updates
// discarded), for cases that only care about the final result.
func runBash(ctx context.Context, t *testing.T, opts BashToolOptions, p bashParams) pi.ToolResult {
	t.Helper()
	tool := NewBashTool(opts)
	result, err := tool.Execute(ctx, "c1", mustMarshalBashParams(t, p))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	return result
}

func TestBashToolEchoHi(t *testing.T) {
	env, _ := newTestOSEnv(t)
	result := runBash(context.Background(), t, BashToolOptions{Env: env}, bashParams{Command: "echo hi"})

	if result.IsError {
		t.Fatalf("IsError = true, Content = %q", result.Content)
	}
	if result.Content != "hi\n" {
		t.Errorf("Content = %q, want %q", result.Content, "hi\n")
	}
}

// TestBashToolZeroTimeoutMeansOmitted pins the zero-value tradeoff
// documented in bash.go: bashParams.Timeout is a plain float64, so an
// explicit 0 is indistinguishable from an omitted field at the JSON layer.
// This port treats both as "no timeout requested" rather than rejecting
// the explicit-0 case the way upstream's validateTimeout would - the same
// zero-means-default tradeoff truncate.go's TruncationOptions already
// makes for MaxLines/MaxBytes.
func TestBashToolZeroTimeoutMeansOmitted(t *testing.T) {
	env, _ := newTestOSEnv(t)
	result := runBash(context.Background(), t, BashToolOptions{Env: env}, bashParams{Command: "echo hi", Timeout: 0})

	if result.IsError {
		t.Fatalf("IsError = true, Content = %q, want a successful run (Timeout: 0 means \"not provided\")", result.Content)
	}
	if result.Content != "hi\n" {
		t.Errorf("Content = %q, want %q", result.Content, "hi\n")
	}
}

func TestBashToolEmptyOutput(t *testing.T) {
	env, _ := newTestOSEnv(t)
	result := runBash(context.Background(), t, BashToolOptions{Env: env}, bashParams{Command: "true"})

	if result.IsError {
		t.Fatalf("IsError = true, Content = %q", result.Content)
	}
	if result.Content != "(no output)" {
		t.Errorf("Content = %q, want %q", result.Content, "(no output)")
	}
}

func TestBashToolNonzeroExit(t *testing.T) {
	env, _ := newTestOSEnv(t)
	result := runBash(context.Background(), t, BashToolOptions{Env: env}, bashParams{Command: "echo hello; exit 7"})

	if !result.IsError {
		t.Fatalf("IsError = false, want true (non-zero exit), Content = %q", result.Content)
	}
	want := "hello\n\n\nCommand exited with code 7"
	if result.Content != want {
		t.Errorf("Content = %q, want %q", result.Content, want)
	}
}

func TestBashToolTimeout(t *testing.T) {
	env, _ := newTestOSEnv(t)

	resultCh := make(chan pi.ToolResult, 1)
	go func() {
		resultCh <- runBash(context.Background(), t, BashToolOptions{Env: env}, bashParams{
			Command: "echo partial; sleep 5",
			Timeout: 0.1,
		})
	}()

	select {
	case result := <-resultCh:
		if !result.IsError {
			t.Fatalf("IsError = false, want true (timeout), Content = %q", result.Content)
		}
		want := "partial\n\n\nCommand timed out after 0.1 seconds"
		if result.Content != want {
			t.Errorf("Content = %q, want %q", result.Content, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bash tool did not return within 5s of a 0.1s timeout")
	}
}

func TestBashToolContextCancel(t *testing.T) {
	env, _ := newTestOSEnv(t)
	ctx, cancel := context.WithCancel(context.Background())

	resultCh := make(chan pi.ToolResult, 1)
	go func() {
		resultCh <- runBash(ctx, t, BashToolOptions{Env: env}, bashParams{Command: "echo partial; sleep 5"})
	}()

	// Give the process a moment to actually start before cancelling, so
	// this exercises "kill a running process" rather than "never start it".
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case result := <-resultCh:
		if !result.IsError {
			t.Fatalf("IsError = false, want true (ctx cancelled), Content = %q", result.Content)
		}
		want := "partial\n\n\nCommand aborted"
		if result.Content != want {
			t.Errorf("Content = %q, want %q", result.Content, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bash tool did not return within 5s of ctx cancellation")
	}
}

func TestBashToolInvalidTimeoutTooSmallDoesNotExecute(t *testing.T) {
	env, dir := newTestOSEnv(t)
	marker := filepath.Join(dir, "marker.txt")

	result := runBash(context.Background(), t, BashToolOptions{Env: env}, bashParams{
		Command: "touch " + marker,
		Timeout: -1,
	})

	if !result.IsError {
		t.Fatalf("IsError = false, want true (invalid timeout), Content = %q", result.Content)
	}
	want := "Invalid timeout: must be a finite number of seconds"
	if result.Content != want {
		t.Errorf("Content = %q, want %q", result.Content, want)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("marker file exists, want the command to never have executed")
	}
}

func TestBashToolInvalidTimeoutTooLargeDoesNotExecute(t *testing.T) {
	env, dir := newTestOSEnv(t)
	marker := filepath.Join(dir, "marker.txt")

	result := runBash(context.Background(), t, BashToolOptions{Env: env}, bashParams{
		Command: "touch " + marker,
		Timeout: 2147483647.0/1000.0 + 1,
	})

	if !result.IsError {
		t.Fatalf("IsError = false, want true (invalid timeout), Content = %q", result.Content)
	}
	want := "Invalid timeout: maximum is 2147483.647 seconds"
	if result.Content != want {
		t.Errorf("Content = %q, want %q", result.Content, want)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("marker file exists, want the command to never have executed")
	}
}

func TestBashToolCommandPrefix(t *testing.T) {
	env, _ := newTestOSEnv(t)
	result := runBash(context.Background(), t, BashToolOptions{Env: env, CommandPrefix: "set -e"}, bashParams{
		Command: "false; echo should-not-print",
	})

	// "set -e" makes the script exit on `false` before ever reaching the
	// echo - proving the prefix actually ran ahead of (and joined by a
	// newline with) the model's command.
	if !result.IsError {
		t.Fatalf("IsError = false, want true (set -e trips on `false`), Content = %q", result.Content)
	}
	if strings.Contains(result.Content, "should-not-print") {
		t.Errorf("Content = %q, want it to NOT contain %q (set -e should have stopped before the echo)", result.Content, "should-not-print")
	}
	want := "Command exited with code 1"
	if !strings.HasSuffix(result.Content, want) {
		t.Errorf("Content = %q, want suffix %q", result.Content, want)
	}
}

func TestBashToolPrepareMutatesExecution(t *testing.T) {
	env, _ := newTestOSEnv(t)
	opts := BashToolOptions{
		Env: env,
		Prepare: func(ctx context.Context, exec *BashExecution) error {
			exec.Env["FOO"] = "bar"
			return nil
		},
	}
	result := runBash(context.Background(), t, opts, bashParams{Command: "echo $FOO"})

	if result.IsError {
		t.Fatalf("IsError = true, Content = %q", result.Content)
	}
	if result.Content != "bar\n" {
		t.Errorf("Content = %q, want %q", result.Content, "bar\n")
	}
}

func TestBashToolPrepareErrorAbortsWithoutExecuting(t *testing.T) {
	env, dir := newTestOSEnv(t)
	marker := filepath.Join(dir, "marker.txt")
	opts := BashToolOptions{
		Env: env,
		Prepare: func(ctx context.Context, exec *BashExecution) error {
			return errors.New("boom")
		},
	}
	result := runBash(context.Background(), t, opts, bashParams{Command: "touch " + marker})

	if !result.IsError {
		t.Fatalf("IsError = false, want true (Prepare error), Content = %q", result.Content)
	}
	if result.Content != "boom" {
		t.Errorf("Content = %q, want %q", result.Content, "boom")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("marker file exists, want the command to never have executed")
	}
}

func TestBashToolTruncationByLines(t *testing.T) {
	env, _ := newTestOSEnv(t)
	result := runBash(context.Background(), t, BashToolOptions{Env: env}, bashParams{Command: "seq 1 5000"})

	if result.IsError {
		t.Fatalf("IsError = true, Content = %q", result.Content)
	}

	wantNotice := "\n\n[Showing lines 3001-5000 of 5000. Full output: "
	idx := strings.Index(result.Content, wantNotice)
	if idx < 0 {
		t.Fatalf("Content does not contain expected notice prefix %q; Content tail = %q", wantNotice, lastN(result.Content, 200))
	}
	if !strings.HasSuffix(result.Content, "]") {
		t.Fatalf("Content = %q, want it to end with ']'", lastN(result.Content, 50))
	}

	path := result.Content[idx+len(wantNotice) : len(result.Content)-1]
	spilled, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading spill file %q: %v", path, err)
	}
	wantFull := seqLines(1, 5000)
	if string(spilled) != wantFull {
		t.Errorf("spill file does not contain all 5000 lines: got %d bytes, want %d bytes", len(spilled), len(wantFull))
	}
}

func TestBashToolTruncationByBytes(t *testing.T) {
	env, _ := newTestOSEnv(t)
	// 3000 lines of 100 'a' characters each - well over DefaultMaxBytes
	// (50KB) long before 3000 lines would ever cross DefaultMaxLines
	// (2000) on line count alone, so the byte limit (not the line limit)
	// must be what triggers truncation - exercising bash.ts's third notice
	// branch (byte-limited, not a single oversized line).
	const cmd = `line=$(printf '%*s' 100 '' | tr ' ' 'a'); for i in $(seq 1 3000); do echo "$line"; done`
	result := runBash(context.Background(), t, BashToolOptions{Env: env}, bashParams{Command: cmd})

	if result.IsError {
		t.Fatalf("IsError = true, Content = %q", result.Content)
	}

	wantNotice := "(" + FormatSize(DefaultMaxBytes) + " limit). Full output: "
	if !strings.Contains(result.Content, wantNotice) {
		t.Fatalf("Content does not contain expected notice fragment %q; Content tail = %q", wantNotice, lastN(result.Content, 200))
	}
	if !strings.Contains(result.Content, "[Showing lines ") {
		t.Errorf("Content = %q, want it to contain a \"[Showing lines ...\" notice", lastN(result.Content, 200))
	}
}

func TestBashToolHugeSingleLineTruncation(t *testing.T) {
	env, _ := newTestOSEnv(t)
	result := runBash(context.Background(), t, BashToolOptions{Env: env}, bashParams{
		Command: `head -c 60000 /dev/zero | tr '\0' 'a'`,
	})

	if result.IsError {
		t.Fatalf("IsError = true, Content = %q", result.Content)
	}

	// bash.ts's single-oversized-line notice: "last N of line L (line is
	// M). Full output: <path>" - N is the truncated (shown) size, M is the
	// line's true full size.
	wantPrefix := fmt.Sprintf("\n\n[Showing last %s of line 1 (line is %s). Full output: ", FormatSize(DefaultMaxBytes), FormatSize(60000))
	idx := strings.Index(result.Content, wantPrefix)
	if idx < 0 {
		t.Fatalf("Content does not contain expected notice prefix %q; Content tail = %q", wantPrefix, lastN(result.Content, 200))
	}
	if !strings.HasSuffix(result.Content, "]") {
		t.Fatalf("Content = %q, want it to end with ']'", lastN(result.Content, 50))
	}

	path := result.Content[idx+len(wantPrefix) : len(result.Content)-1]
	spilled, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading spill file %q: %v", path, err)
	}
	if len(spilled) != 60000 {
		t.Errorf("spill file length = %d, want 60000 (the full untruncated line)", len(spilled))
	}
}

// TestBashToolStreamingThrottledEmits drives ExecuteStream directly (via
// the streamingTool capability) with a command that emits one line every
// ~50ms for ~500ms, asserting: at least 2 partial emits land (proving
// throttled streaming actually happens, not just a single final emit), and
// that successive emits' Content only grows (proving each snapshot is
// cumulative, not a per-chunk delta). Deliberately timing-tolerant: no
// exact emit count is asserted, since the real 100ms throttle window means
// the exact number of emits depends on scheduling jitter.
func TestBashToolStreamingThrottledEmits(t *testing.T) {
	env, _ := newTestOSEnv(t)
	tool := NewBashTool(BashToolOptions{Env: env})
	st, ok := tool.(streamingTool)
	if !ok {
		t.Fatal("bash tool does not implement the streaming capability")
	}

	args := mustMarshalBashParams(t, bashParams{
		Command: `for i in $(seq 1 10); do echo "line $i"; sleep 0.05; done`,
	})

	type outcome struct {
		result    pi.ToolResult
		err       error
		snapshots []string
	}
	outcomeCh := make(chan outcome, 1)
	go func() {
		var mu sync.Mutex
		var snapshots []string
		result, err := st.ExecuteStream(context.Background(), "c1", args, func(r pi.ToolResult) {
			mu.Lock()
			snapshots = append(snapshots, r.Content)
			mu.Unlock()
		})
		outcomeCh <- outcome{result: result, err: err, snapshots: snapshots}
	}()

	select {
	case out := <-outcomeCh:
		if out.err != nil {
			t.Fatalf("ExecuteStream error: %v", out.err)
		}
		if out.result.IsError {
			t.Fatalf("result.IsError = true, Content = %q", out.result.Content)
		}
		if len(out.snapshots) < 2 {
			t.Fatalf("got %d streaming emits, want >= 2 (Content snapshots: %v)", len(out.snapshots), out.snapshots)
		}
		for i := 1; i < len(out.snapshots); i++ {
			if len(out.snapshots[i]) < len(out.snapshots[i-1]) {
				t.Errorf("snapshot %d shrank (%d bytes -> %d bytes): %q -> %q, want cumulative (non-decreasing) output",
					i, len(out.snapshots[i-1]), len(out.snapshots[i]), out.snapshots[i-1], out.snapshots[i])
			}
		}
		if !strings.Contains(out.result.Content, "line 10") {
			t.Errorf("final Content = %q, want it to contain %q", out.result.Content, "line 10")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ExecuteStream did not return within 10s")
	}
}
