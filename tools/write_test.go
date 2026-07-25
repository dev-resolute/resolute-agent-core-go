package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	pi "github.com/dev-resolute/resolute-agent-core-go"
)

// write_test.go exercises write.go, the port of
// packages/agent/src/harness/tools/write.ts @0.82.0. Cases ported from
// packages/agent/test/harness/tools.test.ts's "write" describe block are
// noted per-case below.

func mustMarshalWriteParams(t *testing.T, path, content string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(writeParams{Path: path, Content: content})
	if err != nil {
		t.Fatalf("json.Marshal(writeParams) error: %v", err)
	}
	return raw
}

func TestNewWriteToolNameAndDescription(t *testing.T) {
	env, _ := newTestOSEnv(t)
	tool := NewWriteTool(WriteToolOptions{Env: env})

	if got, want := tool.Name(), "write"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}

	// Verbatim upstream write.ts description string (no interpolation).
	want := "Write content to a file. Creates the file if it doesn't exist, overwrites if it does. Automatically creates parent directories."
	if got := tool.Description(); got != want {
		t.Errorf("Description() = %q, want %q", got, want)
	}
}

func TestWriteToolSchema(t *testing.T) {
	env, _ := newTestOSEnv(t)
	tool := NewWriteTool(WriteToolOptions{Env: env})

	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatalf("json.Unmarshal(schema) error: %v", err)
	}

	pathProp, ok := schema.Properties["path"]
	if !ok {
		t.Fatal(`schema properties missing "path"`)
	}
	if want := "Path to the file to write (relative or absolute)"; pathProp.Description != want {
		t.Errorf(`schema "path" description = %q, want %q`, pathProp.Description, want)
	}

	contentProp, ok := schema.Properties["content"]
	if !ok {
		t.Fatal(`schema properties missing "content"`)
	}
	if want := "Content to write to the file"; contentProp.Description != want {
		t.Errorf(`schema "content" description = %q, want %q`, contentProp.Description, want)
	}
}

// TestWriteToolWritesFileAndCreatesParentDirectories ports upstream's
// "writes files and creates parent directories".
func TestWriteToolWritesFileAndCreatesParentDirectories(t *testing.T) {
	env, dir := newTestOSEnv(t)
	tool := NewWriteTool(WriteToolOptions{Env: env})

	result, err := tool.Execute(context.Background(), "write-1", mustMarshalWriteParams(t, "nested/dir/file.txt", "hello"))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, Content: %q", result.Content)
	}

	if want := "Successfully wrote 5 bytes to nested/dir/file.txt"; result.Content != want {
		t.Errorf("result.Content = %q, want %q", result.Content, want)
	}

	got, err := os.ReadFile(filepath.Join(dir, "nested", "dir", "file.txt"))
	if err != nil {
		t.Fatalf("os.ReadFile error: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("file content = %q, want %q", got, "hello")
	}
}

// TestWriteToolOverwritesExistingFile pins the brief's "overwrite works"
// requirement: a second write to the same path replaces its content and the
// reported byte count reflects the NEW content, not the old.
func TestWriteToolOverwritesExistingFile(t *testing.T) {
	env, dir := newTestOSEnv(t)
	tool := NewWriteTool(WriteToolOptions{Env: env})
	ctx := context.Background()

	if _, err := tool.Execute(ctx, "write-1", mustMarshalWriteParams(t, "f.txt", "first version")); err != nil {
		t.Fatalf("first Execute error: %v", err)
	}

	result, err := tool.Execute(ctx, "write-2", mustMarshalWriteParams(t, "f.txt", "second"))
	if err != nil {
		t.Fatalf("second Execute error: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, Content: %q", result.Content)
	}
	if want := "Successfully wrote 6 bytes to f.txt"; result.Content != want {
		t.Errorf("result.Content = %q, want %q", result.Content, want)
	}

	got, err := os.ReadFile(filepath.Join(dir, "f.txt"))
	if err != nil {
		t.Fatalf("os.ReadFile error: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("file content = %q, want %q (overwrite must replace, not append)", got, "second")
	}
}

// TestWriteToolWriteFailureIsAnErrorResult pins the "errors are error
// RESULTS, not bubbled Go errors" contract: a failing env.WriteFile must
// surface as pi.ToolResult{IsError: true}, with Execute's error return nil.
func TestWriteToolWriteFailureIsAnErrorResult(t *testing.T) {
	env, _ := newTestOSEnv(t)
	failing := &erroringWriteEnv{OSEnv: env, writeErr: errWriteBoom}
	tool := NewWriteTool(WriteToolOptions{Env: failing})

	result, err := tool.Execute(context.Background(), "write-1", mustMarshalWriteParams(t, "f.txt", "x"))
	if err != nil {
		t.Fatalf("Execute error = %v, want nil (failure must surface as ToolResult.IsError)", err)
	}
	if !result.IsError {
		t.Fatalf("result.IsError = false, want true")
	}
	if result.Content != errWriteBoom.Error() {
		t.Errorf("result.Content = %q, want %q", result.Content, errWriteBoom.Error())
	}
}

// TestWriteToolAbortedContextBeforeWriteReturnsOperationAborted pins
// write.ts:29's `if (signal?.aborted) throw new Error("Operation
// aborted")` check, ported as `ctx.Err() != nil` (write.go). Uses
// ctxIgnoringPathEnv so the pre-cancelled ctx reaches THIS check
// specifically, rather than being rejected earlier by env.AbsolutePath's
// own unrelated ctx.Err() early-return (OSEnv.AbsolutePath, env.go) -
// isolating exactly what's under test: the write tool's own abort check,
// not path resolution's.
func TestWriteToolAbortedContextBeforeWriteReturnsOperationAborted(t *testing.T) {
	osEnv, _ := newTestOSEnv(t)
	env := &ctxIgnoringPathEnv{OSEnv: osEnv}
	tool := NewWriteTool(WriteToolOptions{Env: env})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := tool.Execute(ctx, "write-1", mustMarshalWriteParams(t, "f.txt", "x"))
	if err != nil {
		t.Fatalf("Execute error = %v, want nil (failure must surface as ToolResult.IsError)", err)
	}
	if !result.IsError {
		t.Fatalf("result.IsError = false, want true")
	}
	if result.Content != "Operation aborted" {
		t.Errorf("result.Content = %q, want %q", result.Content, "Operation aborted")
	}
}

// TestWriteToolSerializesThroughMutationQueue proves the write tool routes
// through withFileMutationQueue (mutation_queue.go): a second write() call
// targeting the SAME path must block until the first write's env.WriteFile
// call completes, and the file ends up holding the second (later) write's
// content. Adapted from upstream's "keeps the mutation queue locked until an
// aborted write settles" - this port proves the same underlying
// serialization property (same-path writes never interleave) without the
// AbortController-specific half of that test, since mutation_queue.go's
// lock/fn is explicitly NOT cancellable through ctx once acquired (see its
// doc comment) - the abort-checks-inside-fn half is separately covered by
// TestWriteToolAbortedContextBeforeWriteReturnsOperationAborted; there is
// nothing left of upstream's abort-then-second-write scenario to exercise
// here beyond ordinary same-path serialization.
func TestWriteToolSerializesThroughMutationQueue(t *testing.T) {
	osEnv, dir := newTestOSEnv(t)
	env := &blockingWriteEnv{
		OSEnv:             osEnv,
		firstWriteStarted: make(chan struct{}),
		releaseFirstWrite: make(chan struct{}),
	}
	tool := NewWriteTool(WriteToolOptions{Env: env})
	ctx := context.Background()

	firstArgs := mustMarshalWriteParams(t, "file.txt", "first\n")
	secondArgs := mustMarshalWriteParams(t, "file.txt", "second\n")

	firstDone := make(chan pi.ToolResult, 1)
	go func() {
		result, err := tool.Execute(ctx, "write-first", firstArgs)
		if err != nil {
			t.Errorf("first write Execute error: %v", err)
		}
		firstDone <- result
	}()

	select {
	case <-env.firstWriteStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first write never started")
	}

	secondDone := make(chan pi.ToolResult, 1)
	go func() {
		result, err := tool.Execute(ctx, "write-second", secondArgs)
		if err != nil {
			t.Errorf("second write Execute error: %v", err)
		}
		secondDone <- result
	}()

	// The second write must remain blocked on the mutation queue's lock
	// while the first write's env.WriteFile call is still held open - it
	// must neither complete nor have entered env.WriteFile yet.
	select {
	case <-secondDone:
		t.Fatal("second write completed while the first write's mutation was still held - mutation queue did not serialize")
	case <-time.After(200 * time.Millisecond):
	}
	if env.secondWriteStarted.Load() {
		t.Fatal("second write's env.WriteFile ran before the first write's was released")
	}

	close(env.releaseFirstWrite)

	var firstResult, secondResult pi.ToolResult
	select {
	case firstResult = <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first write never completed after release")
	}
	select {
	case secondResult = <-secondDone:
	case <-time.After(2 * time.Second):
		t.Fatal("second write never completed")
	}

	if firstResult.IsError {
		t.Errorf("first write result.IsError = true, Content: %q", firstResult.Content)
	}
	if secondResult.IsError {
		t.Errorf("second write result.IsError = true, Content: %q", secondResult.Content)
	}

	got, err := os.ReadFile(filepath.Join(dir, "file.txt"))
	if err != nil {
		t.Fatalf("os.ReadFile error: %v", err)
	}
	if string(got) != "second\n" {
		t.Errorf("file content = %q, want %q (last write wins)", got, "second\n")
	}
}

// blockingWriteEnv wraps *OSEnv, blocking a WriteFile call whose content is
// "first\n" until releaseFirstWrite is closed, and recording whether a
// WriteFile call for "second\n" started - the doubles used by
// TestWriteToolSerializesThroughMutationQueue to prove same-path
// serialization deterministically (channels/atomics, no sleep-and-hope).
type blockingWriteEnv struct {
	*OSEnv
	firstWriteStarted  chan struct{}
	releaseFirstWrite  chan struct{}
	secondWriteStarted atomic.Bool
}

func (e *blockingWriteEnv) WriteFile(ctx context.Context, path string, data []byte) error {
	switch string(data) {
	case "first\n":
		close(e.firstWriteStarted)
		<-e.releaseFirstWrite
	case "second\n":
		e.secondWriteStarted.Store(true)
	}
	return e.OSEnv.WriteFile(ctx, path, data)
}

// erroringWriteEnv wraps *OSEnv, always failing WriteFile with writeErr -
// the double used by TestWriteToolWriteFailureIsAnErrorResult.
type erroringWriteEnv struct {
	*OSEnv
	writeErr error
}

func (e *erroringWriteEnv) WriteFile(ctx context.Context, path string, data []byte) error {
	return e.writeErr
}

// ctxIgnoringPathEnv wraps *OSEnv, ignoring ctx cancellation in
// AbsolutePath/CanonicalPath (which OSEnv otherwise rejects early - see
// env.go) so a pre-cancelled ctx can reach write.go's own in-lock
// ctx.Err() checks undisturbed - the double used by
// TestWriteToolAbortedContextBeforeWriteReturnsOperationAborted to isolate
// exactly what's under test.
type ctxIgnoringPathEnv struct {
	*OSEnv
}

func (e *ctxIgnoringPathEnv) AbsolutePath(_ context.Context, path string) (string, error) {
	return e.OSEnv.AbsolutePath(context.Background(), path)
}

func (e *ctxIgnoringPathEnv) CanonicalPath(_ context.Context, path string) (string, error) {
	return e.OSEnv.CanonicalPath(context.Background(), path)
}

// errWriteBoom is a fixed error value so
// TestWriteToolWriteFailureIsAnErrorResult can assert its exact message
// text flowed into result.Content unchanged.
var errWriteBoom = errors.New("boom: disk full")
