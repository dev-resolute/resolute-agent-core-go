package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	pi "github.com/dev-resolute/resolute-agent-core-go"
)

// bash.go ports packages/agent/src/harness/tools/bash.ts from upstream pi
// @0.82.0: the "bash" model-facing tool - run a shell command through
// ExecuteShellWithCapture (shell_output.go, Task 8), streaming throttled
// partial output through the root package's ExecuteStream seam (tool.go,
// Task 4), and assembling the same truncation notices, exit/timeout/abort
// status text, and Data payload upstream does, byte-for-byte.
//
// Deviations from upstream:
//   - bashParams.Timeout is *float64, not a plain float64: mirroring
//     read.go's readParams.Limit (Task 10), a pointer is required to tell
//     an explicit `timeout: 0` apart from an omitted field - both collapse
//     to the same JSON zero value on a plain float64, but upstream rejects
//     an explicit 0 (`timeout <= 0`) while treating an omitted timeout as
//     "no timeout enforced". nil means omitted; a non-nil pointer -
//     including one pointing at 0 - means the model supplied a timeout,
//     validated exactly as upstream's validateTimeout validates it.
//   - Upstream emits one extra `onUpdate({ content: [], details: undefined
//     })` immediately before starting the command, to clear any
//     previously-displayed state in a long-lived UI. That has no
//     observable effect in this port (there is no prior displayed state to
//     clear) and isn't part of this task's requirements, so it's omitted.
//   - Every failure (invalid timeout, a Prepare error, ExecuteShellWithCapture
//     itself failing to run the command) is returned as
//     pi.ToolResult{IsError: true, Content: <message>} rather than a
//     bubbled Go error, matching read.go/write.go's established convention:
//     upstream's harness uniformly converts any thrown error into an error
//     tool result regardless of origin. Upstream's thrown errors (cancelled/
//     timed-out/non-zero-exit) never carry `details` either - only the
//     successful, non-error return (bash.ts:155) does - so this port's
//     error-path ToolResults never set Data, matching that exactly.

// maxTimeoutSeconds mirrors upstream's MAX_TIMEOUT_SECONDS = 2_147_483_647 / 1000
// (bash.ts:8) - the largest value setTimeout can honor in milliseconds,
// expressed in seconds.
const maxTimeoutSeconds = 2147483647.0 / 1000.0

// bashUpdateThrottle mirrors upstream's BASH_UPDATE_THROTTLE_MS (bash.ts:9):
// partial-output emits are coalesced to at most once per this interval.
const bashUpdateThrottle = 100 * time.Millisecond

// bashToolDescription is the "bash" tool's model-facing description, ported
// verbatim from upstream's createBashTool with DefaultMaxLines/
// DefaultMaxBytes interpolated (upstream divides DEFAULT_MAX_BYTES by 1024
// directly here, plain integer division - "50KB" - rather than using
// formatSize, matching read.go's readToolDescription convention for the
// same reason).
var bashToolDescription = fmt.Sprintf(
	"Execute a bash command in the current working directory. Returns stdout and stderr. Output is truncated to last %d lines or %dKB (whichever is hit first). If truncated, full output is saved to a temp file. Optionally provide a timeout in seconds.",
	DefaultMaxLines, DefaultMaxBytes/1024,
)

// BashExecution describes a single "bash" tool invocation about to run. It
// is handed to BashToolOptions.Prepare (by pointer) so callers can inspect
// or mutate it - e.g. adding environment variables, or rewriting the
// working directory - before the command actually executes.
type BashExecution struct {
	// Command is the shell command that will run, already including
	// BashToolOptions.CommandPrefix if one was configured.
	Command string
	// Cwd is the working directory the command will run in. Defaults to
	// the tool's ExecutionEnv.Cwd().
	Cwd string
	// Env holds extra environment variables for the command, layered on
	// top of the inherited environment per InheritEnv. Starts empty.
	Env map[string]string
	// InheritEnv controls whether the command additionally inherits the
	// calling process's environment. Defaults to true.
	InheritEnv bool
}

// BashToolOptions configures NewBashTool.
type BashToolOptions struct {
	// Env is the filesystem/shell seam the tool runs through.
	Env ExecutionEnv
	// CommandPrefix, if non-empty, is prepended to every command as
	// CommandPrefix + "\n" + command - e.g. "set -e" to make the command
	// fail fast on any non-zero-exiting step.
	CommandPrefix string
	// Prepare, if set, runs after the BashExecution is built (command,
	// cwd, env, inheritEnv) and before it executes. It may mutate exec in
	// place. Returning an error aborts the call without executing the
	// command - the error's message becomes the tool's error result.
	Prepare func(ctx context.Context, exec *BashExecution) error
}

// bashParams are the model-supplied arguments to the "bash" tool. Timeout is
// *float64, not a plain float64: see this file's package comment for why a
// pointer is needed to distinguish an omitted timeout from an explicit 0.
//
// NOTE on Timeout's jsonschema tag: its description contains a comma,
// escaped here as `\\,` - the same fix edit.go's editParams.Edits tag
// applies and documents in full. Unescaped, invopop/jsonschema's tag
// parser (splitOnUnescapedCommas, reflect.go) treats the comma as a
// tag-option separator rather than description text, silently truncating
// the schema exposed to the model at "Timeout in seconds (optional" (see
// TestNewBashToolSchema, which pins the full, untruncated description this
// escaping produces).
type bashParams struct {
	Command string   `json:"command" jsonschema:"description=Bash command to execute"`
	Timeout *float64 `json:"timeout,omitempty" jsonschema:"description=Timeout in seconds (optional\\, no default timeout)"`
}

// NewBashTool creates the "bash" tool: run a shell command, streaming
// throttled partial output as it arrives and reporting truncation/exit/
// timeout/abort status in the final result. Ported from upstream's
// createBashTool. Registered via ExecuteStream (not Execute): the root
// package's pi.NewTool detects this and builds a tool that also supports
// plain Execute (with emit silently discarded) for callers that don't care
// about partial updates.
func NewBashTool(opts BashToolOptions) pi.RegisteredTool {
	env := opts.Env
	return pi.NewTool(pi.Tool[bashParams]{
		Name:        "bash",
		Description: bashToolDescription,
		ExecuteStream: func(ctx context.Context, p bashParams, emit func(pi.ToolResult)) (pi.ToolResult, error) {
			if err := validateBashTimeout(p.Timeout); err != nil {
				return pi.ToolResult{IsError: true, Content: err.Error()}, nil
			}

			command := p.Command
			if opts.CommandPrefix != "" {
				command = opts.CommandPrefix + "\n" + p.Command
			}
			exec := &BashExecution{
				Command:    command,
				Cwd:        env.Cwd(),
				Env:        map[string]string{},
				InheritEnv: true,
			}
			if opts.Prepare != nil {
				if err := opts.Prepare(ctx, exec); err != nil {
					return pi.ToolResult{IsError: true, Content: err.Error()}, nil
				}
			}

			throttle := newBashThrottle(emit)
			defer throttle.stop()

			var timeout time.Duration
			if p.Timeout != nil {
				timeout = time.Duration(*p.Timeout * float64(time.Second))
			}

			capture, err := ExecuteShellWithCapture(ctx, env, exec.Command, ShellCaptureOptions{
				Dir:        exec.Cwd,
				Env:        exec.Env,
				InheritEnv: exec.InheritEnv,
				Timeout:    timeout,
				OnChunk:    throttle.onChunk,
			})
			if err != nil {
				return pi.ToolResult{IsError: true, Content: err.Error()}, nil
			}
			throttle.finalFlush(capture)

			outputText := capture.Output
			var data json.RawMessage
			if capture.Truncation.Truncated {
				outputText += bashTruncationNotice(capture)
				data = marshalBashDetails(capture.Truncation, true, capture.FullOutputPath)
			}

			switch {
			case capture.Cancelled:
				return pi.ToolResult{IsError: true, Content: appendBashStatus(outputText, "Command aborted")}, nil
			case capture.TimedOut:
				status := fmt.Sprintf("Command timed out after %s seconds", formatJSNumber(*p.Timeout))
				return pi.ToolResult{IsError: true, Content: appendBashStatus(outputText, status)}, nil
			case capture.ExitCode != 0:
				status := fmt.Sprintf("Command exited with code %d", capture.ExitCode)
				return pi.ToolResult{IsError: true, Content: appendBashStatus(outputText, status)}, nil
			}

			if outputText == "" {
				outputText = "(no output)"
			}
			return pi.ToolResult{Content: outputText, Data: data}, nil
		},
	})
}

// validateBashTimeout mirrors bash.ts's validateTimeout: nil (omitted) is
// always valid (no timeout enforced); otherwise the pointed-to value must
// be finite and positive - explicit 0 included - and no larger than
// maxTimeoutSeconds.
func validateBashTimeout(timeout *float64) error {
	if timeout == nil {
		return nil
	}
	v := *timeout
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return errors.New("Invalid timeout: must be a finite number of seconds")
	}
	if v > maxTimeoutSeconds {
		return fmt.Errorf("Invalid timeout: maximum is %s seconds", formatJSNumber(maxTimeoutSeconds))
	}
	return nil
}

// formatJSNumber renders v the way JavaScript's Number.prototype.toString
// would for the timeout values this tool ever handles (non-negative, at
// most maxTimeoutSeconds ~= 2.1e6): plain decimal, never scientific
// notation. Go's %v/%g verbs switch to scientific notation well within
// that range (e.g. 2147483.647 -> "2.147483647e+06"), which would silently
// break byte-for-byte parity with upstream's interpolated template
// literals. JS itself only switches to exponential notation for magnitudes
// >=1e21 or <1e-6 - both far outside this tool's valid timeout range - so
// 'f' formatting with the shortest round-tripping precision matches JS's
// output for every value reachable here.
func formatJSNumber(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// appendBashStatus mirrors bash.ts's appendStatus: joins outputText and
// status with a blank line, or returns status alone when outputText is
// empty.
func appendBashStatus(outputText, status string) string {
	if outputText == "" {
		return status
	}
	return outputText + "\n\n" + status
}

// bashToolDetails is the JSON shape written to ToolResult.Data, mirroring
// upstream's BashToolDetails interface field-for-field with its camelCase
// JSON keys (`{ truncation?: TruncationResult; fullOutputPath?: string }`,
// bash.ts:18-21).
type bashToolDetails struct {
	Truncation     *truncationDetail `json:"truncation,omitempty"`
	FullOutputPath string            `json:"fullOutputPath,omitempty"`
}

// marshalBashDetails builds the {"truncation": ..., "fullOutputPath": ...}
// JSON payload for ToolResult.Data. includeTruncation mirrors upstream's
// `progress.truncation.truncated ? progress.truncation : undefined`
// (bash.ts:82) for partial emits - Truncation is omitted from the payload
// until truncation has actually kicked in, even though a truncation result
// (with Truncated: false) always exists. fullOutputPath is upstream's
// `progress.fullOutputPath`, which stays "" (upstream: undefined) until the
// stream first overflows and a spill file is created - an empty string
// here is dropped by the `omitempty` tag exactly as an undefined property
// is dropped by JSON.stringify. The error from json.Marshal is ignored:
// bashToolDetails is a plain struct of strings/bools/ints (truncationDetail,
// read.go) plus a string, which json.Marshal cannot fail on - matching
// read.go's marshalTruncationDetails established convention for the same
// reason.
func marshalBashDetails(t TruncationResult, includeTruncation bool, fullOutputPath string) json.RawMessage {
	details := bashToolDetails{FullOutputPath: fullOutputPath}
	if includeTruncation {
		details.Truncation = &truncationDetail{
			Content:               t.Content,
			Truncated:             t.Truncated,
			TruncatedBy:           t.TruncatedBy,
			TotalLines:            t.TotalLines,
			TotalBytes:            t.TotalBytes,
			OutputLines:           t.OutputLines,
			OutputBytes:           t.OutputBytes,
			LastLinePartial:       t.LastLinePartial,
			FirstLineExceedsLimit: t.FirstLineExceedsLimit,
			MaxLines:              t.MaxLines,
			MaxBytes:              t.MaxBytes,
		}
	}
	raw, _ := json.Marshal(details)
	return raw
}

// bashTruncationNotice builds the "[Showing ...]" suffix appended to
// capture.Output when capture.Truncation.Truncated, mirroring bash.ts's
// three-way branch on (lastLinePartial, truncatedBy) exactly, including
// its literal text:
//   - a single line too large to fit at all: "last-N-of-line" wording,
//     reporting both the shown size and the line's true full size.
//   - truncated by the line limit: plain "lines X-Y of Z" wording.
//   - truncated by the byte limit (and not a single oversized line):
//     the same wording plus a "(NKB limit)" qualifier.
func bashTruncationNotice(capture *ShellCapture) string {
	t := capture.Truncation
	startLine := t.TotalLines - t.OutputLines + 1
	endLine := t.TotalLines

	switch {
	case t.LastLinePartial:
		return fmt.Sprintf("\n\n[Showing last %s of line %d (line is %s). Full output: %s]",
			FormatSize(t.OutputBytes), endLine, FormatSize(capture.LastLineBytes), capture.FullOutputPath)
	case t.TruncatedBy == "lines":
		return fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Full output: %s]",
			startLine, endLine, t.TotalLines, capture.FullOutputPath)
	default:
		return fmt.Sprintf("\n\n[Showing lines %d-%d of %d (%s limit). Full output: %s]",
			startLine, endLine, t.TotalLines, FormatSize(DefaultMaxBytes), capture.FullOutputPath)
	}
}

// bashThrottle throttles partial-output emits to at most once per
// bashUpdateThrottle, mirroring bash.ts's scheduleOutputUpdate/
// emitOutputUpdate pair (a dirty flag plus a single pending timer). mu
// guards every field: onChunk fires synchronously from
// ExecuteShellWithCapture's output-pumping goroutine, while a scheduled
// timer's callback fires later from its own goroutine - both can touch
// this state.
type bashThrottle struct {
	emit func(pi.ToolResult)

	mu          sync.Mutex
	getProgress func() ShellProgress
	timer       *time.Timer
	dirty       bool
	lastEmit    time.Time
}

// newBashThrottle creates a bashThrottle that emits partial results through
// emit.
func newBashThrottle(emit func(pi.ToolResult)) *bashThrottle {
	return &bashThrottle{emit: emit}
}

// onChunk is passed as ShellCaptureOptions.OnChunk: it stashes the latest
// progress snapshot func and schedules a throttled emit.
func (t *bashThrottle) onChunk(_ []byte, progress func() ShellProgress) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.getProgress = progress
	t.scheduleLocked()
}

// scheduleLocked mirrors scheduleOutputUpdate: mark the state dirty, then
// either emit right away (if bashUpdateThrottle has already elapsed since
// the last emit) or make sure exactly one pending timer will emit once it
// does - a chunk arriving while a timer is already pending just leaves that
// timer in place, since emitLocked always reads the freshest progress at
// the moment it actually fires.
func (t *bashThrottle) scheduleLocked() {
	t.dirty = true
	delay := bashUpdateThrottle - time.Since(t.lastEmit)
	if delay <= 0 {
		t.clearTimerLocked()
		t.emitLocked()
		return
	}
	if t.timer == nil {
		t.timer = time.AfterFunc(delay, func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.timer = nil
			t.emitLocked()
		})
	}
}

// clearTimerLocked stops and forgets any pending timer. Safe to call when
// there is none.
func (t *bashThrottle) clearTimerLocked() {
	if t.timer == nil {
		return
	}
	t.timer.Stop()
	t.timer = nil
}

// emitLocked mirrors emitOutputUpdate: a no-op unless the state is dirty
// (a chunk has arrived since the last emit), so a timer that fires after
// finalFlush already emitted the same progress does nothing. Every emit -
// including before truncation ever kicks in - carries a Data payload,
// mirroring upstream's onUpdate always attaching a details object
// (bash.ts:79-85), even one whose truncation/fullOutputPath fields are
// both still omitted.
func (t *bashThrottle) emitLocked() {
	if !t.dirty || t.getProgress == nil {
		return
	}
	t.dirty = false
	t.lastEmit = time.Now()
	progress := t.getProgress()
	data := marshalBashDetails(progress.Truncation, progress.Truncation.Truncated, progress.FullOutputPath)
	t.emit(pi.ToolResult{Content: progress.Output, Data: data})
}

// finalFlush forces exactly one last emit of capture's final state,
// mirroring bash.ts's post-capture clearUpdateTimer()/emitOutputUpdate()
// pair: any pending timer is cancelled first, then the final capture is
// unconditionally (re-)emitted so the caller's last partial update always
// reflects the command's true final output - even if it arrived within the
// same throttle window as the previous emit.
func (t *bashThrottle) finalFlush(capture *ShellCapture) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clearTimerLocked()
	final := ShellProgress{
		Output:         capture.Output,
		Truncation:     capture.Truncation,
		FullOutputPath: capture.FullOutputPath,
	}
	t.getProgress = func() ShellProgress { return final }
	t.dirty = true
	t.emitLocked()
}

// stop cancels any pending timer without emitting. Deferred immediately
// after the throttle is created so a pending timer is never left running
// (and its goroutine never leaked) on any exit path, including
// ExecuteShellWithCapture itself returning an error before a final capture
// ever existed.
func (t *bashThrottle) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clearTimerLocked()
}
