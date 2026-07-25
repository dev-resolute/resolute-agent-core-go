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
// packages/agent/src/harness/tools/bash.ts @0.82.0. Every case in upstream's
// test/harness/tools.test.ts "bash" describe block is ported here except
// "ignores output callbacks after execution settles", which is deliberately
// NOT ported: that case proves executeShellWithCapture's `acceptingOutput`
// flag (shell-output.ts) drops an onStdout/onStderr call that a
// pathologically-implemented ExecutionEnv fires asynchronously, after its
// exec() call has already resolved. This port's real ExecutionEnv (OSEnv,
// env_unix.go) cannot produce that race structurally - Exec blocks on
// cmd.Wait, which only returns once every OnChunk call from the output-
// pumping goroutines has already completed (see ExecuteShellWithCapture's
// own doc comment in shell_output.go for the same reasoning applied to a
// related upstream safety-net check) - so there is no equivalent guard in
// this port's shellCaptureState/bashThrottle. A synthetic ExecutionEnv that
// fires OnChunk from a goroutine after Exec returns (mirroring upstream's
// LateOutputExecutionEnv) was drafted to check this port's behavior in that
// scenario and found a real gap: bashThrottle.scheduleLocked has no
// "settled" guard, so such a late call would leak into a caller's update
// stream and leave an unstoppable timer running past ExecuteStream's
// return - a latent bug in an ExecutionEnv implementation this port does
// not ship (no adapter here can trigger it), reachable only by a
// contract-violating adapter. Fixing bash.go/shell_output.go is outside
// this task's declared file scope (tools/*_test.go, docs) and was not
// silently done here; flagged in the task-13 report as a follow-up for
// whoever builds the first remote/sandbox ExecutionEnv adapter this port's
// ADR-0011 anticipates.

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

// float64Ptr is a small test-local convenience for constructing
// bashParams.Timeout literals: bashParams.Timeout is *float64 (not a plain
// float64) specifically so an explicit 0 can be told apart from an omitted
// field - see bash.go's package comment.
func float64Ptr(v float64) *float64 { return &v }

// bashDetailsForTest mirrors the subset of bash.go's bashToolDetails JSON
// shape these tests need to assert on (ToolResult.Data's wire format).
type bashDetailsForTest struct {
	Truncation *struct {
		Truncated   bool   `json:"truncated"`
		TruncatedBy string `json:"truncatedBy"`
		TotalLines  int    `json:"totalLines"`
	} `json:"truncation"`
	FullOutputPath string `json:"fullOutputPath"`
}

// mustUnmarshalBashDetails decodes result.Data as bashDetailsForTest,
// failing the test on any decode error - result.Data is expected to always
// be valid JSON produced by bash.go's own marshalBashDetails.
func mustUnmarshalBashDetails(t *testing.T, data json.RawMessage) bashDetailsForTest {
	t.Helper()
	var got bashDetailsForTest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal(Data) error: %v, Data = %s", err, data)
	}
	return got
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

// TestNewBashToolSchema verifies bashParams.Timeout's jsonschema
// description survives the float64 -> *float64 change (a pointer field),
// mirroring TestReadToolSchema's verification of the same
// invopop/jsonschema behavior for readParams.Limit (Task 10).
func TestNewBashToolSchema(t *testing.T) {
	env, _ := newTestOSEnv(t)
	tool := NewBashTool(BashToolOptions{Env: env})

	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatalf("json.Unmarshal(schema) error: %v", err)
	}

	tests := []struct {
		prop string
		want string
	}{
		{"command", "Bash command to execute"},
		{"timeout", "Timeout in seconds (optional, no default timeout)"},
	}
	for _, tc := range tests {
		got, ok := schema.Properties[tc.prop]
		if !ok {
			t.Errorf("schema properties missing %q", tc.prop)
			continue
		}
		if got.Description != tc.want {
			t.Errorf("schema properties[%q].description = %q, want %q", tc.prop, got.Description, tc.want)
		}
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
	if result.Data != nil {
		t.Errorf("Data = %s, want nil (no truncation occurred)", result.Data)
	}
}

// TestBashToolCombinesStdoutAndStderr ports upstream's "executes commands
// and combines stdout and stderr": stdout and stderr must both reach
// Content, merged into a single stream (shell_output.go's chunkWriter,
// env_unix.go's *OSEnv.Exec - both cmd.Stdout and cmd.Stderr write through
// the same writer).
func TestBashToolCombinesStdoutAndStderr(t *testing.T) {
	env, _ := newTestOSEnv(t)
	result := runBash(context.Background(), t, BashToolOptions{Env: env}, bashParams{
		Command: "printf out; printf err >&2",
	})

	if result.IsError {
		t.Fatalf("IsError = true, Content = %q", result.Content)
	}
	if !strings.Contains(result.Content, "out") {
		t.Errorf("Content = %q, want it to contain %q", result.Content, "out")
	}
	if !strings.Contains(result.Content, "err") {
		t.Errorf("Content = %q, want it to contain %q", result.Content, "err")
	}
}

// TestBashToolTimeoutPreservesTruncatedOutput ports upstream's "preserves
// truncated output when a command times out": a command producing enough
// output to overflow (and spill to a full-output file) BEFORE it times out
// must still report the "Full output: <path>" notice in its error Content,
// and that spill file must hold everything the command produced up to the
// kill, not just the truncated tail.
//
// Deviation from upstream's exact fixture: upstream uses a 0.05s timeout;
// this port uses 0.3s for the same command (3000 lines then a long sleep)
// to give the line-generating loop comfortable headroom to finish before
// the kill fires on a loaded CI machine, without changing the property
// under test (truncated output survives a timeout).
func TestBashToolTimeoutPreservesTruncatedOutput(t *testing.T) {
	env, _ := newTestOSEnv(t)

	resultCh := make(chan pi.ToolResult, 1)
	go func() {
		resultCh <- runBash(context.Background(), t, BashToolOptions{Env: env}, bashParams{
			Command: "i=1; while [ $i -le 3000 ]; do echo line-$i; i=$((i + 1)); done; sleep 2",
			Timeout: float64Ptr(0.3),
		})
	}()

	select {
	case result := <-resultCh:
		if !result.IsError {
			t.Fatalf("IsError = false, want true (timeout), Content tail = %q", lastN(result.Content, 200))
		}
		if !strings.Contains(result.Content, "Command timed out after 0.3 seconds") {
			t.Fatalf("Content does not contain the timeout status; tail = %q", lastN(result.Content, 200))
		}
		wantPrefix := "Full output: "
		idx := strings.Index(result.Content, wantPrefix)
		if idx < 0 {
			t.Fatalf("Content does not contain a %q notice (output should have overflowed and spilled); Content tail = %q", wantPrefix, lastN(result.Content, 200))
		}
		afterPrefix := result.Content[idx+len(wantPrefix):]
		closeIdx := strings.IndexByte(afterPrefix, ']')
		if closeIdx < 0 {
			t.Fatalf("no closing ']' found after %q in Content tail = %q", wantPrefix, lastN(result.Content, 200))
		}
		path := afterPrefix[:closeIdx]

		spilled, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading spill file %q: %v", path, err)
		}
		full := string(spilled)
		if !strings.Contains(full, "line-1\nline-2") {
			t.Errorf("spill file does not contain %q; got %d bytes", "line-1\nline-2", len(full))
		}
		if !strings.Contains(full, "line-2999\nline-3000") {
			t.Errorf("spill file does not contain %q; got %d bytes", "line-2999\nline-3000", len(full))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("bash tool did not return within 10s of a 0.3s timeout")
	}
}

// TestBashToolOmittedTimeoutRunsWithNoTimeout pins the nil (omitted) half
// of the pointer-based fix documented in bash.go: bashParams{} with no
// Timeout set marshals to JSON with the "timeout" key entirely absent
// (omitempty on a nil pointer), unmarshals back to a nil pointer, and
// validateBashTimeout treats nil as "no timeout enforced" - matching
// upstream's `timeout === undefined` early return. See
// TestBashToolExplicitZeroTimeoutIsRejected for the other half: an
// explicit 0, now correctly rejected instead of collapsing to this case.
func TestBashToolOmittedTimeoutRunsWithNoTimeout(t *testing.T) {
	env, _ := newTestOSEnv(t)
	result := runBash(context.Background(), t, BashToolOptions{Env: env}, bashParams{Command: "echo hi"})

	if result.IsError {
		t.Fatalf("IsError = true, Content = %q, want a successful run (omitted Timeout means no timeout enforced)", result.Content)
	}
	if result.Content != "hi\n" {
		t.Errorf("Content = %q, want %q", result.Content, "hi\n")
	}
}

// TestBashToolExplicitZeroTimeoutIsRejected pins the fix for the parity gap
// flagged in code review: bashParams.Timeout is now *float64 (mirroring
// read.go's readParams.Limit, Task 10) specifically so an explicit
// `timeout: 0` - which upstream rejects via `timeout <= 0` - can be told
// apart from an omitted field and validated the same way upstream does,
// instead of silently collapsing to "no timeout" like the prior plain-
// float64 field did.
func TestBashToolExplicitZeroTimeoutIsRejected(t *testing.T) {
	env, dir := newTestOSEnv(t)
	marker := filepath.Join(dir, "marker.txt")

	result := runBash(context.Background(), t, BashToolOptions{Env: env}, bashParams{
		Command: "touch " + marker,
		Timeout: float64Ptr(0),
	})

	if !result.IsError {
		t.Fatalf("IsError = false, want true (explicit timeout: 0 is invalid), Content = %q", result.Content)
	}
	want := "Invalid timeout: must be a finite number of seconds"
	if result.Content != want {
		t.Errorf("Content = %q, want %q", result.Content, want)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("marker file exists, want the command to never have executed")
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
			Timeout: float64Ptr(0.1),
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
		Timeout: float64Ptr(-1),
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
		Timeout: float64Ptr(2147483647.0/1000.0 + 1),
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

// TestBashToolPrepareMutatesCwdAndInheritEnv completes upstream's "prepares
// command, cwd, and an explicit environment with the turn context":
// TestBashToolPrepareMutatesExecution above already pins Prepare mutating
// exec.Env; this pins the other two axes upstream's fixture exercises -
// exec.Cwd (the command must actually run in the new directory) and
// exec.InheritEnv = false (an ambient, inherited variable must NOT reach
// the command, while an explicit exec.Env variable still does).
//
// Adapted, not ported verbatim: upstream's Prepare also receives an
// arbitrary caller-supplied "turn context" object and asserts it (and the
// AbortSignal) flow through unchanged. This port's BashToolOptions.Prepare
// has no per-call context parameter - every built-in tool in this port
// closes over its ExecutionEnv via options rather than accepting one per
// call (see BashToolOptions/EditToolOptions/ReadToolOptions/
// WriteToolOptions, Tasks 10-12) - so that half of upstream's assertion has
// no Go analog to port.
func TestBashToolPrepareMutatesCwdAndInheritEnv(t *testing.T) {
	env, dir := newTestOSEnv(t)
	workspace := filepath.Join(dir, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatalf("os.Mkdir error: %v", err)
	}
	realWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks error: %v", err)
	}
	t.Setenv("BASH_TEST_PREPARE_INHERITED", "should-not-appear")

	opts := BashToolOptions{
		Env: env,
		Prepare: func(ctx context.Context, exec *BashExecution) error {
			exec.Cwd = workspace
			exec.Env = map[string]string{"BASH_TEST_PREPARE_EXPLICIT": "explicit"}
			exec.InheritEnv = false
			return nil
		},
	}
	result := runBash(context.Background(), t, opts, bashParams{
		Command: `printf '%s:%s:%s' "$PWD" "${BASH_TEST_PREPARE_INHERITED-}" "$BASH_TEST_PREPARE_EXPLICIT"`,
	})

	if result.IsError {
		t.Fatalf("IsError = true, Content = %q", result.Content)
	}
	want := fmt.Sprintf("%s::explicit", realWorkspace)
	if result.Content != want {
		t.Errorf("Content = %q, want %q", result.Content, want)
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

	if result.Data == nil {
		t.Fatal("Data = nil, want a truncation+fullOutputPath payload (truncation occurred)")
	}
	details := mustUnmarshalBashDetails(t, result.Data)
	if details.Truncation == nil || !details.Truncation.Truncated {
		t.Errorf("Data.truncation = %+v, want truncated=true", details.Truncation)
	} else {
		if details.Truncation.TruncatedBy != "lines" {
			t.Errorf("Data.truncation.truncatedBy = %q, want %q", details.Truncation.TruncatedBy, "lines")
		}
		if details.Truncation.TotalLines != 5000 {
			t.Errorf("Data.truncation.totalLines = %d, want 5000", details.Truncation.TotalLines)
		}
	}
	if details.FullOutputPath != path {
		t.Errorf("Data.fullOutputPath = %q, want %q (matching the Content notice)", details.FullOutputPath, path)
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

	if result.Data == nil {
		t.Fatal("Data = nil, want a truncation+fullOutputPath payload (truncation occurred)")
	}
	details := mustUnmarshalBashDetails(t, result.Data)
	if details.Truncation == nil || !details.Truncation.Truncated {
		t.Errorf("Data.truncation = %+v, want truncated=true", details.Truncation)
	} else if details.Truncation.TruncatedBy != "bytes" {
		t.Errorf("Data.truncation.truncatedBy = %q, want %q", details.Truncation.TruncatedBy, "bytes")
	}
	if details.FullOutputPath == "" {
		t.Error("Data.fullOutputPath = \"\", want non-empty (output overflowed, so it must have been spilled)")
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

	if result.Data == nil {
		t.Fatal("Data = nil, want a truncation+fullOutputPath payload (truncation occurred)")
	}
	details := mustUnmarshalBashDetails(t, result.Data)
	if details.Truncation == nil || !details.Truncation.Truncated {
		t.Errorf("Data.truncation = %+v, want truncated=true", details.Truncation)
	}
	if details.FullOutputPath != path {
		t.Errorf("Data.fullOutputPath = %q, want %q (matching the Content notice)", details.FullOutputPath, path)
	}
}

// TestBashToolStreamingThrottledEmits drives ExecuteStream directly (via
// the streamingTool capability) with a command that bursts 250 lines then
// sleeps ~50ms, ten times over (2500 lines total, comfortably over
// DefaultMaxLines=2000, spread across ~500ms), asserting: at least 2
// partial emits land (proving throttled streaming actually happens, not
// just a single final emit); that successive emits' Content only grows
// (proving each snapshot is cumulative, not a per-chunk delta); and that
// the LAST snapshot - the throttle's finalFlush, deterministically the
// tail of the slice regardless of scheduling jitter (bash.go's
// bashThrottle.finalFlush always runs exactly once, strictly after every
// onChunk-driven emit) - carries a Data payload whose truncation/
// fullOutputPath reflect the overflow. Deliberately timing-tolerant
// otherwise: no exact emit count, and no assertion pinning which
// *particular* earlier snapshot first saw truncation, since the exact
// number and timing of emits depends on scheduling jitter.
func TestBashToolStreamingThrottledEmits(t *testing.T) {
	env, _ := newTestOSEnv(t)
	tool := NewBashTool(BashToolOptions{Env: env})
	st, ok := tool.(streamingTool)
	if !ok {
		t.Fatal("bash tool does not implement the streaming capability")
	}

	args := mustMarshalBashParams(t, bashParams{
		Command: `for i in $(seq 1 10); do for j in $(seq 1 250); do echo "line $i-$j"; done; sleep 0.05; done`,
	})

	type outcome struct {
		result    pi.ToolResult
		err       error
		snapshots []pi.ToolResult
	}
	outcomeCh := make(chan outcome, 1)
	go func() {
		var mu sync.Mutex
		var snapshots []pi.ToolResult
		result, err := st.ExecuteStream(context.Background(), "c1", args, func(r pi.ToolResult) {
			mu.Lock()
			snapshots = append(snapshots, r)
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
			t.Fatalf("got %d streaming emits, want >= 2", len(out.snapshots))
		}
		for i, snap := range out.snapshots {
			if snap.Data == nil {
				t.Errorf("snapshot %d Data = nil, want every partial emit to carry a Data payload (bash.ts:79-85 attaches details to every onUpdate)", i)
				continue
			}
			if i == 0 {
				continue
			}
			prev := out.snapshots[i-1]
			if prev.Data == nil {
				continue // already reported above
			}
			currDetails := mustUnmarshalBashDetails(t, snap.Data)
			prevDetails := mustUnmarshalBashDetails(t, prev.Data)
			currTruncated := currDetails.Truncation != nil && currDetails.Truncation.Truncated
			prevTruncated := prevDetails.Truncation != nil && prevDetails.Truncation.Truncated
			if currTruncated || prevTruncated {
				// Once truncation kicks in - including the very transition
				// step itself, where Output switches from the raw,
				// ever-growing tail to TruncateTail's capped "last N
				// lines/bytes" window (shell_output.go's snapshotLocked) -
				// its length can shrink or otherwise fluctuate as the
				// window slides forward. That's correct upstream-mirroring
				// behavior (confirmed by direct inspection: the shrink
				// lands exactly on the chunk where Truncation.Truncated
				// first flips true), not a bug, so the monotonic-growth
				// assertion only applies to the still-growing prefix before
				// truncation starts.
				continue
			}
			if len(snap.Content) < len(prev.Content) {
				t.Errorf("snapshot %d shrank (%d bytes -> %d bytes) before truncation kicked in: %q -> %q, want cumulative (non-decreasing) output",
					i, len(prev.Content), len(snap.Content), prev.Content, snap.Content)
			}
		}
		if !strings.Contains(out.result.Content, "line 10-250") {
			t.Errorf("final Content = %q, want it to contain %q", out.result.Content, "line 10-250")
		}

		last := out.snapshots[len(out.snapshots)-1]
		details := mustUnmarshalBashDetails(t, last.Data)
		if details.Truncation == nil || !details.Truncation.Truncated {
			t.Errorf("final streaming snapshot's Data.truncation = %+v, want truncated=true (2500 lines exceeds DefaultMaxLines=2000)", details.Truncation)
		}
		if details.FullOutputPath == "" {
			t.Error("final streaming snapshot's Data.fullOutputPath = \"\", want non-empty (output overflowed, so it must have been spilled)")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ExecuteStream did not return within 10s")
	}
}
