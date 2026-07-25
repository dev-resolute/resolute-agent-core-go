// Package tools provides utilities consumed by agent-facing tool
// implementations.
package tools

import (
	"fmt"
	"strings"
)

// truncate.go ports packages/agent/src/harness/utils/truncate.ts from
// upstream pi @0.82.0: shared truncation utilities for tool outputs.
//
// Truncation is based on two independent limits - whichever is hit first wins:
//   - Line limit (default: DefaultMaxLines)
//   - Byte limit (default: DefaultMaxBytes)
//
// Never returns partial lines (except TruncateTail's LastLinePartial edge case).

// DefaultMaxLines is the line limit applied when TruncationOptions.MaxLines is zero.
const DefaultMaxLines = 2000

// DefaultMaxBytes is the byte limit (50KB) applied when TruncationOptions.MaxBytes is zero.
const DefaultMaxBytes = 50 * 1024

// TruncationResult describes the outcome of a TruncateHead or TruncateTail call.
type TruncationResult struct {
	// Content is the truncated content.
	Content string
	// Truncated reports whether truncation occurred.
	Truncated bool
	// TruncatedBy is which limit was hit: "lines", "bytes", or "" if Truncated is false.
	TruncatedBy string
	// TotalLines is the total number of lines in the original content.
	TotalLines int
	// TotalBytes is the total number of bytes in the original content.
	TotalBytes int
	// OutputLines is the number of complete lines in the truncated output.
	OutputLines int
	// OutputBytes is the number of bytes in the truncated output.
	OutputBytes int
	// LastLinePartial reports whether the last line was partially truncated.
	// Only ever set by TruncateTail's single-oversized-line edge case.
	LastLinePartial bool
	// FirstLineExceedsLimit reports whether the first line alone exceeded MaxBytes.
	// Only ever set by TruncateHead.
	FirstLineExceedsLimit bool
	// MaxLines is the max lines limit that was applied.
	MaxLines int
	// MaxBytes is the max bytes limit that was applied.
	MaxBytes int
}

// TruncationOptions configures TruncateHead and TruncateTail. A zero value
// for either field selects the corresponding Default* constant.
type TruncationOptions struct {
	// MaxLines is the maximum number of lines. Zero selects DefaultMaxLines.
	MaxLines int
	// MaxBytes is the maximum number of bytes. Zero selects DefaultMaxBytes.
	MaxBytes int
}

// resolveLimits applies TruncationOptions defaults, mirroring upstream's
// `options.maxLines ?? DEFAULT_MAX_LINES` / `options.maxBytes ?? DEFAULT_MAX_BYTES`.
func resolveLimits(opts TruncationOptions) (maxLines, maxBytes int) {
	maxLines = opts.MaxLines
	if maxLines == 0 {
		maxLines = DefaultMaxLines
	}
	maxBytes = opts.MaxBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxBytes
	}
	return maxLines, maxBytes
}

// splitLinesForCounting splits content into lines for counting purposes: a
// trailing newline does not produce an extra empty final line, and empty
// content has zero lines (nil, not an empty non-nil slice).
func splitLinesForCounting(content string) []string {
	if len(content) == 0 {
		return nil
	}
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// noTruncationResult builds the "no truncation needed" result shared by
// TruncateHead and TruncateTail.
func noTruncationResult(content string, totalLines, totalBytes, maxLines, maxBytes int) TruncationResult {
	return TruncationResult{
		Content:               content,
		Truncated:             false,
		TruncatedBy:           "",
		TotalLines:            totalLines,
		TotalBytes:            totalBytes,
		OutputLines:           totalLines,
		OutputBytes:           totalBytes,
		LastLinePartial:       false,
		FirstLineExceedsLimit: false,
		MaxLines:              maxLines,
		MaxBytes:              maxBytes,
	}
}

// TruncateHead truncates content from the head (keeps the first N
// lines/bytes). Suitable for file reads where you want to see the beginning.
//
// Never returns partial lines. If the first line exceeds MaxBytes, returns
// empty content with FirstLineExceedsLimit set.
func TruncateHead(content string, opts TruncationOptions) TruncationResult {
	maxLines, maxBytes := resolveLimits(opts)

	totalBytes := len(content)
	lines := splitLinesForCounting(content)
	totalLines := len(lines)

	// Check if no truncation needed.
	if totalLines <= maxLines && totalBytes <= maxBytes {
		return noTruncationResult(content, totalLines, totalBytes, maxLines, maxBytes)
	}

	// Check if first line alone exceeds byte limit.
	firstLineBytes := len(lines[0])
	if firstLineBytes > maxBytes {
		return TruncationResult{
			Content:               "",
			Truncated:             true,
			TruncatedBy:           "bytes",
			TotalLines:            totalLines,
			TotalBytes:            totalBytes,
			OutputLines:           0,
			OutputBytes:           0,
			LastLinePartial:       false,
			FirstLineExceedsLimit: true,
			MaxLines:              maxLines,
			MaxBytes:              maxBytes,
		}
	}

	// Collect complete lines that fit.
	var outputLinesArr []string
	outputBytesCount := 0
	truncatedBy := "lines"

	for i := 0; i < len(lines) && i < maxLines; i++ {
		line := lines[i]
		lineBytes := len(line)
		if i > 0 {
			lineBytes++ // +1 for newline
		}

		if outputBytesCount+lineBytes > maxBytes {
			truncatedBy = "bytes"
			break
		}

		outputLinesArr = append(outputLinesArr, line)
		outputBytesCount += lineBytes
	}

	// If we exited due to line limit.
	if len(outputLinesArr) >= maxLines && outputBytesCount <= maxBytes {
		truncatedBy = "lines"
	}

	outputContent := strings.Join(outputLinesArr, "\n")

	return TruncationResult{
		Content:               outputContent,
		Truncated:             true,
		TruncatedBy:           truncatedBy,
		TotalLines:            totalLines,
		TotalBytes:            totalBytes,
		OutputLines:           len(outputLinesArr),
		OutputBytes:           len(outputContent),
		LastLinePartial:       false,
		FirstLineExceedsLimit: false,
		MaxLines:              maxLines,
		MaxBytes:              maxBytes,
	}
}

// TruncateTail truncates content from the tail (keeps the last N
// lines/bytes). Suitable for bash output where you want to see the end
// (errors, final results).
//
// May return a partial last line if the last line of the original content
// exceeds the byte limit.
func TruncateTail(content string, opts TruncationOptions) TruncationResult {
	maxLines, maxBytes := resolveLimits(opts)

	totalBytes := len(content)
	lines := splitLinesForCounting(content)
	totalLines := len(lines)

	// Check if no truncation needed.
	if totalLines <= maxLines && totalBytes <= maxBytes {
		return noTruncationResult(content, totalLines, totalBytes, maxLines, maxBytes)
	}

	// Work backwards from the end, collecting lines in reverse (last-to-first) order.
	var reversed []string
	outputBytesCount := 0
	truncatedBy := "lines"
	lastLinePartial := false

	for i := len(lines) - 1; i >= 0 && len(reversed) < maxLines; i-- {
		line := lines[i]
		lineBytes := len(line)
		if len(reversed) > 0 {
			lineBytes++ // +1 for newline
		}

		if outputBytesCount+lineBytes > maxBytes {
			truncatedBy = "bytes"
			// Edge case: if we haven't added ANY lines yet and this line
			// exceeds maxBytes, take the end of the line (partial).
			if len(reversed) == 0 {
				truncatedLine := truncateStringToBytesFromEnd(line, maxBytes)
				reversed = append(reversed, truncatedLine)
				outputBytesCount = len(truncatedLine)
				lastLinePartial = true
			}
			break
		}

		reversed = append(reversed, line)
		outputBytesCount += lineBytes
	}

	// If we exited due to line limit.
	if len(reversed) >= maxLines && outputBytesCount <= maxBytes {
		truncatedBy = "lines"
	}

	outputLinesArr := make([]string, len(reversed))
	for i, line := range reversed {
		outputLinesArr[len(reversed)-1-i] = line
	}
	outputContent := strings.Join(outputLinesArr, "\n")

	return TruncationResult{
		Content:               outputContent,
		Truncated:             true,
		TruncatedBy:           truncatedBy,
		TotalLines:            totalLines,
		TotalBytes:            totalBytes,
		OutputLines:           len(outputLinesArr),
		OutputBytes:           len(outputContent),
		LastLinePartial:       lastLinePartial,
		FirstLineExceedsLimit: false,
		MaxLines:              maxLines,
		MaxBytes:              maxBytes,
	}
}

// truncateStringToBytesFromEnd returns the longest suffix of s that fits
// within maxBytes without splitting a multi-byte UTF-8 sequence.
//
// Deviation from upstream: the TypeScript source additionally calls
// replaceUnpairedSurrogates after a UTF-16-code-unit-based walk, because JS
// strings are UTF-16 and a naive cut can leave a lone surrogate that must be
// replaced with U+FFFD when encoded to UTF-8. Go strings are already raw
// bytes - there is no UTF-16 surrogate layer to repair - so skipping forward
// past any continuation-byte prefix left by the raw cut is sufficient to
// land on a clean UTF-8 character boundary.
func truncateStringToBytesFromEnd(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && s[start]&0xc0 == 0x80 {
		start++
	}
	return s[start:]
}

// FormatSize formats bytes as a human-readable size, e.g. "512B", "50.0KB", "1.2MB".
func FormatSize(bytes int) string {
	if bytes < 1024 {
		return fmt.Sprintf("%dB", bytes)
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(bytes)/(1024*1024))
}
