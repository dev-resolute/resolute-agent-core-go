package tools

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// env_test.go exercises the ExecutionEnv seam via its OSEnv implementation.
// Upstream reference for behavior: packages/agent/src/harness/env/nodejs.ts
// @0.82.0 (NodeExecutionEnv) - in particular its process-group kill via
// process.kill(-pid, "SIGKILL"), mirrored here by env_unix.go's
// killProcessGroup. Timing-sensitive cases (timeout, cancellation,
// process-group kill) use generous margins and bound their own worst-case
// runtime via a select-with-timeout, rather than asserting exact durations,
// so they stay stable under -race on a loaded machine.

func newTestOSEnv(t *testing.T) (*OSEnv, string) {
	t.Helper()
	dir := t.TempDir()
	env, err := NewOSEnv(dir)
	if err != nil {
		t.Fatalf("NewOSEnv(%q) error: %v", dir, err)
	}
	return env, dir
}

func TestNewOSEnv(t *testing.T) {
	t.Run("resolves and stats an existing directory", func(t *testing.T) {
		dir := t.TempDir()
		env, err := NewOSEnv(dir)
		if err != nil {
			t.Fatalf("NewOSEnv(%q) error: %v", dir, err)
		}
		wantAbs, err := filepath.Abs(dir)
		if err != nil {
			t.Fatalf("filepath.Abs(%q) error: %v", dir, err)
		}
		if env.Cwd() != wantAbs {
			t.Errorf("Cwd() = %q, want %q", env.Cwd(), wantAbs)
		}
	})

	t.Run("errors on a nonexistent directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "does-not-exist")
		if _, err := NewOSEnv(dir); err == nil {
			t.Errorf("NewOSEnv(%q) error = nil, want non-nil", dir)
		}
	})

	t.Run("errors when cwd is a file, not a directory", func(t *testing.T) {
		dir := t.TempDir()
		file := filepath.Join(dir, "not-a-dir")
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatalf("os.WriteFile(%q) error: %v", file, err)
		}
		if _, err := NewOSEnv(file); err == nil {
			t.Errorf("NewOSEnv(%q) error = nil, want non-nil", file)
		}
	})
}

func TestOSEnvAbsolutePath(t *testing.T) {
	env, dir := newTestOSEnv(t)
	ctx := context.Background()

	tests := []struct {
		name string
		path string
		want string
	}{
		{"relative resolves against Cwd", "sub/file.txt", filepath.Join(dir, "sub/file.txt")},
		{"absolute is returned unchanged (cleaned)", filepath.Join(dir, "abs.txt"), filepath.Join(dir, "abs.txt")},
		{"relative with dot-dot cleans", "sub/../file.txt", filepath.Join(dir, "file.txt")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := env.AbsolutePath(ctx, tc.path)
			if err != nil {
				t.Fatalf("AbsolutePath(%q) error: %v", tc.path, err)
			}
			if got != tc.want {
				t.Errorf("AbsolutePath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}

	t.Run("honors an already-cancelled context", func(t *testing.T) {
		cctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := env.AbsolutePath(cctx, "x"); err == nil {
			t.Errorf("AbsolutePath with cancelled ctx error = nil, want non-nil")
		}
	})
}

func TestOSEnvCanonicalPath(t *testing.T) {
	env, dir := newTestOSEnv(t)
	ctx := context.Background()

	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("hi"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(%q) error: %v", target, err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("os.Symlink error: %v", err)
	}

	got, err := env.CanonicalPath(ctx, "link.txt")
	if err != nil {
		t.Fatalf("CanonicalPath(%q) error: %v", "link.txt", err)
	}
	wantTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks(%q) error: %v", target, err)
	}
	if got != wantTarget {
		t.Errorf("CanonicalPath(%q) = %q, want %q", "link.txt", got, wantTarget)
	}

	t.Run("errors on a path that does not exist", func(t *testing.T) {
		if _, err := env.CanonicalPath(ctx, "missing.txt"); err == nil {
			t.Errorf("CanonicalPath(missing) error = nil, want non-nil")
		}
	})
}

func TestOSEnvExists(t *testing.T) {
	env, dir := newTestOSEnv(t)
	ctx := context.Background()

	present := filepath.Join(dir, "present.txt")
	if err := os.WriteFile(present, []byte("x"), 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	broken := filepath.Join(dir, "broken-link")
	if err := os.Symlink(filepath.Join(dir, "does-not-exist"), broken); err != nil {
		t.Fatalf("os.Symlink error: %v", err)
	}

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"existing file", "present.txt", true},
		{"missing file", "absent.txt", false},
		{"broken symlink still exists (lstat)", "broken-link", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := env.Exists(ctx, tc.path)
			if err != nil {
				t.Fatalf("Exists(%q) error: %v", tc.path, err)
			}
			if got != tc.want {
				t.Errorf("Exists(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestOSEnvFileInfo(t *testing.T) {
	env, dir := newTestOSEnv(t)
	ctx := context.Background()

	file := filepath.Join(dir, "file.txt")
	content := []byte("hello world")
	if err := os.WriteFile(file, content, 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	subdir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatalf("os.Mkdir error: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatalf("os.Symlink error: %v", err)
	}

	tests := []struct {
		name     string
		path     string
		wantKind FileKind
		wantSize int64 // -1 skips the size assertion
	}{
		{"regular file", "file.txt", FileKindFile, int64(len(content))},
		{"directory", "subdir", FileKindDir, -1},
		{"symlink is reported as symlink, not followed", "link", FileKindSymlink, -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := env.FileInfo(ctx, tc.path)
			if err != nil {
				t.Fatalf("FileInfo(%q) error: %v", tc.path, err)
			}
			if got.Kind != tc.wantKind {
				t.Errorf("FileInfo(%q).Kind = %q, want %q", tc.path, got.Kind, tc.wantKind)
			}
			if tc.wantSize >= 0 && got.Size != tc.wantSize {
				t.Errorf("FileInfo(%q).Size = %d, want %d", tc.path, got.Size, tc.wantSize)
			}
		})
	}
}

func TestOSEnvReadFile(t *testing.T) {
	env, dir := newTestOSEnv(t)
	ctx := context.Background()

	file := filepath.Join(dir, "file.txt")
	content := []byte("hello\nworld\n")
	if err := os.WriteFile(file, content, 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}

	got, err := env.ReadFile(ctx, "file.txt")
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("ReadFile = %q, want %q", got, content)
	}

	t.Run("errors on a missing file", func(t *testing.T) {
		if _, err := env.ReadFile(ctx, "missing.txt"); err == nil {
			t.Errorf("ReadFile(missing) error = nil, want non-nil")
		}
	})
}

func TestOSEnvWriteFile(t *testing.T) {
	env, dir := newTestOSEnv(t)
	ctx := context.Background()

	t.Run("creates parent directories", func(t *testing.T) {
		path := "a/b/c/file.txt"
		if err := env.WriteFile(ctx, path, []byte("content")); err != nil {
			t.Fatalf("WriteFile(%q) error: %v", path, err)
		}
		got, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			t.Fatalf("os.ReadFile error: %v", err)
		}
		if string(got) != "content" {
			t.Errorf("file content = %q, want %q", got, "content")
		}
	})

	t.Run("overwrites an existing file", func(t *testing.T) {
		path := "overwrite.txt"
		if err := env.WriteFile(ctx, path, []byte("first")); err != nil {
			t.Fatalf("WriteFile error: %v", err)
		}
		if err := env.WriteFile(ctx, path, []byte("second")); err != nil {
			t.Fatalf("WriteFile error: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			t.Fatalf("os.ReadFile error: %v", err)
		}
		if string(got) != "second" {
			t.Errorf("file content = %q, want %q", got, "second")
		}
	})
}

func TestOSEnvAppendFile(t *testing.T) {
	env, dir := newTestOSEnv(t)
	ctx := context.Background()

	t.Run("appends to an existing file", func(t *testing.T) {
		path := "append.txt"
		if err := env.WriteFile(ctx, path, []byte("first\n")); err != nil {
			t.Fatalf("WriteFile error: %v", err)
		}
		if err := env.AppendFile(ctx, path, []byte("second\n")); err != nil {
			t.Fatalf("AppendFile error: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			t.Fatalf("os.ReadFile error: %v", err)
		}
		want := "first\nsecond\n"
		if string(got) != want {
			t.Errorf("file content = %q, want %q", got, want)
		}
	})

	t.Run("creates a new file (and parents) when absent", func(t *testing.T) {
		path := "new/nested/append.txt"
		if err := env.AppendFile(ctx, path, []byte("only\n")); err != nil {
			t.Fatalf("AppendFile error: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			t.Fatalf("os.ReadFile error: %v", err)
		}
		if string(got) != "only\n" {
			t.Errorf("file content = %q, want %q", got, "only\n")
		}
	})
}

func TestOSEnvCreateTemp(t *testing.T) {
	env, _ := newTestOSEnv(t)
	ctx := context.Background()

	path, err := env.CreateTemp(ctx, "bash-", ".log")
	if err != nil {
		t.Fatalf("CreateTemp error: %v", err)
	}
	t.Cleanup(func() { os.Remove(path) })

	base := filepath.Base(path)
	if !strings.HasPrefix(base, "bash-") {
		t.Errorf("CreateTemp name %q does not have prefix %q", base, "bash-")
	}
	if !strings.HasSuffix(base, ".log") {
		t.Errorf("CreateTemp name %q does not have suffix %q", base, ".log")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("CreateTemp did not create an existing file: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("CreateTemp file size = %d, want 0", info.Size())
	}

	t.Run("two calls produce distinct paths", func(t *testing.T) {
		other, err := env.CreateTemp(ctx, "bash-", ".log")
		if err != nil {
			t.Fatalf("CreateTemp error: %v", err)
		}
		t.Cleanup(func() { os.Remove(other) })
		if other == path {
			t.Errorf("CreateTemp produced the same path twice: %q", path)
		}
	})
}

func TestOSEnvExecEchoAndExitCode(t *testing.T) {
	env, _ := newTestOSEnv(t)
	ctx := context.Background()

	t.Run("echo hi delivers merged output via OnChunk and exits 0", func(t *testing.T) {
		var mu sync.Mutex
		var buf bytes.Buffer
		result, err := env.Exec(ctx, ExecSpec{
			Command: "echo hi",
			OnChunk: func(chunk []byte) {
				mu.Lock()
				defer mu.Unlock()
				buf.Write(chunk)
			},
		})
		if err != nil {
			t.Fatalf("Exec error: %v", err)
		}
		if result.ExitCode != 0 {
			t.Errorf("ExitCode = %d, want 0", result.ExitCode)
		}
		if result.Cancelled || result.TimedOut {
			t.Errorf("Cancelled=%v TimedOut=%v, want both false", result.Cancelled, result.TimedOut)
		}
		mu.Lock()
		got := buf.String()
		mu.Unlock()
		if got != "hi\n" {
			t.Errorf("merged OnChunk output = %q, want %q", got, "hi\n")
		}
	})

	t.Run("exit 3 reports ExitCode 3", func(t *testing.T) {
		result, err := env.Exec(ctx, ExecSpec{Command: "exit 3"})
		if err != nil {
			t.Fatalf("Exec error: %v", err)
		}
		if result.ExitCode != 3 {
			t.Errorf("ExitCode = %d, want 3", result.ExitCode)
		}
		if result.Cancelled || result.TimedOut {
			t.Errorf("Cancelled=%v TimedOut=%v, want both false", result.Cancelled, result.TimedOut)
		}
	})
}

func TestOSEnvExecTimeout(t *testing.T) {
	env, _ := newTestOSEnv(t)
	ctx := context.Background()

	start := time.Now()
	resultCh := make(chan *ExecResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := env.Exec(ctx, ExecSpec{Command: "sleep 5", Timeout: 100 * time.Millisecond})
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- result
	}()

	select {
	case err := <-errCh:
		t.Fatalf("Exec error: %v", err)
	case result := <-resultCh:
		elapsed := time.Since(start)
		if !result.TimedOut {
			t.Errorf("TimedOut = false, want true")
		}
		if result.Cancelled {
			t.Errorf("Cancelled = true, want false")
		}
		// Generous margin: proves the timeout fires promptly (not that it
		// waited out the full 5s sleep) without pinning an exact duration
		// that could flake on a loaded machine.
		if elapsed > 2*time.Second {
			t.Errorf("Exec with 100ms timeout returned after %v, want < 2s", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Exec did not return within 5s of a 100ms timeout - likely failed to kill the process")
	}
}

func TestOSEnvExecCancelledContext(t *testing.T) {
	env, _ := newTestOSEnv(t)
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	resultCh := make(chan *ExecResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := env.Exec(cctx, ExecSpec{Command: "sleep 5"})
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- result
	}()

	// Give the process a moment to actually start before cancelling, so
	// this exercises "kill a running process" rather than "never start it".
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		t.Fatalf("Exec error: %v", err)
	case result := <-resultCh:
		elapsed := time.Since(start)
		if !result.Cancelled {
			t.Errorf("Cancelled = false, want true")
		}
		if result.TimedOut {
			t.Errorf("TimedOut = true, want false")
		}
		if elapsed > 2*time.Second {
			t.Errorf("Exec with cancelled ctx returned after %v, want < 2s", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Exec did not return within 5s of ctx cancellation - likely failed to kill the process")
	}
}

func TestOSEnvExecNonInheritedEnv(t *testing.T) {
	env, _ := newTestOSEnv(t)
	ctx := context.Background()

	var mu sync.Mutex
	var buf bytes.Buffer
	result, err := env.Exec(ctx, ExecSpec{
		Command:    "env",
		Env:        map[string]string{"ONLY": "x"},
		InheritEnv: false,
		OnChunk: func(chunk []byte) {
			mu.Lock()
			defer mu.Unlock()
			buf.Write(chunk)
		},
	})
	if err != nil {
		t.Fatalf("Exec error: %v", err)
	}
	mu.Lock()
	output := buf.String()
	mu.Unlock()
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (output so far: %q)", result.ExitCode, output)
	}
	if !strings.Contains(output, "ONLY=x") {
		t.Errorf("env output = %q, want it to contain %q", output, "ONLY=x")
	}
	if strings.Contains(output, "HOME=") {
		t.Errorf("env output = %q, want it to NOT contain %q (InheritEnv:false leaked the parent's HOME)", output, "HOME=")
	}
}

func TestOSEnvExecInheritedEnv(t *testing.T) {
	env, _ := newTestOSEnv(t)
	ctx := context.Background()

	t.Setenv("RESOLUTE_TOOLS_ENV_TEST_MARKER", "present")

	var mu sync.Mutex
	var buf bytes.Buffer
	result, err := env.Exec(ctx, ExecSpec{
		Command:    "env",
		Env:        map[string]string{"ONLY": "x"},
		InheritEnv: true,
		OnChunk: func(chunk []byte) {
			mu.Lock()
			defer mu.Unlock()
			buf.Write(chunk)
		},
	})
	if err != nil {
		t.Fatalf("Exec error: %v", err)
	}
	mu.Lock()
	output := buf.String()
	mu.Unlock()
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
	if !strings.Contains(output, "ONLY=x") {
		t.Errorf("env output missing ONLY=x: %q", output)
	}
	if !strings.Contains(output, "RESOLUTE_TOOLS_ENV_TEST_MARKER=present") {
		t.Errorf("env output missing inherited RESOLUTE_TOOLS_ENV_TEST_MARKER=present: %q", output)
	}
}

func TestOSEnvExecHonorsDirOverride(t *testing.T) {
	env, dir := newTestOSEnv(t)
	ctx := context.Background()

	subdir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatalf("os.Mkdir error: %v", err)
	}
	realSubdir, err := filepath.EvalSymlinks(subdir)
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks error: %v", err)
	}

	var mu sync.Mutex
	var buf bytes.Buffer
	result, err := env.Exec(ctx, ExecSpec{
		Command: "pwd",
		Dir:     subdir,
		OnChunk: func(chunk []byte) {
			mu.Lock()
			defer mu.Unlock()
			buf.Write(chunk)
		},
	})
	if err != nil {
		t.Fatalf("Exec error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
	mu.Lock()
	got := strings.TrimSpace(buf.String())
	mu.Unlock()
	if got != realSubdir {
		t.Errorf("pwd in Dir override = %q, want %q", got, realSubdir)
	}
}

// TestOSEnvExecKillsProcessGroup pins the process-group-kill contract: a
// command that backgrounds a child and waits on it (`sleep N & wait`) must
// be killed IN ITS ENTIRETY on timeout, not just the immediate `bash -c`
// process. If only the immediate process were killed, `wait` would never
// return, but the backgrounded `sleep` would keep running as an orphan
// (or Exec itself would hang, since exec.Cmd.Wait blocks until stdout/
// stderr pipes close, which they only do once every process sharing the
// fds - including the orphan - has exited).
func TestOSEnvExecKillsProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not available, cannot verify process-group cleanup")
	}

	env, _ := newTestOSEnv(t)
	ctx := context.Background()

	// A distinctive sleep duration used purely as a pgrep marker. It is
	// never expected to actually elapse: Timeout below kills the whole
	// process group well before then.
	const marker = "424242"

	start := time.Now()
	resultCh := make(chan *ExecResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := env.Exec(ctx, ExecSpec{
			Command: "sleep " + marker + " & wait",
			Timeout: 300 * time.Millisecond,
		})
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- result
	}()

	select {
	case err := <-errCh:
		t.Fatalf("Exec error: %v", err)
	case result := <-resultCh:
		elapsed := time.Since(start)
		if !result.TimedOut {
			t.Errorf("TimedOut = false, want true")
		}
		if elapsed > 2*time.Second {
			t.Errorf("Exec returned after %v, want < 2s (process-group kill should be prompt)", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Exec did not return within 5s - the backgrounded child likely kept the parent alive (process-group kill not working)")
	}

	// Directly confirm the backgrounded grandchild is actually gone, not
	// merely that our function returned - this is the real proof that
	// SIGKILL reached the whole process group (no orphan left running, and
	// since we always reap via cmd.Wait, no zombie either).
	deadline := time.Now().Add(2 * time.Second)
	for {
		out, _ := exec.Command("pgrep", "-f", "sleep "+marker).Output()
		if len(strings.TrimSpace(string(out))) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sleep %s is still running 2s after Exec returned - process group was not killed (pgrep output: %q)", marker, out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// countOpenFDs shells out to lsof to count this test process's open file
// descriptors. It skips (rather than fails) when lsof isn't available,
// since it's a self-review diagnostic, not a load-bearing behavior test.
func countOpenFDs(t *testing.T) int {
	t.Helper()
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not available, cannot verify fd leaks")
	}
	out, err := exec.Command("lsof", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		t.Skipf("lsof failed: %v", err)
	}
	return len(strings.Split(strings.TrimSpace(string(out)), "\n"))
}

// TestOSEnvExecNoGoroutineLeak and TestOSEnvExecNoFDLeak are self-review
// diagnostics (not brief-mandated cases): they run a batch of Exec calls
// covering the normal-exit, ctx-cancelled, and timeout paths, then assert
// the goroutine count and open-fd count return to baseline. Both the
// "done" channel (buffered, always drained - see env_unix.go's Exec) and
// cmd.Wait()'s pipe reaping are only correct if every code path joins the
// background goroutine and every stdio pipe gets closed; these tests catch
// a regression that a black-box "does it return the right ExecResult" test
// would miss.
func TestOSEnvExecNoGoroutineLeak(t *testing.T) {
	env, _ := newTestOSEnv(t)

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	for i := 0; i < 20; i++ {
		if _, err := env.Exec(context.Background(), ExecSpec{Command: "echo hi"}); err != nil {
			t.Fatalf("Exec error: %v", err)
		}
	}
	for i := 0; i < 10; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(30 * time.Millisecond)
			cancel()
		}()
		if _, err := env.Exec(ctx, ExecSpec{Command: "sleep 5"}); err != nil {
			t.Fatalf("Exec error: %v", err)
		}
	}
	for i := 0; i < 10; i++ {
		if _, err := env.Exec(context.Background(), ExecSpec{Command: "sleep 5", Timeout: 30 * time.Millisecond}); err != nil {
			t.Fatalf("Exec error: %v", err)
		}
	}

	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	after := runtime.NumGoroutine()

	if after > before+2 { // small slack for the Go test runtime's own scheduling goroutines
		t.Errorf("goroutine count grew from %d to %d after 40 Exec calls (incl. timeouts/cancels) - possible leak", before, after)
	}
}

func TestOSEnvExecNoFDLeak(t *testing.T) {
	env, _ := newTestOSEnv(t)

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	before := countOpenFDs(t)

	for i := 0; i < 20; i++ {
		if _, err := env.Exec(context.Background(), ExecSpec{Command: "echo hi"}); err != nil {
			t.Fatalf("Exec error: %v", err)
		}
	}
	for i := 0; i < 10; i++ {
		if _, err := env.Exec(context.Background(), ExecSpec{Command: "sleep 5", Timeout: 30 * time.Millisecond}); err != nil {
			t.Fatalf("Exec error: %v", err)
		}
	}

	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	after := countOpenFDs(t)

	if after > before+5 { // small slack for lsof's own transient fds
		t.Errorf("open FD count grew from %d to %d after 30 Exec calls - possible fd leak", before, after)
	}
}
