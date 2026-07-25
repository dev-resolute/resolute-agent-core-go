package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// env.go defines the ExecutionEnv seam: the filesystem/shell boundary that
// built-in tools (read/write/edit/bash, ported in later tasks) run over,
// plus OSEnv, the local-process implementation of it. Splitting the seam
// (this file) from the OS-process-specific Exec implementation
// (env_unix.go, //go:build unix) keeps the filesystem methods portable while
// isolating the process-group-kill logic that only makes sense on POSIX.
//
// Upstream reference: packages/agent/src/harness/env/nodejs.ts @0.82.0
// (NodeExecutionEnv) for behavior and the process-group-kill approach
// (process.kill(-pid, "SIGKILL")); this port narrows the interface to what
// the built-in tools in this repo need (no listDir/createDir/remove — not
// yet consumed) and adds AppendFile, which upstream also has but which
// wasn't in the originally-drafted seam.

// FileKind identifies what kind of filesystem entry FileInfo describes.
type FileKind string

const (
	// FileKindFile is a regular file.
	FileKindFile FileKind = "file"
	// FileKindDir is a directory.
	FileKindDir FileKind = "dir"
	// FileKindSymlink is a symbolic link (not followed).
	FileKindSymlink FileKind = "symlink"
)

// FileInfo describes a filesystem entry as reported by ExecutionEnv.FileInfo.
type FileInfo struct {
	// Kind is the entry's type.
	Kind FileKind
	// Size is the entry's size in bytes, as reported by lstat (i.e. the
	// symlink's own size, not its target's, when Kind is FileKindSymlink).
	Size int64
}

// ExecutionEnv is the filesystem/shell seam the built-in tools run over.
// Implementations MUST be pointer types: the mutation queue keys on instance
// identity. All methods are ctx-first so remote/sandbox adapters can honor
// cancellation.
type ExecutionEnv interface {
	// Cwd returns the environment's working directory, as an absolute path.
	Cwd() string
	// AbsolutePath resolves path to an absolute path, relative to Cwd() if
	// path is not already absolute. It does not touch the filesystem.
	AbsolutePath(ctx context.Context, path string) (string, error)
	// CanonicalPath resolves path (via AbsolutePath) and then resolves any
	// symlinks in it, returning the real underlying path. The path must
	// exist.
	CanonicalPath(ctx context.Context, path string) (string, error)
	// Exists reports whether path exists (following symlinks is not
	// required to succeed: a broken symlink still exists).
	Exists(ctx context.Context, path string) (bool, error)
	// FileInfo stats path (without following a trailing symlink) and
	// returns its kind and size.
	FileInfo(ctx context.Context, path string) (FileInfo, error)
	// ReadFile reads the entire contents of path.
	ReadFile(ctx context.Context, path string) ([]byte, error)
	// WriteFile writes data to path, creating path's parent directories if
	// they don't already exist, and truncating/overwriting any existing
	// file at path.
	WriteFile(ctx context.Context, path string, data []byte) error
	// AppendFile appends data to path, creating the file (and its parent
	// directories) if it doesn't already exist.
	AppendFile(ctx context.Context, path string, data []byte) error
	// CreateTemp creates a new, empty, uniquely-named file (not a
	// directory) named prefix + <random> + suffix, and returns its path.
	CreateTemp(ctx context.Context, prefix, suffix string) (string, error)
	// Exec runs spec.Command as a shell command and returns its result.
	Exec(ctx context.Context, spec ExecSpec) (*ExecResult, error)
}

// ExecSpec configures a call to ExecutionEnv.Exec.
type ExecSpec struct {
	// Command is the shell command to run (via `bash -c`).
	Command string
	// Dir is the working directory for the command. Empty selects the
	// environment's Cwd().
	Dir string
	// Env holds extra environment variables for the command. Their
	// interaction with the inherited environment is controlled by
	// InheritEnv.
	Env map[string]string
	// InheritEnv, when true, runs the command with the calling process's
	// environment plus Env layered on top. When false, the command runs
	// with EXACTLY Env as its environment (nothing inherited).
	InheritEnv bool
	// Timeout bounds how long the command may run. A value <= 0 means no
	// timeout is applied at this layer (validating a user-supplied timeout,
	// e.g. rejecting <= 0, is the caller's responsibility).
	Timeout time.Duration
	// OnChunk, if set, is called with each chunk of output as it arrives.
	// stdout and stderr are merged into a single stream, delivered in
	// arrival order. Calls are serialized (mutex-guarded on the OSEnv
	// implementation): OnChunk is never invoked concurrently with itself,
	// and must not retain the passed slice past the call. Because calls
	// are serialized, a slow or blocking OnChunk stalls output pumping for
	// both stdout and stderr - it holds the underlying writer's lock for
	// the duration of the call.
	OnChunk func(chunk []byte)
}

// ExecResult is the outcome of an ExecutionEnv.Exec call.
type ExecResult struct {
	// ExitCode is the process's exit code. When Cancelled or TimedOut is
	// true, the process was killed rather than exiting normally, and
	// ExitCode reflects whatever the Go runtime reports for a
	// signal-terminated process (-1 on POSIX, via
	// os.ProcessState.ExitCode()) - Cancelled/TimedOut, not ExitCode, are
	// the authoritative signal that the process didn't complete on its
	// own.
	ExitCode int
	// Cancelled reports whether the command was killed because ctx was
	// cancelled.
	Cancelled bool
	// TimedOut reports whether the command was killed because Timeout
	// elapsed.
	TimedOut bool
}

// OSEnv is the ExecutionEnv implementation backed by the local OS process:
// real files, real subprocesses. It is the default environment built-in
// tools run over outside of remote/sandbox adapters.
type OSEnv struct {
	cwd string
}

// NewOSEnv creates an OSEnv rooted at cwd. cwd is resolved to an absolute
// path and must name an existing directory.
func NewOSEnv(cwd string) (*OSEnv, error) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return nil, fmt.Errorf("resolving cwd %q: %w", cwd, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("stat cwd %q: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("cwd %q is not a directory", abs)
	}
	return &OSEnv{cwd: abs}, nil
}

// Cwd returns the environment's working directory.
func (e *OSEnv) Cwd() string { return e.cwd }

// AbsolutePath resolves path against e.Cwd() (if path is relative) and
// cleans the result. It does not touch the filesystem, so it succeeds even
// for paths that don't exist.
func (e *OSEnv) AbsolutePath(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	return filepath.Clean(filepath.Join(e.cwd, path)), nil
}

// CanonicalPath resolves path (via AbsolutePath) and then resolves any
// symlinks in it. path must exist.
func (e *OSEnv) CanonicalPath(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	abs, err := e.AbsolutePath(ctx, path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("canonicalizing %q: %w", abs, err)
	}
	return resolved, nil
}

// Exists reports whether path exists (via lstat, so a broken symlink still
// reports true).
func (e *OSEnv) Exists(ctx context.Context, path string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	abs, err := e.AbsolutePath(ctx, path)
	if err != nil {
		return false, err
	}
	if _, err := os.Lstat(abs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("checking existence of %q: %w", abs, err)
	}
	return true, nil
}

// FileInfo stats path (without following a trailing symlink) and returns
// its kind and size.
func (e *OSEnv) FileInfo(ctx context.Context, path string) (FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return FileInfo{}, err
	}
	abs, err := e.AbsolutePath(ctx, path)
	if err != nil {
		return FileInfo{}, err
	}
	stat, err := os.Lstat(abs)
	if err != nil {
		return FileInfo{}, fmt.Errorf("stat %q: %w", abs, err)
	}
	kind := FileKindFile
	switch {
	case stat.Mode()&os.ModeSymlink != 0:
		kind = FileKindSymlink
	case stat.IsDir():
		kind = FileKindDir
	}
	return FileInfo{Kind: kind, Size: stat.Size()}, nil
}

// ReadFile reads the entire contents of path.
func (e *OSEnv) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	abs, err := e.AbsolutePath(ctx, path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", abs, err)
	}
	return data, nil
}

// WriteFile writes data to path, creating path's parent directories if they
// don't already exist, and truncating/overwriting any existing file at
// path.
func (e *OSEnv) WriteFile(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	abs, err := e.AbsolutePath(ctx, path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("creating parent directories for %q: %w", abs, err)
	}
	if err := os.WriteFile(abs, data, 0o644); err != nil {
		return fmt.Errorf("writing %q: %w", abs, err)
	}
	return nil
}

// AppendFile appends data to path, creating the file (and its parent
// directories) if it doesn't already exist.
func (e *OSEnv) AppendFile(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	abs, err := e.AbsolutePath(ctx, path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("creating parent directories for %q: %w", abs, err)
	}
	f, err := os.OpenFile(abs, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening %q for append: %w", abs, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("appending to %q: %w", abs, err)
	}
	// Close explicitly (no defer): a failed flush on close after a
	// successful Write must still surface as an error, not be swallowed.
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %q after append: %w", abs, err)
	}
	return nil
}

// CreateTemp creates a new, empty, uniquely-named file named
// prefix + <random> + suffix in the system temp directory, and returns its
// path.
func (e *OSEnv) CreateTemp(ctx context.Context, prefix, suffix string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", prefix+"*"+suffix)
	if err != nil {
		return "", fmt.Errorf("creating temp file (prefix %q, suffix %q): %w", prefix, suffix, err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("closing temp file %q: %w", name, err)
	}
	return name, nil
}
