package tools

import (
	"context"
	"errors"
	"io/fs"
	"sync"

	pi "github.com/dev-resolute/resolute-agent-core-go"
)

// mutation_queue.go ports
// packages/agent/src/harness/tools/file-mutation-queue.ts from upstream pi
// @0.82.0: serializing concurrent tool calls that mutate the SAME file (two
// overlapping edit calls, in particular) so a read-modify-write tool
// implementation never races against itself, while calls targeting
// different files proceed fully concurrently.
//
// Design differs from upstream, which hand-rolls a promise-chaining
// "registration" step to atomically get-or-create a per-key queue - JS has
// no compare-and-swap primitive for a Map, so upstream serializes key
// creation itself through a second, outer promise chain. Go's
// sync.Map.LoadOrStore already does exactly that atomically, so this port
// collapses to: resolve the key, atomically get-or-create its *sync.Mutex,
// lock it around fn, unlock when fn returns.
//
// mutationLocks entries are never removed. The map is bounded by the number
// of DISTINCT (env, canonical-path) pairs ever mutated during the process's
// life, not by the number of concurrent callers - a long-running agent
// process editing a bounded set of files across a bounded set of
// ExecutionEnvs never grows this unboundedly in practice. Unbounded growth
// would only become a concern for a process that mutates an unbounded
// number of distinct file paths over its lifetime, which this package does
// not attempt to guard against (documented here rather than solved, per the
// brief).

// mutationKey identifies the file a mutation targets: the ExecutionEnv it
// runs against, plus the resolved path within it. ExecutionEnv
// implementations MUST be pointer types (see ExecutionEnv's doc comment),
// so env is comparable via pointer identity - required for mutationKey to
// be usable as a sync.Map key at all.
type mutationKey struct {
	env  ExecutionEnv
	path string
}

// mutationLocks holds one *sync.Mutex per mutationKey ever seen, created
// lazily on first use and never removed - see the package doc comment.
var mutationLocks sync.Map // mutationKey -> *sync.Mutex

// withFileMutationQueue serializes calls to fn that target the same file:
// concurrent callers resolving to the same (env, path) key run fn one at a
// time (in whatever order they acquire the lock - no ordering guarantee
// beyond mutual exclusion); callers targeting a different file proceed
// fully concurrently, never waiting on each other. Ported from upstream's
// withFileMutationQueue.
//
// ctx is used only to resolve path to its queue key (via env.AbsolutePath /
// env.CanonicalPath, both ctx-aware); once the key is resolved, waiting for
// the lock and running fn are NOT cancellable through ctx - matching
// upstream, whose withFileMutationQueue takes no AbortSignal either.
func withFileMutationQueue(ctx context.Context, env ExecutionEnv, path string, fn func() (pi.ToolResult, error)) (pi.ToolResult, error) {
	key, err := mutationQueueKey(ctx, env, path)
	if err != nil {
		return pi.ToolResult{}, err
	}

	lockAny, _ := mutationLocks.LoadOrStore(mutationKey{env: env, path: key}, &sync.Mutex{})
	lock := lockAny.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	return fn()
}

// mutationQueueKey resolves path to the string that identifies it in
// mutationLocks: its canonical (symlink-resolved) form when it exists, or
// its plain absolute form when it doesn't exist yet to canonicalize (e.g. a
// write tool creating a new file) - mirroring upstream's fallback for its
// canonicalPath call's "not_found"/"not_supported" outcomes. Any other
// resolution error (including a cancelled/expired ctx) is returned as-is.
func mutationQueueKey(ctx context.Context, env ExecutionEnv, path string) (string, error) {
	absolute, err := env.AbsolutePath(ctx, path)
	if err != nil {
		return "", err
	}
	canonical, err := env.CanonicalPath(ctx, absolute)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return absolute, nil
		}
		return "", err
	}
	return canonical, nil
}
