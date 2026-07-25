package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// shell_output.go ports packages/agent/src/harness/utils/shell-output.ts
// from upstream pi @0.82.0: it runs a command via ExecutionEnv.Exec while
// capturing its merged stdout+stderr into an in-memory tail buffer capped
// at 2*DefaultMaxBytes (maxTailBytes below). The moment the WHOLE stream
// (not just the buffer) first exceeds DefaultMaxBytes or DefaultMaxLines,
// everything received so far is spilled to a temp file
// (env.CreateTemp("bash-", ".log") then env.AppendFile) so the full,
// untruncated output is never lost even once the in-memory buffer starts
// dropping its oldest bytes; every subsequent chunk is appended to that
// file as it arrives.
//
// The final Output/Truncation reported to the caller is TruncateTail
// applied to the (possibly front-trimmed) tail buffer, but its
// TotalLines/TotalBytes fields are overridden with the TRUE whole-stream
// counts, tracked incrementally chunk-by-chunk - the tail buffer alone
// loses that information once it has been trimmed.
//
// sanitizeBinaryOutput and CRLF normalization (upstream's
// `.replace(/\r/g, "")`) are ported ahead of byte/line accounting, in the
// same order upstream applies them, so byte/line counts reflect the
// cleaned text exactly as upstream's do.

// maxTailBytes is the in-memory tail buffer's cap, matching upstream's
// `DEFAULT_MAX_BYTES * 2`. It is deliberately larger than DefaultMaxBytes
// (the final reported-output limit) so the buffer retains enough history
// for TruncateTail to have real "last N lines/bytes" content to trim from.
const maxTailBytes = 2 * DefaultMaxBytes

// ShellProgress is a point-in-time snapshot of an in-progress
// ExecuteShellWithCapture call, obtained via the progress func passed to
// ShellCaptureOptions.OnChunk. It remains safe to call after
// ExecuteShellWithCapture has returned - it reads from the same
// mutex-guarded state the final ShellCapture is built from, so a caller
// may stash the progress func and invoke it later.
type ShellProgress struct {
	// Output is the best-effort output seen so far: the raw tail buffer,
	// or - once the whole stream has exceeded DefaultMaxBytes or
	// DefaultMaxLines - that buffer run through TruncateTail.
	Output string
	// Truncation describes Output's truncation status. Unlike a bare
	// TruncateTail(Output) call, TotalLines/TotalBytes here reflect the
	// WHOLE stream seen so far, not just the (possibly trimmed) tail
	// buffer.
	Truncation TruncationResult
	// FullOutputPath is the spill file's path once the stream has
	// overflowed and the file has been created, "" until then.
	FullOutputPath string
}

// ShellCapture is the final result of a completed ExecuteShellWithCapture
// call.
type ShellCapture struct {
	// Output, Truncation, FullOutputPath mirror ShellProgress's fields as
	// of process exit, cancellation, or timeout.
	Output         string
	Truncation     TruncationResult
	FullOutputPath string // "" unless the output overflowed and was spilled
	// LastLineBytes is the byte length of the stream's final (possibly
	// still-open, i.e. not newline-terminated) line. Unlike Output, it is
	// tracked against the WHOLE stream and is never affected by tail-buffer
	// trimming.
	LastLineBytes int
	// ExitCode is the command's exit code. When Cancelled or TimedOut is
	// true, treat those flags - not ExitCode - as authoritative that the
	// process didn't complete on its own; see ExecResult.ExitCode's doc
	// comment.
	ExitCode  int
	Cancelled bool
	TimedOut  bool
}

// ShellCaptureOptions configures ExecuteShellWithCapture.
type ShellCaptureOptions struct {
	// Dir, Env, InheritEnv, Timeout are forwarded to ExecutionEnv.Exec as
	// the corresponding ExecSpec fields.
	Dir        string
	Env        map[string]string
	InheritEnv bool
	Timeout    time.Duration
	// OnChunk, if set, is called with each sanitized output chunk as it
	// arrives (calls are serialized - see ExecSpec.OnChunk's doc comment,
	// which this wraps) along with a progress func that snapshots the
	// capture's current state. progress is safe to call synchronously from
	// within OnChunk, or stashed and called later, including after
	// ExecuteShellWithCapture has returned.
	OnChunk func(chunk []byte, progress func() ShellProgress)
}

// shellCaptureState holds the mutable state accumulated across a single
// ExecuteShellWithCapture call's OnChunk invocations. mu protects it
// because a caller may stash a progress func (handed to OnChunk) and
// invoke it concurrently with a later OnChunk call, or after
// ExecuteShellWithCapture has already returned.
type shellCaptureState struct {
	mu sync.Mutex

	tail string // in-memory tail buffer, capped at maxTailBytes

	// Whole-stream counters: track the ENTIRE stream, not just tail, since
	// tail is front-trimmed once it exceeds maxTailBytes.
	totalBytes       int
	completedLines   int
	hasOpenLine      bool
	currentLineBytes int

	fullOutputPath    string
	fullOutputStarted bool // whether the spill file has been created
	spillErr          error
}

// observeChunk sanitizes chunk, folds it into the running totals and tail
// buffer, and - if the whole stream has just overflowed, or already has -
// spills to (or appends to) the full-output file. It returns the sanitized
// text, which is what upstream hands to the caller's onChunk callback (not
// the raw chunk).
func (s *shellCaptureState) observeChunk(ctx context.Context, env ExecutionEnv, chunk []byte) []byte {
	text := strings.ReplaceAll(sanitizeBinaryOutput(string(chunk)), "\r", "")
	textBytes := len(text)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.totalBytes += textBytes
	s.completedLines += strings.Count(text, "\n")
	if lastNewline := strings.LastIndexByte(text, '\n'); lastNewline >= 0 {
		trailing := text[lastNewline+1:]
		s.currentLineBytes = len(trailing)
		s.hasOpenLine = len(trailing) > 0
	} else if len(text) > 0 {
		s.currentLineBytes += textBytes
		s.hasOpenLine = true
	}

	s.tail += text

	if s.spillErr == nil {
		totalLines := s.completedLines
		if s.hasOpenLine {
			totalLines++
		}
		overflowing := s.totalBytes > DefaultMaxBytes || totalLines > DefaultMaxLines

		switch {
		case overflowing && !s.fullOutputStarted:
			// First overflow: create the spill file and write EVERYTHING
			// received so far (s.tail is still untrimmed for this chunk -
			// the trim below happens after this branch), not just this
			// chunk.
			s.fullOutputStarted = true
			path, err := env.CreateTemp(ctx, "bash-", ".log")
			if err != nil {
				s.spillErr = fmt.Errorf("creating full-output spill file: %w", err)
				break
			}
			s.fullOutputPath = path
			if err := env.AppendFile(ctx, path, []byte(s.tail)); err != nil {
				s.spillErr = fmt.Errorf("writing full-output spill file %q: %w", path, err)
			}
		case s.fullOutputStarted:
			if err := env.AppendFile(ctx, s.fullOutputPath, []byte(text)); err != nil {
				s.spillErr = fmt.Errorf("appending to full-output spill file %q: %w", s.fullOutputPath, err)
			}
		}
	}

	s.tail = truncateStringToBytesFromEnd(s.tail, maxTailBytes)

	return []byte(text)
}

// snapshot builds a ShellProgress from the current state. Safe to call at
// any time, from any goroutine - see ShellCaptureOptions.OnChunk's doc
// comment.
func (s *shellCaptureState) snapshot() ShellProgress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

// snapshotLocked is snapshot's body, factored out so ExecuteShellWithCapture
// can build the final ShellCapture under the same lock acquisition it uses
// to read spillErr and currentLineBytes, without a second lock/unlock round
// trip.
func (s *shellCaptureState) snapshotLocked() ShellProgress {
	tailTruncation := TruncateTail(s.tail, TruncationOptions{})

	totalLines := s.completedLines
	if s.hasOpenLine {
		totalLines++
	}
	truncated := totalLines > DefaultMaxLines || s.totalBytes > DefaultMaxBytes

	truncation := tailTruncation
	truncation.Truncated = truncated
	truncation.TotalLines = totalLines
	truncation.TotalBytes = s.totalBytes
	switch {
	case !truncated:
		truncation.TruncatedBy = ""
	case tailTruncation.TruncatedBy != "":
		truncation.TruncatedBy = tailTruncation.TruncatedBy
	case s.totalBytes > DefaultMaxBytes:
		truncation.TruncatedBy = "bytes"
	default:
		truncation.TruncatedBy = "lines"
	}

	output := s.tail
	if truncated {
		output = truncation.Content
	}

	return ShellProgress{
		Output:         output,
		Truncation:     truncation,
		FullOutputPath: s.fullOutputPath,
	}
}

// sanitizeBinaryOutput strips control characters (other than tab/LF/CR)
// and interlinear annotation marks (U+FFF9-U+FFFB) from s, mirroring
// upstream's sanitizeBinaryOutput. Shell output can contain raw
// binary/control bytes (e.g. terminal escape sequences, stray NULs from a
// misbehaving command) that should not be treated as meaningful text.
func sanitizeBinaryOutput(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return r
		case r <= 0x1f:
			return -1
		case r >= 0xfff9 && r <= 0xfffb:
			return -1
		default:
			return r
		}
	}, s)
}

// ExecuteShellWithCapture runs command via env.Exec, capturing its merged
// stdout+stderr into the tail-buffer/spill-file scheme documented in this
// file's package comment. It returns a non-nil error only when env.Exec
// itself fails to run the command (e.g. bash isn't available) or when
// writing to the spill file fails - never for a non-zero exit code, a
// timeout, or a cancellation, all of which are reported on the returned
// ShellCapture instead.
//
// Deviation from upstream: after env.Exec returns, upstream re-checks
// `progress.truncation.truncated && !fullOutputRequested` and spills as a
// safety net before building the final result. That check is unreachable
// here and is deliberately omitted: env.Exec (env_unix.go's OSEnv.Exec)
// only returns once cmd.Wait has reaped the process, which - because
// stdout/stderr are plain io.Writers, not manually-drained pipes - only
// happens once every OnChunk call has already completed. observeChunk's
// overflow check runs on every chunk against the same monotonically
// non-decreasing totals the final snapshot would use, so if the stream
// ever overflows, spilling has already started by the time env.Exec
// returns.
//
// ctx is threaded into every observeChunk call (including its
// env.CreateTemp/env.AppendFile spill I/O), per this task's brief. A
// consequence: if ctx is cancelled or times out while a spill write is
// in-flight, that write can fail with ctx's error, which - like any other
// spillErr - aborts the whole call with a non-nil error instead of
// returning a partial ShellCapture. In practice this only matters for
// commands whose output is large enough to have already overflowed
// (triggering a spill) at the moment of cancellation/timeout; the small
// partial output a cancelled/timed-out command typically produces stays
// well under the spill threshold and is unaffected.
func ExecuteShellWithCapture(ctx context.Context, env ExecutionEnv, command string, opts ShellCaptureOptions) (*ShellCapture, error) {
	state := &shellCaptureState{}

	result, err := env.Exec(ctx, ExecSpec{
		Command:    command,
		Dir:        opts.Dir,
		Env:        opts.Env,
		InheritEnv: opts.InheritEnv,
		Timeout:    opts.Timeout,
		OnChunk: func(chunk []byte) {
			text := state.observeChunk(ctx, env, chunk)
			if opts.OnChunk != nil {
				opts.OnChunk(text, state.snapshot)
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("executing shell command with capture: %w", err)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.spillErr != nil {
		return nil, state.spillErr
	}
	progress := state.snapshotLocked()

	return &ShellCapture{
		Output:         progress.Output,
		Truncation:     progress.Truncation,
		FullOutputPath: progress.FullOutputPath,
		LastLineBytes:  state.currentLineBytes,
		ExitCode:       result.ExitCode,
		Cancelled:      result.Cancelled,
		TimedOut:       result.TimedOut,
	}, nil
}
