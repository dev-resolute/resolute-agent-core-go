package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"syscall"

	pi "github.com/dev-resolute/resolute-agent-core-go"
)

// edit.go ports packages/agent/src/harness/tools/edit.ts from upstream pi
// @0.82.0: the "edit" model-facing tool - apply one or more targeted
// oldText -> newText replacements to a file, via the edit-diff engine
// (editdiff.go, Task 9: StripBOM/DetectLineEnding/NormalizeToLF/
// ApplyEditsToNormalizedContent/RestoreLineEndings/GenerateDiffString/
// GenerateUnifiedPatch) for fuzzy-match fallback, BOM/CRLF-preserving I/O,
// and both a display diff and a unified patch attached to the result.
//
// Deviation from upstream (matches write.go/read.go): every failure is
// returned as pi.ToolResult{IsError: true, Content: <message>} rather than a
// bubbled Go error - upstream's execute throws, and its harness uniformly
// converts any thrown error into an error tool result regardless of origin,
// so returning the same shape directly keeps that behavior whether this tool
// is invoked through the full agent loop or called directly.

// EditToolOptions configures NewEditTool.
type EditToolOptions struct {
	// Env is the filesystem seam the tool edits through.
	Env ExecutionEnv
}

// editEntry is one oldText -> newText replacement, as supplied by the
// model. Ported from upstream's replaceEditSchema.
type editEntry struct {
	OldText string `json:"oldText" jsonschema:"description=Exact text for one targeted replacement. It must be unique in the original file and must not overlap with any other edits[].oldText in the same call."`
	NewText string `json:"newText" jsonschema:"description=Replacement text for this targeted edit."`
}

// editParams are the model-supplied arguments to the "edit" tool. Ported
// from upstream's editSchema.
//
// NOTE on Edits' jsonschema tag: its description contains commas, escaped
// here in source as `\\,` (a Go string escape for a literal backslash,
// followed by a literal comma) so that reflect.StructTag.Get - which runs
// the tag's quoted value through strconv.Unquote before invopop ever sees
// it - hands invopop the two literal characters `\,`. invopop/jsonschema's
// own tag parser (splitOnUnescapedCommas, reflect.go) treats an UNescaped
// comma as a tag-option separator, not description text, which would
// otherwise truncate the schema actually exposed to the model at the first
// comma (see TestEditToolSchema, which pins the FULL, untruncated
// description this escaping produces) - full model-facing description
// parity with upstream's editSchema wins over literal, unescaped brief tag
// text. (A single unescaped `\,` is not a valid Go string escape at all -
// go vet's structtag check rejects it outright.)
type editParams struct {
	Path  string      `json:"path" jsonschema:"description=Path to the file to edit (relative or absolute)"`
	Edits []editEntry `json:"edits" jsonschema:"description=One or more targeted replacements. Each edit is matched against the original file\\, not incrementally. Do not include overlapping or nested edits. If two changes touch the same block or nearby lines\\, merge them into one edit instead."`
}

// editToolDescription is the "edit" tool's model-facing description, ported
// verbatim from upstream's createEditTool.
const editToolDescription = "Edit a single file using exact text replacement. Every edits[].oldText must match a unique, non-overlapping region of the original file. If two changes affect the same block or nearby lines, merge them into one edit instead of emitting overlapping edits. Do not include large unchanged regions just to connect distant changes."

// editInputInvalidMessage is upstream's `new Error("Edit tool input is
// invalid. edits must contain at least one replacement.")` text
// (validateEditInput, edit.ts), ported verbatim. It is checked at the tool
// layer (NewEditTool's Execute, mirroring upstream's layering: execute calls
// validateEditInput before resolveToolPath) - editdiff.go's
// ApplyEditsToNormalizedContent also guards a zero-length edits slice with
// this exact message (see its doc comment), so this check is redundant with
// the engine's own, not the only place it could be caught; it is kept here
// anyway to match upstream's structure (reject before touching the
// filesystem at all) rather than relying on the engine to catch it later.
const editInputInvalidMessage = "Edit tool input is invalid. edits must contain at least one replacement."

// editToolDetails is the JSON shape written to ToolResult.Data, mirroring
// upstream's EditToolDetails interface field-for-field.
type editToolDetails struct {
	Diff             string `json:"diff"`
	Patch            string `json:"patch"`
	FirstChangedLine int    `json:"firstChangedLine"`
}

// NewEditTool creates the "edit" tool: apply one or more exact-text
// replacements to a file. Ported from upstream's createEditTool.
func NewEditTool(opts EditToolOptions) pi.RegisteredTool {
	env := opts.Env
	return pi.NewTool(pi.Tool[editParams]{
		Name:             "edit",
		Description:      editToolDescription,
		PrepareArguments: prepareEditArguments,
		Execute: func(ctx context.Context, p editParams) (pi.ToolResult, error) {
			// Ported from edit.ts's validateEditInput, called before
			// resolveToolPath - see editInputInvalidMessage's doc comment.
			if len(p.Edits) == 0 {
				return pi.ToolResult{IsError: true, Content: editInputInvalidMessage}, nil
			}

			absolutePath, err := ResolveToolPath(ctx, env, p.Path)
			if err != nil {
				return pi.ToolResult{IsError: true, Content: err.Error()}, nil
			}

			result, err := withFileMutationQueue(ctx, env, absolutePath, func() (pi.ToolResult, error) {
				return runEdit(ctx, env, p, absolutePath)
			})
			if err != nil {
				return pi.ToolResult{IsError: true, Content: err.Error()}, nil
			}
			return result, nil
		},
	})
}

// runEdit performs the edit itself, once the mutation queue lock for
// absolutePath is held. Ported from edit.ts's withFileMutationQueue
// callback: abort checks at the same four points upstream checks
// signal?.aborted (entry, after read, after applying edits, after write),
// FileInfo kind check (file|symlink only), read -> BOM/line-ending
// normalize -> apply edits -> restore BOM/line-endings -> write, then
// render both diff formats into the result.
func runEdit(ctx context.Context, env ExecutionEnv, p editParams, absolutePath string) (pi.ToolResult, error) {
	if ctx.Err() != nil {
		return pi.ToolResult{IsError: true, Content: operationAbortedMessage}, nil
	}

	info, err := env.FileInfo(ctx, absolutePath)
	if err != nil {
		return pi.ToolResult{IsError: true, Content: editAccessErrorMessage(p.Path, err)}, nil
	}
	if info.Kind != FileKindFile && info.Kind != FileKindSymlink {
		return pi.ToolResult{IsError: true, Content: fmt.Sprintf("Could not edit file: %s. Path is not a file.", p.Path)}, nil
	}

	data, err := env.ReadFile(ctx, absolutePath)
	if err != nil {
		return pi.ToolResult{IsError: true, Content: editAccessErrorMessage(p.Path, err)}, nil
	}
	if ctx.Err() != nil {
		return pi.ToolResult{IsError: true, Content: operationAbortedMessage}, nil
	}

	bom, content := StripBOM(string(data))
	originalEnding := DetectLineEnding(content)
	normalizedContent := NormalizeToLF(content)

	edits := make([]Edit, len(p.Edits))
	for i, e := range p.Edits {
		edits[i] = Edit{OldText: e.OldText, NewText: e.NewText}
	}
	baseContent, newContent, err := ApplyEditsToNormalizedContent(normalizedContent, edits, p.Path)
	if err != nil {
		return pi.ToolResult{IsError: true, Content: err.Error()}, nil
	}
	if ctx.Err() != nil {
		return pi.ToolResult{IsError: true, Content: operationAbortedMessage}, nil
	}

	finalContent := bom + RestoreLineEndings(newContent, originalEnding)
	if err := env.WriteFile(ctx, absolutePath, []byte(finalContent)); err != nil {
		return pi.ToolResult{IsError: true, Content: editAccessErrorMessage(p.Path, err)}, nil
	}
	if ctx.Err() != nil {
		return pi.ToolResult{IsError: true, Content: operationAbortedMessage}, nil
	}

	diff, firstChangedLine := GenerateDiffString(baseContent, newContent)
	patch := GenerateUnifiedPatch(p.Path, baseContent, newContent, 0)
	details, err := json.Marshal(editToolDetails{Diff: diff, Patch: patch, FirstChangedLine: firstChangedLine})
	if err != nil {
		return pi.ToolResult{IsError: true, Content: err.Error()}, nil
	}

	return pi.ToolResult{
		Content: fmt.Sprintf("Successfully replaced %d block(s) in %s.", len(p.Edits), p.Path),
		Data:    details,
	}, nil
}

// editAccessErrorMessage formats the message upstream's editAccessError
// produces for a fileInfo/read/write failure that carries an underlying
// FileError: `Could not edit file: ${path}. Error code: ${error.code}.`
// (edit.ts). path is the model's INPUT path, matching upstream (which
// closes over the un-resolved `path` variable, not the resolved
// absolutePath).
func editAccessErrorMessage(path string, err error) string {
	return fmt.Sprintf("Could not edit file: %s. Error code: %s.", path, fileErrorCode(err))
}

// fileErrorCode maps a Go filesystem error to the small, stable error-code
// vocabulary upstream's FileError.code exposes (see
// packages/agent/src/harness/types.ts's FileErrorCode and
// packages/agent/src/harness/env/nodejs.ts's toFileError, @0.82.0): a
// backend-independent name for the KIND of failure, not a raw platform
// errno. This Go ExecutionEnv seam has no typed FileError of its own (its
// methods return plain `error`), so this reproduces the errno-mapped codes
// an OSEnv-backed failure can actually surface:
//
//   - "not_found" - ENOENT (e.g. editing a file that doesn't exist).
//   - "permission_denied" - EACCES/EPERM.
//   - "not_directory" - ENOTDIR (e.g. editing "file.txt/nested" where
//     file.txt is itself a regular file, not a directory - Go's os.Lstat
//     doesn't distinguish this from ENOENT via fs.ErrNotExist, so it needs
//     its own errors.Is check against syscall.ENOTDIR).
//   - "is_directory" - EISDIR.
//   - "unknown" - upstream's own catch-all for anything else, exactly as
//     toFileError's default branch does for an unmapped errno.
//
// Two of upstream's FileErrorCode values are deliberately NOT produced here:
// "aborted" is handled separately in this file via ctx.Err() checks against
// operationAbortedMessage, never through a FileError-shaped code; "invalid"
// (EINVAL) and "not_supported" have no OSEnv call site in this package that
// is known to produce them today, so mapping them would be speculative
// rather than reproducing an observed behavior.
func fileErrorCode(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "not_found"
	case errors.Is(err, fs.ErrPermission):
		return "permission_denied"
	case errors.Is(err, syscall.ENOTDIR):
		return "not_directory"
	case errors.Is(err, syscall.EISDIR):
		return "is_directory"
	default:
		return "unknown"
	}
}

// decodeJSONString reports whether raw is present and is a JSON string
// literal (not absent, not null, not any other JSON type), returning its
// decoded value. Unmarshaling into *string (rather than string) is what
// makes a JSON null distinguishable from an omitted key: null decodes into
// a nil pointer without error, same as an absent key, whereas any non-string
// non-null JSON value fails to unmarshal into *string at all. Shared by
// prepareEditArguments's three `typeof x === "string"` checks (edits,
// oldText, newText).
func decodeJSONString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s *string
	if err := json.Unmarshal(raw, &s); err != nil || s == nil {
		return "", false
	}
	return *s, true
}

// prepareEditArguments is the edit tool's PrepareArguments hook, ported
// verbatim (field-for-field, branch-for-branch) from upstream's
// prepareEditArguments (edit.ts). It handles two legacy/loose argument
// shapes older or less careful callers may still send:
//
//  1. edits encoded as a JSON string rather than a JSON array - decoded in
//     place if (and only if) it parses to a JSON array; a string that fails
//     to parse, or that parses to something other than an array, is left
//     untouched (matching upstream's try/catch-with-Array.isArray guard).
//  2. a single edit passed as top-level oldText/newText fields (predating
//     the edits[] array shape) - folded into a one-entry edits[] array,
//     APPENDED after any edits[] already present, with the top-level
//     oldText/newText keys removed from the result. This only fires when
//     BOTH oldText and newText are present and are JSON strings; if either
//     is missing/non-string, the (possibly edits-decoded) input is returned
//     unchanged.
//
// Non-object input (raw isn't a JSON object) is returned unchanged, matching
// upstream's `!input || typeof input !== "object"` early return - the
// downstream json.Unmarshal into editParams surfaces any resulting shape
// mismatch on its own.
func prepareEditArguments(_ context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var args map[string]json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil {
		return raw, nil
	}

	if editsAsString, ok := decodeJSONString(args["edits"]); ok {
		var parsedArray []json.RawMessage
		if err := json.Unmarshal([]byte(editsAsString), &parsedArray); err == nil {
			if reencoded, err := json.Marshal(parsedArray); err == nil {
				args["edits"] = reencoded
			}
		}
	}

	oldText, hasOldText := decodeJSONString(args["oldText"])
	newText, hasNewText := decodeJSONString(args["newText"])
	if !hasOldText || !hasNewText {
		return json.Marshal(args)
	}

	// Array.isArray(legacy.edits) ? [...legacy.edits] : [] - a missing or
	// non-array args["edits"] fails to unmarshal here and is ignored,
	// leaving edits nil (Go's nil slice behaves as [] for append below).
	var edits []json.RawMessage
	_ = json.Unmarshal(args["edits"], &edits)

	legacyEdit, err := json.Marshal(editEntry{OldText: oldText, NewText: newText})
	if err != nil {
		return nil, err
	}
	edits = append(edits, legacyEdit)

	delete(args, "oldText")
	delete(args, "newText")
	editsJSON, err := json.Marshal(edits)
	if err != nil {
		return nil, err
	}
	args["edits"] = editsJSON

	return json.Marshal(args)
}
