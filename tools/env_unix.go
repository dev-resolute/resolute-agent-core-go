//go:build unix

package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// env_unix.go implements OSEnv.Exec for POSIX systems: it runs the command
// in its own process group (SysProcAttr.Setpgid) so that on timeout or
// cancellation the WHOLE group can be killed with a single
// syscall.Kill(-pid, SIGKILL) - matching upstream's cross-platform
// killProcessTree (packages/agent/src/harness/env/nodejs.ts @0.82.0, which
// does process.kill(-pid, "SIGKILL") on POSIX). Killing only the immediate
// `bash -c` process would leave background/child processes the command
// spawned (e.g. `sleep 30 &`) running as orphans; killing the process group
// takes them all out.

// chunkWriter is a mutex-guarded io.Writer that forwards each Write to fn,
// if set. cmd.Stdout and cmd.Stderr are both set to the SAME chunkWriter, so
// OnChunk observes merged stdout+stderr output in arrival order; the mutex
// serializes the two concurrent pipe-copying goroutines exec.Cmd runs
// internally (one per stdio stream) so writes/callbacks never interleave or
// race.
type chunkWriter struct {
	mu sync.Mutex
	fn func(chunk []byte)
}

// Write implements io.Writer. It never returns an error: a failing OnChunk
// callback should not abort output capture (bash tool output, in
// particular, must keep flowing to the in-memory/spill capture in Task 8
// regardless of what an optional progress callback does).
func (w *chunkWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fn != nil {
		// p is only valid for the duration of this call (exec.Cmd reuses
		// its read buffer); copy before handing it to fn, which the
		// ExecSpec.OnChunk contract also documents.
		chunk := make([]byte, len(p))
		copy(chunk, p)
		w.fn(chunk)
	}
	return len(p), nil
}

// Exec runs spec.Command via `bash -c` in a new process group. Timeout and
// ctx cancellation both kill the process group and reap the process (via
// cmd.Wait, run on a background goroutine so the kill path never blocks on
// it) before returning, so callers never leak a goroutine or a zombie.
func (e *OSEnv) Exec(ctx context.Context, spec ExecSpec) (*ExecResult, error) {
	dir := spec.Dir
	if dir == "" {
		dir = e.cwd
	}

	cmd := exec.Command("bash", "-c", spec.Command)
	cmd.Dir = dir
	if spec.InheritEnv {
		cmd.Env = append(os.Environ(), flattenEnv(spec.Env)...)
	} else {
		cmd.Env = flattenEnv(spec.Env)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	w := &chunkWriter{fn: spec.OnChunk}
	cmd.Stdout = w
	cmd.Stderr = w

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting command: %w", err)
	}

	// cmd.Wait() also reaps the process (required to avoid a zombie) and
	// waits for the stdout/stderr copying goroutines to finish, so it must
	// always be called exactly once and always be drained - done is
	// buffered so the goroutine can never block on a send nobody reads.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var timeoutC <-chan time.Time
	if spec.Timeout > 0 {
		timer := time.NewTimer(spec.Timeout)
		defer timer.Stop()
		timeoutC = timer.C
	}

	result := &ExecResult{}

	select {
	case <-done:
		// Process exited on its own; nothing to kill.
	case <-timeoutC:
		result.TimedOut = true
		killProcessGroup(cmd)
		<-done // reap
	case <-ctx.Done():
		result.Cancelled = true
		killProcessGroup(cmd)
		<-done // reap
	}

	result.ExitCode = cmd.ProcessState.ExitCode()
	return result, nil
}

// killProcessGroup sends SIGKILL to the negative PID, i.e. the whole process
// group Setpgid created, so children the command spawned (e.g. `cmd &`
// background jobs) are killed too, not just the immediate `bash -c`
// process. A kill on an already-exited group (ESRCH) is not an error here:
// the goal (process gone) is already achieved.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// flattenEnv converts an env map into a KEY=VALUE slice suitable for
// exec.Cmd.Env. Ordering is not deterministic (map iteration) and does not
// need to be - exec.Cmd.Env accepts entries in any order.
func flattenEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
