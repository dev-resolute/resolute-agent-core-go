package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	pi "github.com/dev-resolute/resolute-agent-core-go"
	"github.com/dev-resolute/resolute-llm-go"
)

// read.go ports packages/agent/src/harness/tools/read.ts from upstream pi
// @0.82.0: the "read" model-facing tool - read a text or image file. Text
// files are paged via offset/limit (1-indexed lines) and auto-truncated
// (truncate.go's TruncateHead) when they exceed DefaultMaxLines/
// DefaultMaxBytes; images are detected by content (image.go's
// DetectSupportedImageMimeType, not by file extension) and either embedded
// as-is or run through an optional ReadImageProcessor hook.
//
// Deviation from upstream: as in write.go, every failure is returned as
// pi.ToolResult{IsError: true, Content: <message>} rather than a bubbled Go
// error, matching upstream's uniform thrown-error-to-error-result harness
// behavior regardless of how this tool is invoked.

// ReadToolOptions configures NewReadTool.
type ReadToolOptions struct {
	// Env is the filesystem seam the tool reads through.
	Env ExecutionEnv
	// AutoResizeImages controls whether an injected ImageProcessor should
	// resize images. nil selects true (upstream's `autoResizeImages ?? true`).
	// Only consulted when ImageProcessor is non-nil.
	AutoResizeImages *bool
	// ImageProcessor optionally resizes/re-encodes sniffed image bytes
	// before they're embedded in the result. Nil embeds sniffed bytes as-is,
	// except BMP - which without a processor can't be embedded and is
	// reported as omitted instead (see readImageToolResult).
	ImageProcessor ReadImageProcessor
}

// readParams are the model-supplied arguments to the "read" tool. Limit is
// *int, not int: upstream distinguishes an omitted limit (undefined, "read
// to EOF") from an explicit limit of 0 ("read zero lines" - selects an
// empty window, still reports a continuation notice) via `limit !==
// undefined`. A plain int can't represent that distinction (its zero value
// is indistinguishable from "omitted"), so nil means omitted and a non-nil
// pointer - including one pointing at 0 - means the model supplied a limit.
// Offset stays a plain int: upstream's own `offset ? ... : 0` treats
// undefined, 0, and 1 identically (all three collapse to startLine 0), so
// there is no equivalent distinction to preserve for Offset.
type readParams struct {
	Path   string `json:"path" jsonschema:"description=Path to the file to read (relative or absolute)"`
	Offset int    `json:"offset,omitempty" jsonschema:"description=Line number to start reading from (1-indexed)"`
	Limit  *int   `json:"limit,omitempty" jsonschema:"description=Maximum number of lines to read"`
}

// readToolDescription is the "read" tool's model-facing description, ported
// verbatim from upstream's createReadTool with DefaultMaxLines/
// DefaultMaxBytes interpolated. Upstream divides DEFAULT_MAX_BYTES by 1024
// directly here (plain integer division, "50KB") rather than using
// formatSize (which the truncation notices below use, and which renders
// "50.0KB") - the two are deliberately different strings.
var readToolDescription = fmt.Sprintf(
	"Read the contents of a file. Supports text files and images (jpg, png, gif, webp, bmp). Images are sent as attachments. For text files, output is truncated to %d lines or %dKB (whichever is hit first). Use offset/limit for large files. When you need the full file, continue with offset until complete.",
	DefaultMaxLines, DefaultMaxBytes/1024,
)

// readToolDetails is the JSON shape written to ToolResult.Data once the text
// path's truncation applies, mirroring upstream's ReadToolDetails interface
// (`{ truncation?: TruncationResult }`).
type readToolDetails struct {
	Truncation truncationDetail `json:"truncation"`
}

// truncationDetail mirrors truncate.ts's TruncationResult interface field-
// for-field with its camelCase JSON keys. truncate.go's TruncationResult
// (Task 5) carries no JSON tags of its own - nothing in that task's scope
// needed marshaling - so this tool-local type owns the wire shape here
// rather than adding tags to a struct used well beyond this file.
type truncationDetail struct {
	Content               string `json:"content"`
	Truncated             bool   `json:"truncated"`
	TruncatedBy           string `json:"truncatedBy"`
	TotalLines            int    `json:"totalLines"`
	TotalBytes            int    `json:"totalBytes"`
	OutputLines           int    `json:"outputLines"`
	OutputBytes           int    `json:"outputBytes"`
	LastLinePartial       bool   `json:"lastLinePartial"`
	FirstLineExceedsLimit bool   `json:"firstLineExceedsLimit"`
	MaxLines              int    `json:"maxLines"`
	MaxBytes              int    `json:"maxBytes"`
}

// marshalTruncationDetails wraps t as the {"truncation": ...} JSON payload
// for ToolResult.Data. The error from json.Marshal is ignored: truncationDetail
// is a plain struct of strings/bools/ints, which json.Marshal cannot fail on.
func marshalTruncationDetails(t TruncationResult) json.RawMessage {
	raw, _ := json.Marshal(readToolDetails{Truncation: truncationDetail{
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
	}})
	return raw
}

// NewReadTool creates the "read" tool: read a text or image file. Ported
// from upstream's createReadTool.
func NewReadTool(opts ReadToolOptions) pi.RegisteredTool {
	env := opts.Env
	autoResizeImages := true
	if opts.AutoResizeImages != nil {
		autoResizeImages = *opts.AutoResizeImages
	}
	imageProcessor := opts.ImageProcessor

	return pi.NewTool(pi.Tool[readParams]{
		Name:        "read",
		Description: readToolDescription,
		Execute: func(ctx context.Context, p readParams) (pi.ToolResult, error) {
			resolvedPath, err := ResolveReadToolPath(ctx, env, p.Path)
			if err != nil {
				return pi.ToolResult{IsError: true, Content: err.Error()}, nil
			}
			data, err := env.ReadFile(ctx, resolvedPath)
			if err != nil {
				return pi.ToolResult{IsError: true, Content: err.Error()}, nil
			}

			if mimeType := DetectSupportedImageMimeType(data); mimeType != "" {
				return readImageToolResult(ctx, data, mimeType, imageProcessor, autoResizeImages)
			}
			return readTextToolResult(p, data)
		},
	})
}

// readImageToolResult builds the read tool's result for a sniffed image
// file: with an ImageProcessor configured, delegates conversion/resizing to
// it; otherwise embeds the sniffed bytes as-is, except BMP - which without a
// processor can't be embedded and is reported as omitted. Ported from the
// image branch of upstream createReadTool's execute.
func readImageToolResult(ctx context.Context, data []byte, mimeType string, processor ReadImageProcessor, autoResizeImages bool) (pi.ToolResult, error) {
	if processor != nil {
		processed, err := processor(ctx, data, mimeType, autoResizeImages)
		if err != nil {
			return pi.ToolResult{IsError: true, Content: err.Error()}, nil
		}
		if !processed.OK {
			return pi.ToolResult{
				Content: fmt.Sprintf("Read image file [%s]\n%s", mimeType, processed.Message),
			}, nil
		}
		hints := ""
		if len(processed.Hints) > 0 {
			hints = "\n" + strings.Join(processed.Hints, "\n")
		}
		return pi.ToolResult{
			Content: fmt.Sprintf("Read image file [%s]%s", processed.MimeType, hints),
			Images:  []llm.ImageContent{{Data: processed.Data, MimeType: processed.MimeType}},
		}, nil
	}

	if mimeType == "image/bmp" {
		return pi.ToolResult{
			Content: "Read image file [image/bmp]\n[Image omitted: configure an imageProcessor to convert BMP images.]",
		}, nil
	}

	return pi.ToolResult{
		Content: fmt.Sprintf("Read image file [%s]", mimeType),
		Images:  []llm.ImageContent{{Data: data, MimeType: mimeType}},
	}, nil
}

// readTextToolResult builds the read tool's result for a text file: resolve
// the requested offset/limit window (1-indexed lines), auto-truncate it
// (TruncateHead) if it still exceeds DefaultMaxLines/DefaultMaxBytes, and
// assemble the matching notice. Ported from the text branch of upstream
// createReadTool's execute.
func readTextToolResult(p readParams, data []byte) (pi.ToolResult, error) {
	allLines := strings.Split(string(data), "\n")
	totalFileLines := len(allLines)

	startLine := 0
	if p.Offset != 0 {
		startLine = p.Offset - 1
		if startLine < 0 {
			startLine = 0
		}
	}
	startLineDisplay := startLine + 1

	if startLine >= len(allLines) {
		return pi.ToolResult{
			IsError: true,
			Content: fmt.Sprintf("Offset %d is beyond end of file (%d lines total)", p.Offset, len(allLines)),
		}, nil
	}

	haveUserLimit := p.Limit != nil
	var selectedContent string
	var userLimitedLines int
	if haveUserLimit {
		endLine := startLine + *p.Limit
		if endLine > len(allLines) {
			endLine = len(allLines)
		}
		// DELIBERATE deviation from upstream (NOT parity - no upstream test
		// pins this input): upstream's own arithmetic is degenerate for a
		// negative Limit. `Math.min(startLine + limit, allLines.length)`
		// with limit < 0 yields a negative endLine, and JS's
		// `allLines.slice(startLine, endLine)` reinterprets a negative end
		// as "count back from the array's end" - producing a nonsensical
		// negative nextOffset in the continuation notice. Traced literally
		// against read.ts (packages/agent/src/harness/tools/read.ts:100-136
		// @0.82.0): offset=1, limit=-5 on a 3-line file makes upstream emit
		// "Use offset=-4 to continue." - not usable, not intentional
		// behavior worth reproducing. Go's slice expression has no such
		// negative-index reinterpretation: it panics outright when
		// endLine < startLine. Rather than port upstream's accidental
		// arithmetic (and its unusable negative offset), this clamps to an
		// empty selection - a considered Go-side safety fix, not an attempt
		// to match upstream's output for this degenerate input.
		if endLine < startLine {
			endLine = startLine
		}
		selectedContent = strings.Join(allLines[startLine:endLine], "\n")
		userLimitedLines = endLine - startLine
	} else {
		selectedContent = strings.Join(allLines[startLine:], "\n")
	}

	truncation := TruncateHead(selectedContent, TruncationOptions{})

	var outputText string
	var details json.RawMessage
	switch {
	case truncation.FirstLineExceedsLimit:
		firstLineSize := FormatSize(len(allLines[startLine]))
		outputText = fmt.Sprintf(
			"[Line %d is %s, exceeds %s limit. Use bash: sed -n '%dp' %s | head -c %d]",
			startLineDisplay, firstLineSize, FormatSize(DefaultMaxBytes), startLineDisplay, p.Path, DefaultMaxBytes,
		)
		details = marshalTruncationDetails(truncation)

	case truncation.Truncated:
		endLineDisplay := startLineDisplay + truncation.OutputLines - 1
		nextOffset := endLineDisplay + 1
		outputText = truncation.Content
		if truncation.TruncatedBy == "lines" {
			outputText += fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Use offset=%d to continue.]", startLineDisplay, endLineDisplay, totalFileLines, nextOffset)
		} else {
			outputText += fmt.Sprintf("\n\n[Showing lines %d-%d of %d (%s limit). Use offset=%d to continue.]", startLineDisplay, endLineDisplay, totalFileLines, FormatSize(DefaultMaxBytes), nextOffset)
		}
		details = marshalTruncationDetails(truncation)

	case haveUserLimit && startLine+userLimitedLines < len(allLines):
		remaining := len(allLines) - (startLine + userLimitedLines)
		nextOffset := startLine + userLimitedLines + 1
		outputText = fmt.Sprintf("%s\n\n[%d more lines in file. Use offset=%d to continue.]", truncation.Content, remaining, nextOffset)

	default:
		outputText = truncation.Content
	}

	return pi.ToolResult{Content: outputText, Data: details}, nil
}
