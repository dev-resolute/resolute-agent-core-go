package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	pi "github.com/dev-resolute/resolute-agent-core-go"
)

// edit_test.go exercises edit.go, the port of
// packages/agent/src/harness/tools/edit.ts @0.82.0. Cases ported from
// upstream's "edit" describe block (packages/agent/test/harness/tools.test.ts)
// are noted per-case below; the legacy-shim cases (prepareEditArguments) have
// no upstream test counterpart (edit.ts's own test suite never exercises
// prepareEditArguments directly) and are pinned here from the ported source
// itself.

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal(%#v) error: %v", v, err)
	}
	return raw
}

func TestNewEditToolNameAndDescription(t *testing.T) {
	env, _ := newTestOSEnv(t)
	tool := NewEditTool(EditToolOptions{Env: env})

	if got, want := tool.Name(), "edit"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}

	// Verbatim upstream edit.ts description string (no interpolation).
	want := "Edit a single file using exact text replacement. Every edits[].oldText must match a unique, non-overlapping region of the original file. If two changes affect the same block or nearby lines, merge them into one edit instead of emitting overlapping edits. Do not include large unchanged regions just to connect distant changes."
	if got := tool.Description(); got != want {
		t.Errorf("Description() = %q, want %q", got, want)
	}
}

func TestEditToolSchema(t *testing.T) {
	env, _ := newTestOSEnv(t)
	tool := NewEditTool(EditToolOptions{Env: env})

	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
			Items       *struct {
				Properties map[string]struct {
					Description string `json:"description"`
				} `json:"properties"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatalf("json.Unmarshal(schema) error: %v", err)
	}

	pathProp, ok := schema.Properties["path"]
	if !ok {
		t.Fatal(`schema properties missing "path"`)
	}
	if want := "Path to the file to edit (relative or absolute)"; pathProp.Description != want {
		t.Errorf(`schema "path" description = %q, want %q`, pathProp.Description, want)
	}

	editsProp, ok := schema.Properties["edits"]
	if !ok {
		t.Fatal(`schema properties missing "edits"`)
	}
	// Full description, byte-identical to upstream editSchema's edits field -
	// editParams.Edits' jsonschema tag escapes its internal commas (`\,`) so
	// invopop/jsonschema's tag parser (which otherwise treats an unescaped
	// comma as a tag-option separator, truncating the description at the
	// first one) emits the complete text into the generated schema.
	if want := "One or more targeted replacements. Each edit is matched against the original file, not incrementally. Do not include overlapping or nested edits. If two changes touch the same block or nearby lines, merge them into one edit instead."; editsProp.Description != want {
		t.Errorf(`schema "edits" description = %q, want %q`, editsProp.Description, want)
	}
	if editsProp.Items == nil {
		t.Fatal(`schema "edits" has no "items"`)
	}

	oldTextProp, ok := editsProp.Items.Properties["oldText"]
	if !ok {
		t.Fatal(`schema "edits" items missing "oldText"`)
	}
	if want := "Exact text for one targeted replacement. It must be unique in the original file and must not overlap with any other edits[].oldText in the same call."; oldTextProp.Description != want {
		t.Errorf(`schema "edits" items "oldText" description = %q, want %q`, oldTextProp.Description, want)
	}

	newTextProp, ok := editsProp.Items.Properties["newText"]
	if !ok {
		t.Fatal(`schema "edits" items missing "newText"`)
	}
	if want := "Replacement text for this targeted edit."; newTextProp.Description != want {
		t.Errorf(`schema "edits" items "newText" description = %q, want %q`, newTextProp.Description, want)
	}
}

// TestEditToolAppliesDisjointEditsAndReturnsBothDiffFormats ports upstream's
// "applies disjoint edits and returns both diff formats".
func TestEditToolAppliesDisjointEditsAndReturnsBothDiffFormats(t *testing.T) {
	env, dir := newTestOSEnv(t)
	original := "alpha\nbeta\ngamma\ndelta\n"
	if err := os.WriteFile(filepath.Join(dir, "edit.txt"), []byte(original), 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	tool := NewEditTool(EditToolOptions{Env: env})

	args := mustMarshal(t, editParams{
		Path: "edit.txt",
		Edits: []editEntry{
			{OldText: "alpha\n", NewText: "ALPHA\n"},
			{OldText: "gamma\n", NewText: "GAMMA\n"},
		},
	})
	result, err := tool.Execute(context.Background(), "edit-1", args)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, Content: %q", result.Content)
	}

	if want := "Successfully replaced 2 block(s) in edit.txt."; result.Content != want {
		t.Errorf("result.Content = %q, want %q", result.Content, want)
	}

	got, err := os.ReadFile(filepath.Join(dir, "edit.txt"))
	if err != nil {
		t.Fatalf("os.ReadFile error: %v", err)
	}
	if want := "ALPHA\nbeta\nGAMMA\ndelta\n"; string(got) != want {
		t.Errorf("file content = %q, want %q", got, want)
	}

	var details editToolDetails
	if err := json.Unmarshal(result.Data, &details); err != nil {
		t.Fatalf("json.Unmarshal(result.Data) error: %v", err)
	}
	if !strings.Contains(details.Diff, "ALPHA") {
		t.Errorf("details.Diff = %q, want to contain %q", details.Diff, "ALPHA")
	}
	if !strings.Contains(details.Diff, "GAMMA") {
		t.Errorf("details.Diff = %q, want to contain %q", details.Diff, "GAMMA")
	}
	if !strings.Contains(details.Patch, "-alpha") || !strings.Contains(details.Patch, "+ALPHA") {
		t.Errorf("details.Patch = %q, want to contain replaced alpha/ALPHA lines", details.Patch)
	}
	if !strings.HasPrefix(details.Patch, "--- edit.txt\n+++ edit.txt\n") {
		t.Errorf("details.Patch = %q, want to start with unified-diff file headers for edit.txt", details.Patch)
	}
	if want := 1; details.FirstChangedLine != want {
		t.Errorf("details.FirstChangedLine = %d, want %d", details.FirstChangedLine, want)
	}
}

// TestEditToolPreservesBOMAndCRLF ports upstream's "preserves BOM and CRLF
// line endings".
func TestEditToolPreservesBOMAndCRLF(t *testing.T) {
	env, dir := newTestOSEnv(t)
	original := "\uFEFFone\r\ntwo\r\n"
	if err := os.WriteFile(filepath.Join(dir, "edit.txt"), []byte(original), 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	tool := NewEditTool(EditToolOptions{Env: env})

	args := mustMarshal(t, editParams{
		Path:  "edit.txt",
		Edits: []editEntry{{OldText: "two", NewText: "TWO"}},
	})
	result, err := tool.Execute(context.Background(), "edit-1", args)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, Content: %q", result.Content)
	}

	got, err := os.ReadFile(filepath.Join(dir, "edit.txt"))
	if err != nil {
		t.Fatalf("os.ReadFile error: %v", err)
	}
	if want := "\uFEFFone\r\nTWO\r\n"; string(got) != want {
		t.Errorf("file content = %q, want %q (BOM and CRLF must round-trip)", got, want)
	}
}

// TestEditToolDirectoryPathIsNotAFile pins the FileInfo kind check: editing
// a directory must fail with upstream's exact "Path is not a file" message,
// not a generic I/O error.
func TestEditToolDirectoryPathIsNotAFile(t *testing.T) {
	env, dir := newTestOSEnv(t)
	if err := os.Mkdir(filepath.Join(dir, "adir"), 0o755); err != nil {
		t.Fatalf("os.Mkdir error: %v", err)
	}
	tool := NewEditTool(EditToolOptions{Env: env})

	args := mustMarshal(t, editParams{
		Path:  "adir",
		Edits: []editEntry{{OldText: "x", NewText: "y"}},
	})
	result, err := tool.Execute(context.Background(), "edit-1", args)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("result.IsError = false, want true")
	}
	if want := "Could not edit file: adir. Path is not a file."; result.Content != want {
		t.Errorf("result.Content = %q, want %q", result.Content, want)
	}
}

// TestEditToolMissingFileErrorCode pins editAccessErrorMessage's "Error
// code" slot: a fileInfo failure on a file that doesn't exist (ENOENT) must
// report upstream's stable "not_found" FileErrorCode (nodejs.ts's
// toFileError maps ENOENT to "not_found"), not a raw errno or a generic Go
// error string.
func TestEditToolMissingFileErrorCode(t *testing.T) {
	env, _ := newTestOSEnv(t)
	tool := NewEditTool(EditToolOptions{Env: env})

	args := mustMarshal(t, editParams{
		Path:  "does-not-exist.txt",
		Edits: []editEntry{{OldText: "x", NewText: "y"}},
	})
	result, err := tool.Execute(context.Background(), "edit-1", args)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("result.IsError = false, want true")
	}
	if want := "Could not edit file: does-not-exist.txt. Error code: not_found."; result.Content != want {
		t.Errorf("result.Content = %q, want %q", result.Content, want)
	}
}

// TestFileErrorCodeMapsAllReachableCodes pins fileErrorCode's full mapping
// directly (bypassing the tool pipeline), including the two upstream
// toFileError (nodejs.ts) branches it didn't originally cover:
// ENOTDIR -> "not_directory" and EISDIR -> "is_directory". "is_directory" is
// exercised here rather than end-to-end because edit.go's own FileInfo kind
// check (runEdit) always intercepts an actual directory BEFORE any
// ReadFile/WriteFile call could surface EISDIR - same as upstream's
// edit.ts, which performs the identical kind check before ever touching
// env.readTextFile/env.writeFile - so EISDIR has no reachable call site in
// this tool's own pipeline to exercise it through, only through
// fileErrorCode itself.
func TestFileErrorCodeMapsAllReachableCodes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "not found", err: fmt.Errorf("wrap: %w", fs.ErrNotExist), want: "not_found"},
		{name: "permission denied", err: fmt.Errorf("wrap: %w", fs.ErrPermission), want: "permission_denied"},
		{name: "not a directory", err: fmt.Errorf("wrap: %w", syscall.ENOTDIR), want: "not_directory"},
		{name: "is a directory", err: fmt.Errorf("wrap: %w", syscall.EISDIR), want: "is_directory"},
		{name: "unmapped error falls back to unknown", err: errors.New("boom"), want: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fileErrorCode(tt.err); got != tt.want {
				t.Errorf("fileErrorCode(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

// TestEditToolNotADirectoryErrorCode pins fileErrorCode's ENOTDIR mapping
// end-to-end, through the tool: editing a path that walks THROUGH a regular
// file as though it were a directory (here, "file.txt/nested" where
// file.txt is itself a plain file) fails at the OS level with ENOTDIR,
// which - unlike ENOENT - does NOT satisfy errors.Is(err, fs.ErrNotExist)
// in Go.
//
// This must reach runEdit's own env.FileInfo call (and thus
// editAccessErrorMessage/fileErrorCode) rather than fail earlier, at
// mutation_queue.go's own path-resolution step - which, ported faithfully
// from upstream's getMutationQueueKey (file-mutation-queue.ts), ALSO throws
// a raw, unformatted error for any canonicalPath failure other than
// "not_found"/"not_supported", including "not_directory". Verified against
// edit.ts + file-mutation-queue.ts: upstream itself produces that same raw,
// pre-execute() error for this exact "file.txt/nested" scenario, never
// reaching editAccessError's "Could not edit file: ..." formatting - so
// testing THROUGH NewEditTool.Execute with a plain OSEnv would only prove
// mutation_queue.go's own (already-tested, upstream-faithful) early exit,
// not fileErrorCode's new mapping. canonicalPathSkippingEnv isolates
// exactly what's under test here (same idiom as ctxIgnoringPathEnv in
// write_test.go): it lets the mutation queue's key resolution succeed
// trivially, so the SAME "file.txt/nested" path's ENOTDIR is instead first
// surfaced by the real *OSEnv.FileInfo call inside runEdit, going through
// editAccessErrorMessage/fileErrorCode as designed.
func TestEditToolNotADirectoryErrorCode(t *testing.T) {
	osEnv, dir := newTestOSEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("content"), 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	env := &canonicalPathSkippingEnv{OSEnv: osEnv}
	tool := NewEditTool(EditToolOptions{Env: env})

	args := mustMarshal(t, editParams{
		Path:  "file.txt/nested",
		Edits: []editEntry{{OldText: "x", NewText: "y"}},
	})
	result, err := tool.Execute(context.Background(), "edit-1", args)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("result.IsError = false, want true")
	}
	if want := "Could not edit file: file.txt/nested. Error code: not_directory."; result.Content != want {
		t.Errorf("result.Content = %q, want %q", result.Content, want)
	}
}

// canonicalPathSkippingEnv wraps *OSEnv, making CanonicalPath always
// succeed (returning the plain absolute path, without ever calling
// filepath.EvalSymlinks) - the double used by
// TestEditToolNotADirectoryErrorCode to keep mutation_queue.go's own
// path-resolution step from failing first, isolating fileErrorCode's ENOTDIR
// mapping as reached from runEdit's own env.FileInfo call.
type canonicalPathSkippingEnv struct {
	*OSEnv
}

func (e *canonicalPathSkippingEnv) CanonicalPath(ctx context.Context, path string) (string, error) {
	return e.OSEnv.AbsolutePath(ctx, path)
}

// TestEditToolEmptyEditsIsInvalid pins validateEditInput's exact message.
// Per the task-11 brief: this observable result is pinned regardless of
// which layer (the tool-level check in NewEditTool, or
// ApplyEditsToNormalizedContent's own guard) produces it.
func TestEditToolEmptyEditsIsInvalid(t *testing.T) {
	env, dir := newTestOSEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "edit.txt"), []byte("content"), 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	tool := NewEditTool(EditToolOptions{Env: env})

	args := mustMarshal(t, editParams{Path: "edit.txt", Edits: []editEntry{}})
	result, err := tool.Execute(context.Background(), "edit-1", args)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("result.IsError = false, want true")
	}
	if want := "Edit tool input is invalid. edits must contain at least one replacement."; result.Content != want {
		t.Errorf("result.Content = %q, want %q", result.Content, want)
	}
}

// TestEditToolLegacyShimRawOldTextNewText pins prepareEditArguments's
// deprecated top-level oldText/newText shape: raw args with no "edits" key
// at all must execute as a single edit.
func TestEditToolLegacyShimRawOldTextNewText(t *testing.T) {
	env, dir := newTestOSEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	tool := NewEditTool(EditToolOptions{Env: env})

	raw := json.RawMessage(`{"path":"f.txt","oldText":"world","newText":"there"}`)
	result, err := tool.Execute(context.Background(), "edit-1", raw)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, Content: %q", result.Content)
	}
	if want := "Successfully replaced 1 block(s) in f.txt."; result.Content != want {
		t.Errorf("result.Content = %q, want %q", result.Content, want)
	}

	got, err := os.ReadFile(filepath.Join(dir, "f.txt"))
	if err != nil {
		t.Fatalf("os.ReadFile error: %v", err)
	}
	if want := "hello there"; string(got) != want {
		t.Errorf("file content = %q, want %q", got, want)
	}
}

// TestEditToolLegacyShimMergesWithExistingEdits pins prepareEditArguments's
// merge behavior: a legacy oldText/newText pair alongside an existing
// edits[] array is appended AFTER the existing entries, not used instead of
// them.
func TestEditToolLegacyShimMergesWithExistingEdits(t *testing.T) {
	env, dir := newTestOSEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("one two three"), 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	tool := NewEditTool(EditToolOptions{Env: env})

	raw := json.RawMessage(`{"path":"f.txt","edits":[{"oldText":"one","newText":"ONE"}],"oldText":"three","newText":"THREE"}`)
	result, err := tool.Execute(context.Background(), "edit-1", raw)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, Content: %q", result.Content)
	}
	if want := "Successfully replaced 2 block(s) in f.txt."; result.Content != want {
		t.Errorf("result.Content = %q, want %q", result.Content, want)
	}

	got, err := os.ReadFile(filepath.Join(dir, "f.txt"))
	if err != nil {
		t.Fatalf("os.ReadFile error: %v", err)
	}
	if want := "ONE two THREE"; string(got) != want {
		t.Errorf("file content = %q, want %q", got, want)
	}
}

// TestEditToolLegacyShimEditsAsJSONString pins prepareEditArguments's
// string-decode branch: edits supplied as a JSON-ENCODED STRING (rather than
// a JSON array) must be decoded and applied exactly as if the array had been
// sent directly.
func TestEditToolLegacyShimEditsAsJSONString(t *testing.T) {
	env, dir := newTestOSEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("foo bar"), 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	tool := NewEditTool(EditToolOptions{Env: env})

	raw := json.RawMessage(`{"path":"f.txt","edits":"[{\"oldText\":\"foo\",\"newText\":\"FOO\"},{\"oldText\":\"bar\",\"newText\":\"BAR\"}]"}`)
	result, err := tool.Execute(context.Background(), "edit-1", raw)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, Content: %q", result.Content)
	}
	if want := "Successfully replaced 2 block(s) in f.txt."; result.Content != want {
		t.Errorf("result.Content = %q, want %q", result.Content, want)
	}

	got, err := os.ReadFile(filepath.Join(dir, "f.txt"))
	if err != nil {
		t.Fatalf("os.ReadFile error: %v", err)
	}
	if want := "FOO BAR"; string(got) != want {
		t.Errorf("file content = %q, want %q", got, want)
	}
}

// TestEditToolLegacyShimEditsAsUnparsableStringLeftUntouched pins the
// try/catch-with-Array.isArray guard in prepareEditArguments: an edits
// string that fails to parse as JSON, or that parses to something other
// than an array, must be left exactly as-is (not silently dropped or
// replaced with an empty array) so downstream unmarshaling reports the real
// shape mismatch rather than swallowing it.
func TestEditToolLegacyShimEditsAsUnparsableStringLeftUntouched(t *testing.T) {
	env, dir := newTestOSEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("content"), 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	tool := NewEditTool(EditToolOptions{Env: env})

	raw := json.RawMessage(`{"path":"f.txt","edits":"not valid json"}`)
	_, err := tool.Execute(context.Background(), "edit-1", raw)
	if err == nil {
		t.Fatal("Execute error = nil, want an unmarshal error (edits string left untouched must fail to decode into []editEntry)")
	}
}

// TestEditToolAbortedContextBeforeFileInfoReturnsOperationAborted pins
// edit.ts:93's first `if (signal?.aborted) throw new Error("Operation
// aborted")` check, run once the mutation lock is held and before
// env.fileInfo - ported as `ctx.Err() != nil` (edit.go's runEdit). Uses
// ctxIgnoringPathEnv (defined in write_test.go, same package) so the
// pre-cancelled ctx reaches THIS check specifically, rather than being
// rejected earlier by env.AbsolutePath's own unrelated ctx.Err() early
// return.
func TestEditToolAbortedContextBeforeFileInfoReturnsOperationAborted(t *testing.T) {
	osEnv, dir := newTestOSEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("content"), 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	env := &ctxIgnoringPathEnv{OSEnv: osEnv}
	tool := NewEditTool(EditToolOptions{Env: env})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	args := mustMarshal(t, editParams{
		Path:  "f.txt",
		Edits: []editEntry{{OldText: "content", NewText: "new"}},
	})
	result, err := tool.Execute(ctx, "edit-1", args)
	if err != nil {
		t.Fatalf("Execute error = %v, want nil (failure must surface as ToolResult.IsError)", err)
	}
	if !result.IsError {
		t.Fatalf("result.IsError = false, want true")
	}
	if result.Content != operationAbortedMessage {
		t.Errorf("result.Content = %q, want %q", result.Content, operationAbortedMessage)
	}
}

// TestEditToolEditsThroughSymlink ports upstream's "edits regular files
// through symlinks": FileInfo's kind check must allow FileKindSymlink, not
// just FileKindFile.
func TestEditToolEditsThroughSymlink(t *testing.T) {
	env, dir := newTestOSEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "target.txt"), []byte("before\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "target.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Fatalf("os.Symlink error: %v", err)
	}
	tool := NewEditTool(EditToolOptions{Env: env})

	args := mustMarshal(t, editParams{
		Path:  "link.txt",
		Edits: []editEntry{{OldText: "before", NewText: "after"}},
	})
	result, err := tool.Execute(context.Background(), "edit-1", args)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, Content: %q", result.Content)
	}

	got, err := os.ReadFile(filepath.Join(dir, "target.txt"))
	if err != nil {
		t.Fatalf("os.ReadFile error: %v", err)
	}
	if want := "after\n"; string(got) != want {
		t.Errorf("target file content = %q, want %q", got, want)
	}
}

// TestEditToolSerializesThroughMutationQueue proves the edit tool routes
// through withFileMutationQueue (mutation_queue.go): a second edit() call
// targeting the SAME path must block until the first edit's env.WriteFile
// call completes, and the file ends up holding BOTH edits, applied in
// order (the second edit's read must observe the first edit's write, not
// race a stale copy of the file).
//
// Adapted from upstream's "keeps the mutation queue locked until an
// aborted edit write settles" - like write_test.go's
// TestWriteToolSerializesThroughMutationQueue, this proves the same
// underlying serialization property without the AbortController-specific
// half of that test, since mutation_queue.go's lock/fn is explicitly NOT
// cancellable through ctx once acquired (see its doc comment) - the
// abort-checks-inside-fn half is separately covered by
// TestEditToolAbortedContextBeforeFileInfoReturnsOperationAborted; there is
// nothing left of upstream's abort-then-second-edit scenario to exercise
// here beyond ordinary same-path serialization.
func TestEditToolSerializesThroughMutationQueue(t *testing.T) {
	osEnv, dir := newTestOSEnv(t)
	env := &blockingEditWriteEnv{
		OSEnv:             osEnv,
		firstWriteStarted: make(chan struct{}),
		releaseFirstWrite: make(chan struct{}),
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	tool := NewEditTool(EditToolOptions{Env: env})
	ctx := context.Background()

	firstArgs := mustMarshal(t, editParams{Path: "file.txt", Edits: []editEntry{{OldText: "alpha", NewText: "ALPHA"}}})
	secondArgs := mustMarshal(t, editParams{Path: "file.txt", Edits: []editEntry{{OldText: "beta", NewText: "BETA"}}})

	firstDone := make(chan pi.ToolResult, 1)
	go func() {
		result, err := tool.Execute(ctx, "edit-first", firstArgs)
		if err != nil {
			t.Errorf("first edit Execute error: %v", err)
		}
		firstDone <- result
	}()

	select {
	case <-env.firstWriteStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first edit's write never started")
	}

	secondDone := make(chan pi.ToolResult, 1)
	go func() {
		result, err := tool.Execute(ctx, "edit-second", secondArgs)
		if err != nil {
			t.Errorf("second edit Execute error: %v", err)
		}
		secondDone <- result
	}()

	// The second edit must remain blocked on the mutation queue's lock
	// while the first edit's env.WriteFile call is still held open.
	select {
	case <-secondDone:
		t.Fatal("second edit completed while the first edit's write was still held - mutation queue did not serialize")
	case <-time.After(200 * time.Millisecond):
	}
	if env.secondWriteStarted.Load() {
		t.Fatal("second edit's env.WriteFile ran before the first edit's was released")
	}

	close(env.releaseFirstWrite)

	var firstResult, secondResult pi.ToolResult
	select {
	case firstResult = <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first edit never completed after release")
	}
	select {
	case secondResult = <-secondDone:
	case <-time.After(2 * time.Second):
		t.Fatal("second edit never completed")
	}

	if firstResult.IsError {
		t.Errorf("first edit result.IsError = true, Content: %q", firstResult.Content)
	}
	if secondResult.IsError {
		t.Errorf("second edit result.IsError = true, Content: %q", secondResult.Content)
	}

	got, err := os.ReadFile(filepath.Join(dir, "file.txt"))
	if err != nil {
		t.Fatalf("os.ReadFile error: %v", err)
	}
	if want := "ALPHA\nBETA\n"; string(got) != want {
		t.Errorf("file content = %q, want %q (both edits must have landed, in order)", got, want)
	}
}

// blockingEditWriteEnv wraps *OSEnv, blocking a WriteFile call whose content
// is the first edit's expected output ("ALPHA\nbeta\n") until
// releaseFirstWrite is closed, and recording whether a WriteFile call for
// the second edit's expected output ("ALPHA\nBETA\n") started - the double
// used by TestEditToolSerializesThroughMutationQueue to prove same-path
// serialization deterministically (channels/atomics, no sleep-and-hope),
// mirroring write_test.go's blockingWriteEnv.
type blockingEditWriteEnv struct {
	*OSEnv
	firstWriteStarted  chan struct{}
	releaseFirstWrite  chan struct{}
	secondWriteStarted atomic.Bool
}

func (e *blockingEditWriteEnv) WriteFile(ctx context.Context, path string, data []byte) error {
	switch string(data) {
	case "ALPHA\nbeta\n":
		close(e.firstWriteStarted)
		<-e.releaseFirstWrite
	case "ALPHA\nBETA\n":
		e.secondWriteStarted.Store(true)
	}
	return e.OSEnv.WriteFile(ctx, path, data)
}

// TestEditToolSerializesConcurrentEditsThroughSymlinkAndCanonicalPath ports
// upstream's "serializes concurrent edits through canonical and symlink
// paths": two concurrent edit calls targeting the SAME underlying file
// through DIFFERENT path strings (one via the canonical path, one via a
// symlink to it) must still serialize through the mutation queue - proving
// mutationQueueKey's symlink resolution (TestMutationQueueKey,
// mutation_queue_test.go) actually PREVENTS the two calls from racing each
// other, not merely that it computes the same key value in isolation.
// slowReadEditEnv delays every ReadFile by 20ms, widening the race window:
// without correct serialization, both calls would read the pre-edit
// content concurrently and the second write would silently lose the
// first edit (last-write-wins on stale data) - deterministically exposing
// a broken queue instead of merely usually passing.
func TestEditToolSerializesConcurrentEditsThroughSymlinkAndCanonicalPath(t *testing.T) {
	osEnv, dir := newTestOSEnv(t)
	env := &slowReadEditEnv{OSEnv: osEnv}
	if err := os.WriteFile(filepath.Join(dir, "target.txt"), []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "target.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Fatalf("os.Symlink error: %v", err)
	}
	tool := NewEditTool(EditToolOptions{Env: env})
	ctx := context.Background()

	targetArgs := mustMarshal(t, editParams{Path: "target.txt", Edits: []editEntry{{OldText: "alpha", NewText: "ALPHA"}}})
	linkArgs := mustMarshal(t, editParams{Path: "link.txt", Edits: []editEntry{{OldText: "beta", NewText: "BETA"}}})

	calls := []struct {
		id   string
		args json.RawMessage
	}{
		{"edit-target", targetArgs},
		{"edit-link", linkArgs},
	}
	var wg sync.WaitGroup
	results := make(chan pi.ToolResult, len(calls))
	for _, call := range calls {
		wg.Add(1)
		go func(id string, args json.RawMessage) {
			defer wg.Done()
			result, err := tool.Execute(ctx, id, args)
			if err != nil {
				t.Errorf("%s Execute error: %v", id, err)
				return
			}
			results <- result
		}(call.id, call.args)
	}
	wg.Wait()
	close(results)
	for result := range results {
		if result.IsError {
			t.Errorf("result.IsError = true, Content: %q", result.Content)
		}
	}

	got, err := os.ReadFile(filepath.Join(dir, "target.txt"))
	if err != nil {
		t.Fatalf("os.ReadFile error: %v", err)
	}
	if want := "ALPHA\nBETA\ngamma\n"; string(got) != want {
		t.Errorf("file content = %q, want %q (both concurrent edits, through different paths to the same file, must land - a broken mutation queue would lose one)", got, want)
	}
}

// slowReadEditEnv wraps *OSEnv, delaying every ReadFile call by 20ms - the
// double used by
// TestEditToolSerializesConcurrentEditsThroughSymlinkAndCanonicalPath to
// widen the race window a broken mutation queue would need to lose an
// update through, mirroring upstream's SlowReadExecutionEnv.
type slowReadEditEnv struct {
	*OSEnv
}

func (e *slowReadEditEnv) ReadFile(ctx context.Context, path string) ([]byte, error) {
	time.Sleep(20 * time.Millisecond)
	return e.OSEnv.ReadFile(ctx, path)
}
