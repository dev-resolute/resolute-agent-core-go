package tools

import (
	"context"
	"fmt"

	pi "github.com/dev-resolute/resolute-agent-core-go"
)

// write.go ports packages/agent/src/harness/tools/write.ts from upstream pi
// @0.82.0: the "write" model-facing tool - write content to a file,
// creating parent directories automatically (free from env.WriteFile, see
// env.go), serialized through the mutation queue (mutation_queue.go) so
// concurrent tool calls targeting the same file never race.
//
// Deviation from upstream: every failure (path resolution, the write
// itself, the mutation queue's own key-resolution step) is returned as
// pi.ToolResult{IsError: true, Content: <message>} rather than a bubbled Go
// error - upstream's execute throws, and its harness uniformly converts any
// thrown error into an error tool result regardless of origin, so returning
// the same shape directly (instead of relying on a caller to convert a
// returned error) keeps that behavior whether this tool is invoked through
// the full agent loop or called directly.

// WriteToolOptions configures NewWriteTool.
type WriteToolOptions struct {
	// Env is the filesystem seam the tool writes through.
	Env ExecutionEnv
}

// writeParams are the model-supplied arguments to the "write" tool.
type writeParams struct {
	Path    string `json:"path" jsonschema:"description=Path to the file to write (relative or absolute)"`
	Content string `json:"content" jsonschema:"description=Content to write to the file"`
}

// writeToolDescription is the "write" tool's model-facing description,
// ported verbatim from upstream's createWriteTool.
const writeToolDescription = "Write content to a file. Creates the file if it doesn't exist, overwrites if it does. Automatically creates parent directories."

// operationAbortedMessage is upstream's `new Error("Operation aborted")`
// text (write.ts:29,32), ported verbatim - it's checked both before and
// after the write inside the mutation queue (see NewWriteTool), matching
// upstream's `signal?.aborted` checks at the same two points.
const operationAbortedMessage = "Operation aborted"

// NewWriteTool creates the "write" tool: write content to a file, creating
// the file (and its parent directories) if it doesn't exist and overwriting
// it if it does. Ported from upstream's createWriteTool.
func NewWriteTool(opts WriteToolOptions) pi.RegisteredTool {
	env := opts.Env
	return pi.NewTool(pi.Tool[writeParams]{
		Name:        "write",
		Description: writeToolDescription,
		Execute: func(ctx context.Context, p writeParams) (pi.ToolResult, error) {
			absolutePath, err := ResolveToolPath(ctx, env, p.Path)
			if err != nil {
				return pi.ToolResult{IsError: true, Content: err.Error()}, nil
			}

			result, err := withFileMutationQueue(ctx, env, absolutePath, func() (pi.ToolResult, error) {
				// Ported from write.ts:29 - checked once the mutation lock is
				// held (mutation_queue.go's fn is not itself cancellable via
				// ctx once running, but it's free to check ctx.Err() itself,
				// same as upstream checking signal?.aborted inside its own
				// queued closure) and again after the write completes
				// (write.ts:32), so a cancellation racing the write is still
				// caught before reporting success.
				if ctx.Err() != nil {
					return pi.ToolResult{IsError: true, Content: operationAbortedMessage}, nil
				}
				if err := env.WriteFile(ctx, absolutePath, []byte(p.Content)); err != nil {
					return pi.ToolResult{IsError: true, Content: err.Error()}, nil
				}
				if ctx.Err() != nil {
					return pi.ToolResult{IsError: true, Content: operationAbortedMessage}, nil
				}
				// The success message uses p.Path, the model's INPUT path,
				// not absolutePath - matching upstream, which reports back
				// exactly what the caller asked for.
				return pi.ToolResult{
					Content: fmt.Sprintf("Successfully wrote %d bytes to %s", len(p.Content), p.Path),
				}, nil
			})
			if err != nil {
				return pi.ToolResult{IsError: true, Content: err.Error()}, nil
			}
			return result, nil
		},
	})
}
