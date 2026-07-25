package tools

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/dev-resolute/resolute-llm-go"
)

// read_test.go exercises read.go, the port of
// packages/agent/src/harness/tools/read.ts @0.82.0. Cases ported from
// packages/agent/test/harness/tools.test.ts's "read" describe block are
// noted per-case below; the byte-truncation and oversized-first-line cases
// have no upstream test counterpart (not exercised there) and are
// hand-derived fixtures sized to trigger those exact branches.

// pngFixture is a real, minimal 1x1 PNG - the exact bytes upstream's
// "detects supported images by content" test decodes from its base64
// literal (packages/agent/test/harness/tools.test.ts), reproduced here as a
// literal so DetectSupportedImageMimeType/isPng exercise a real file rather
// than a hand-rolled approximation.
var pngFixture = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x60, 0x60, 0x60, 0xf8,
	0x0f, 0x00, 0x01, 0x04, 0x01, 0x00, 0x5f, 0xe5, 0xc3, 0x4b, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45,
	0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

// newTinyBMP builds a minimal well-formed BMP (58 bytes: 14-byte file
// header + 40-byte BITMAPINFOHEADER + 4 bytes of pixel data), matching
// upstream's createTinyBmp fixture field-for-field.
func newTinyBMP() []byte {
	const size = 58
	data := make([]byte, size)
	data[0], data[1] = 'B', 'M'
	binary.LittleEndian.PutUint32(data[2:], uint32(size)) // declared file size
	binary.LittleEndian.PutUint32(data[10:], 54)          // pixel data offset (14 + 40)
	binary.LittleEndian.PutUint32(data[14:], 40)          // DIB header size (BITMAPINFOHEADER)
	binary.LittleEndian.PutUint32(data[18:], 1)           // width
	binary.LittleEndian.PutUint32(data[22:], 1)           // height
	binary.LittleEndian.PutUint16(data[26:], 1)           // color planes
	binary.LittleEndian.PutUint16(data[28:], 24)          // bits per pixel
	binary.LittleEndian.PutUint32(data[34:], 4)           // image data size
	return data
}

// mustMarshalReadParams builds readParams JSON for tests that don't care
// about the omitted-vs-explicit-zero limit distinction: limit == 0 means
// "omit the field entirely" (readParams.Limit stays nil), matching every
// existing call site's intent. Tests that specifically need an explicit
// zero limit (readParams.Limit pointing at 0) construct readParams
// directly instead - see TestReadToolExplicitZeroLimitIsNotOmitted.
func mustMarshalReadParams(t *testing.T, path string, offset, limit int) json.RawMessage {
	t.Helper()
	var limitPtr *int
	if limit != 0 {
		limitPtr = &limit
	}
	raw, err := json.Marshal(readParams{Path: path, Offset: offset, Limit: limitPtr})
	if err != nil {
		t.Fatalf("json.Marshal(readParams) error: %v", err)
	}
	return raw
}

// wantTruncationDetails is a partial decode of ToolResult.Data, mirroring
// the fields upstream's tests assert on `result.details?.truncation` via
// toMatchObject (a subset check, not full equality).
type wantTruncationDetails struct {
	Truncation struct {
		Truncated             bool   `json:"truncated"`
		TruncatedBy           string `json:"truncatedBy"`
		TotalLines            int    `json:"totalLines"`
		OutputLines           int    `json:"outputLines"`
		FirstLineExceedsLimit bool   `json:"firstLineExceedsLimit"`
	} `json:"truncation"`
}

func decodeTruncationDetails(t *testing.T, data json.RawMessage) wantTruncationDetails {
	t.Helper()
	var got wantTruncationDetails
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal(result.Data) error: %v, data: %s", err, data)
	}
	return got
}

func TestNewReadToolNameAndDescription(t *testing.T) {
	env, _ := newTestOSEnv(t)
	tool := NewReadTool(ReadToolOptions{Env: env})

	if got, want := tool.Name(), "read"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}

	// Verbatim upstream read.ts description with DEFAULT_MAX_LINES=2000 and
	// DEFAULT_MAX_BYTES/1024=50 (plain division - "50KB", NOT formatSize's
	// "50.0KB") interpolated.
	want := "Read the contents of a file. Supports text files and images (jpg, png, gif, webp, bmp). Images are sent as attachments. For text files, output is truncated to 2000 lines or 50KB (whichever is hit first). Use offset/limit for large files. When you need the full file, continue with offset until complete."
	if got := tool.Description(); got != want {
		t.Errorf("Description() = %q, want %q", got, want)
	}
}

func TestReadToolSchema(t *testing.T) {
	env, _ := newTestOSEnv(t)
	tool := NewReadTool(ReadToolOptions{Env: env})

	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatalf("json.Unmarshal(schema) error: %v", err)
	}

	tests := []struct {
		prop string
		want string
	}{
		{"path", "Path to the file to read (relative or absolute)"},
		{"offset", "Line number to start reading from (1-indexed)"},
		{"limit", "Maximum number of lines to read"},
	}
	for _, tc := range tests {
		got, ok := schema.Properties[tc.prop]
		if !ok {
			t.Errorf("schema properties missing %q", tc.prop)
			continue
		}
		if got.Description != tc.want {
			t.Errorf("schema %q description = %q, want %q", tc.prop, got.Description, tc.want)
		}
	}
}

func TestReadToolReadsFullSmallFileVerbatim(t *testing.T) {
	env, dir := newTestOSEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "small.txt"), []byte("hello\nworld"), 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	tool := NewReadTool(ReadToolOptions{Env: env})

	result, err := tool.Execute(context.Background(), "read-1", mustMarshalReadParams(t, "small.txt", 0, 0))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, Content: %q", result.Content)
	}
	if want := "hello\nworld"; result.Content != want {
		t.Errorf("result.Content = %q, want %q", result.Content, want)
	}
	if len(result.Data) != 0 {
		t.Errorf("result.Data = %s, want empty (no truncation)", result.Data)
	}
}

// TestReadToolOffsetLimitContinuationNotice ports upstream's "reads text
// with offsets, limits, and continuation notices".
func TestReadToolOffsetLimitContinuationNotice(t *testing.T) {
	env, dir := newTestOSEnv(t)
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = "Line " + strconv.Itoa(i+1)
	}
	if err := os.WriteFile(filepath.Join(dir, "test.txt"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	tool := NewReadTool(ReadToolOptions{Env: env})

	result, err := tool.Execute(context.Background(), "read-1", mustMarshalReadParams(t, "test.txt", 41, 20))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, Content: %q", result.Content)
	}

	if strings.Contains(result.Content, "Line 40") {
		t.Error("result.Content contains \"Line 40\", want it excluded (offset=41 starts at Line 41)")
	}
	if !strings.Contains(result.Content, "Line 41") {
		t.Error("result.Content missing \"Line 41\"")
	}
	if !strings.Contains(result.Content, "Line 60") {
		t.Error("result.Content missing \"Line 60\" (limit=20 from offset=41 ends at Line 60)")
	}
	if strings.Contains(result.Content, "Line 61") {
		t.Error("result.Content contains \"Line 61\", want it excluded")
	}
	if want := "\n\n[40 more lines in file. Use offset=61 to continue.]"; !strings.HasSuffix(result.Content, want) {
		t.Errorf("result.Content does not end with %q; got %q", want, result.Content)
	}
}

// TestReadToolTruncatesByLineCount ports upstream's "truncates large text
// by line count".
func TestReadToolTruncatesByLineCount(t *testing.T) {
	env, dir := newTestOSEnv(t)
	lines := make([]string, 2500)
	for i := range lines {
		lines[i] = "Line " + strconv.Itoa(i+1)
	}
	if err := os.WriteFile(filepath.Join(dir, "large.txt"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	tool := NewReadTool(ReadToolOptions{Env: env})

	result, err := tool.Execute(context.Background(), "read-2", mustMarshalReadParams(t, "large.txt", 0, 0))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, Content: %q", result.Content)
	}

	want := "\n\n[Showing lines 1-2000 of 2500. Use offset=2001 to continue.]"
	if !strings.HasSuffix(result.Content, want) {
		t.Errorf("result.Content does not end with %q; got last 100 chars: %q", want, lastN(result.Content, 100))
	}

	got := decodeTruncationDetails(t, result.Data)
	if !got.Truncation.Truncated || got.Truncation.TruncatedBy != "lines" || got.Truncation.TotalLines != 2500 || got.Truncation.OutputLines != 2000 {
		t.Errorf("result.Data truncation = %+v, want {truncated:true truncatedBy:lines totalLines:2500 outputLines:2000}", got.Truncation)
	}
}

// TestReadToolTrailingNewlineNotCountedAsExtraLineAtLimit ports upstream's
// "does not count a trailing newline as an extra line at the truncation
// limit".
func TestReadToolTrailingNewlineNotCountedAsExtraLineAtLimit(t *testing.T) {
	env, dir := newTestOSEnv(t)
	lines := make([]string, 2000)
	for i := range lines {
		lines[i] = "x"
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "exact.txt"), []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	tool := NewReadTool(ReadToolOptions{Env: env})

	result, err := tool.Execute(context.Background(), "read-exact", mustMarshalReadParams(t, "exact.txt", 0, 0))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, Content: %q", result.Content)
	}
	if len(result.Data) != 0 {
		t.Errorf("result.Data = %s, want empty (no truncation)", result.Data)
	}
	if strings.Contains(result.Content, "Use offset=") {
		t.Errorf("result.Content unexpectedly contains a continuation notice: %q", result.Content)
	}
}

// TestReadToolRejectsOffsetBeyondEOF ports upstream's "rejects offsets
// beyond the file".
func TestReadToolRejectsOffsetBeyondEOF(t *testing.T) {
	env, dir := newTestOSEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "short.txt"), []byte("one\ntwo\nthree"), 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	tool := NewReadTool(ReadToolOptions{Env: env})

	result, err := tool.Execute(context.Background(), "read-3", mustMarshalReadParams(t, "short.txt", 100, 0))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("result.IsError = false, want true")
	}
	if want := "Offset 100 is beyond end of file (3 lines total)"; result.Content != want {
		t.Errorf("result.Content = %q, want %q", result.Content, want)
	}
}

// TestReadToolNegativeLimitClampsSafely pins a Go deviation: negative limit
// clamps safely - this is NOT an upstream-parity assertion, and no upstream
// test pins this degenerate input.
//
// Traced literally, upstream's own arithmetic for this exact input (offset=1,
// limit=-5, 3-line file) is degenerate: `Math.min(startLine + limit,
// allLines.length)` = `Math.min(-5, 3)` = -5, and JS's
// `allLines.slice(0, -5)` reinterprets that negative end as "count back
// from the array's end" (clamped to 0), silently yielding an empty
// selection - but the SAME raw (unclamped) arithmetic then also drives the
// continuation notice's numbers, producing
// "[8 more lines in file. Use offset=-4 to continue.]": a nonsensical
// negative offset a caller could never use. That is upstream's actual,
// verified behavior for this input - not something this port emulates.
//
// This port deliberately does NOT reproduce that arithmetic. Go's slice
// expression has no negative-index reinterpretation (it panics outright on
// endLine < startLine), so read.go clamps endLine to startLine instead -
// both because leaving it unclamped would crash the tool, and because
// clamping avoids propagating upstream's nonsensical negative nextOffset
// into the notice. The assertion below is this port's own considered
// output for this input, not a port of upstream's.
func TestReadToolNegativeLimitClampsSafely(t *testing.T) {
	env, dir := newTestOSEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "short.txt"), []byte("one\ntwo\nthree"), 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	tool := NewReadTool(ReadToolOptions{Env: env})

	result, err := tool.Execute(context.Background(), "read-neg-limit", mustMarshalReadParams(t, "short.txt", 1, -5))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, Content: %q", result.Content)
	}
	if want := "\n\n[3 more lines in file. Use offset=1 to continue.]"; result.Content != want {
		t.Errorf("result.Content = %q, want %q", result.Content, want)
	}
}

// TestReadToolExplicitZeroLimitIsNotOmitted pins the readParams.Limit
// pointer-vs-int distinction: upstream checks `limit !== undefined`, so an
// explicit limit of 0 selects a zero-line window (not "no limit"/read to
// EOF). Derived from upstream's arithmetic for offset omitted (startLine=0)
// and limit=0: endLine = Math.min(0+0, len) = 0, selectedContent = "",
// userLimitedLines = 0; then remaining = totalLines - 0, nextOffset =
// 0 + 0 + 1 = 1.
func TestReadToolExplicitZeroLimitIsNotOmitted(t *testing.T) {
	env, dir := newTestOSEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "five.txt"), []byte("one\ntwo\nthree\nfour\nfive"), 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	tool := NewReadTool(ReadToolOptions{Env: env})

	zero := 0
	args, err := json.Marshal(readParams{Path: "five.txt", Limit: &zero})
	if err != nil {
		t.Fatalf("json.Marshal(readParams) error: %v", err)
	}

	result, err := tool.Execute(context.Background(), "read-zero-limit", args)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, Content: %q", result.Content)
	}
	if want := "\n\n[5 more lines in file. Use offset=1 to continue.]"; result.Content != want {
		t.Errorf("result.Content = %q, want %q (explicit limit:0 must NOT be treated as omitted)", result.Content, want)
	}
}

// TestReadToolTruncatesByByteCount is a hand-derived fixture (no upstream
// test counterpart): 600 lines of exactly 100 'a' bytes each (60,599 bytes
// total, well under DefaultMaxLines=2000 but over DefaultMaxBytes=51200),
// sized so TruncateHead hits the BYTE limit, not the line limit. Hand-
// derivation (see task report): the 507th line would push cumulative output
// bytes to 51,206 > 51,200, so exactly 506 lines are kept.
func TestReadToolTruncatesByByteCount(t *testing.T) {
	env, dir := newTestOSEnv(t)
	lines := make([]string, 600)
	for i := range lines {
		lines[i] = strings.Repeat("a", 100)
	}
	if err := os.WriteFile(filepath.Join(dir, "wide.txt"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	tool := NewReadTool(ReadToolOptions{Env: env})

	result, err := tool.Execute(context.Background(), "read-bytes", mustMarshalReadParams(t, "wide.txt", 0, 0))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, Content: %q", result.Content)
	}

	want := "\n\n[Showing lines 1-506 of 600 (50.0KB limit). Use offset=507 to continue.]"
	if !strings.HasSuffix(result.Content, want) {
		t.Errorf("result.Content does not end with %q; got last 120 chars: %q", want, lastN(result.Content, 120))
	}

	got := decodeTruncationDetails(t, result.Data)
	if !got.Truncation.Truncated || got.Truncation.TruncatedBy != "bytes" || got.Truncation.TotalLines != 600 || got.Truncation.OutputLines != 506 {
		t.Errorf("result.Data truncation = %+v, want {truncated:true truncatedBy:bytes totalLines:600 outputLines:506}", got.Truncation)
	}
}

// TestReadToolOversizedFirstLine is a hand-derived fixture (no upstream
// test counterpart): a single 60,000-byte line (no newline at all), which
// alone exceeds DefaultMaxBytes=51200. FormatSize(60000) = "58.6KB"
// (60000/1024 = 58.59375, rounds to one decimal).
func TestReadToolOversizedFirstLine(t *testing.T) {
	env, dir := newTestOSEnv(t)
	huge := strings.Repeat("a", 60000)
	if err := os.WriteFile(filepath.Join(dir, "huge.txt"), []byte(huge), 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	tool := NewReadTool(ReadToolOptions{Env: env})

	result, err := tool.Execute(context.Background(), "read-huge", mustMarshalReadParams(t, "huge.txt", 0, 0))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, want a non-error result carrying the sed hint; Content: %q", result.Content)
	}

	want := "[Line 1 is 58.6KB, exceeds 50.0KB limit. Use bash: sed -n '1p' huge.txt | head -c 51200]"
	if result.Content != want {
		t.Errorf("result.Content = %q, want %q", result.Content, want)
	}

	got := decodeTruncationDetails(t, result.Data)
	if !got.Truncation.FirstLineExceedsLimit || !got.Truncation.Truncated || got.Truncation.TotalLines != 1 {
		t.Errorf("result.Data truncation = %+v, want {truncated:true firstLineExceedsLimit:true totalLines:1}", got.Truncation)
	}
}

// TestReadToolDetectsPNGImage ports upstream's "detects supported images by
// content" - the file is named .txt to prove detection is content-based
// (magic-byte sniffing), not extension-based.
func TestReadToolDetectsPNGImage(t *testing.T) {
	env, dir := newTestOSEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "image.txt"), pngFixture, 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	tool := NewReadTool(ReadToolOptions{Env: env})

	result, err := tool.Execute(context.Background(), "read-4", mustMarshalReadParams(t, "image.txt", 0, 0))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, Content: %q", result.Content)
	}
	if want := "Read image file [image/png]"; result.Content != want {
		t.Errorf("result.Content = %q, want %q", result.Content, want)
	}
	wantImages := []llm.ImageContent{{Data: pngFixture, MimeType: "image/png"}}
	if !reflect.DeepEqual(result.Images, wantImages) {
		t.Errorf("result.Images = %+v, want %+v", result.Images, wantImages)
	}
}

// TestReadToolBMPWithoutProcessor pins the brief's exact omitted-image
// message for a supported-but-not-directly-embeddable format (BMP) when no
// ImageProcessor is configured.
func TestReadToolBMPWithoutProcessor(t *testing.T) {
	env, dir := newTestOSEnv(t)
	bmp := newTinyBMP()
	if err := os.WriteFile(filepath.Join(dir, "image.bmp"), bmp, 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	tool := NewReadTool(ReadToolOptions{Env: env})

	result, err := tool.Execute(context.Background(), "read-bmp-noproc", mustMarshalReadParams(t, "image.bmp", 0, 0))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, Content: %q", result.Content)
	}
	want := "Read image file [image/bmp]\n[Image omitted: configure an imageProcessor to convert BMP images.]"
	if result.Content != want {
		t.Errorf("result.Content = %q, want %q", result.Content, want)
	}
	if len(result.Images) != 0 {
		t.Errorf("result.Images = %+v, want none", result.Images)
	}
}

// TestReadToolImageProcessorHintsAndData ports upstream's "delegates image
// conversion and resizing to an injected processor".
func TestReadToolImageProcessorHintsAndData(t *testing.T) {
	env, dir := newTestOSEnv(t)
	bmp := newTinyBMP()
	if err := os.WriteFile(filepath.Join(dir, "image.bmp"), bmp, 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}

	var receivedData []byte
	var receivedMime string
	var receivedAutoResize bool
	autoResizeImages := false
	tool := NewReadTool(ReadToolOptions{
		Env:              env,
		AutoResizeImages: &autoResizeImages,
		ImageProcessor: func(_ context.Context, data []byte, mimeType string, autoResize bool) (ReadImageProcessorResult, error) {
			receivedData = data
			receivedMime = mimeType
			receivedAutoResize = autoResize
			return ReadImageProcessorResult{
				OK:       true,
				Data:     []byte("converted"),
				MimeType: "image/png",
				Hints:    []string{"[Image converted from image/bmp to image/png.]"},
			}, nil
		},
	})

	result, err := tool.Execute(context.Background(), "read-bmp", mustMarshalReadParams(t, "image.bmp", 0, 0))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, Content: %q", result.Content)
	}

	if receivedMime != "image/bmp" {
		t.Errorf("processor received mimeType = %q, want %q", receivedMime, "image/bmp")
	}
	if receivedAutoResize {
		t.Error("processor received autoResize = true, want false (explicitly configured)")
	}
	if !bytes.Equal(receivedData, bmp) {
		t.Error("processor did not receive the raw sniffed bytes")
	}

	want := "Read image file [image/png]\n[Image converted from image/bmp to image/png.]"
	if result.Content != want {
		t.Errorf("result.Content = %q, want %q", result.Content, want)
	}
	wantImages := []llm.ImageContent{{Data: []byte("converted"), MimeType: "image/png"}}
	if !reflect.DeepEqual(result.Images, wantImages) {
		t.Errorf("result.Images = %+v, want %+v", result.Images, wantImages)
	}
}

// TestReadToolImageProcessorDefaultAutoResizeTrue pins
// ReadToolOptions.AutoResizeImages's documented default: nil selects true.
func TestReadToolImageProcessorDefaultAutoResizeTrue(t *testing.T) {
	env, dir := newTestOSEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "image.png"), pngFixture, 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}

	var receivedAutoResize bool
	tool := NewReadTool(ReadToolOptions{
		Env: env,
		ImageProcessor: func(_ context.Context, data []byte, mimeType string, autoResize bool) (ReadImageProcessorResult, error) {
			receivedAutoResize = autoResize
			return ReadImageProcessorResult{OK: true, Data: data, MimeType: mimeType}, nil
		},
	})

	if _, err := tool.Execute(context.Background(), "read-default", mustMarshalReadParams(t, "image.png", 0, 0)); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !receivedAutoResize {
		t.Error("processor received autoResize = false, want true (default when AutoResizeImages is nil)")
	}
}

// TestReadToolImageProcessorNotOK pins the processor-failure branch: the
// header line still reports the SNIFFED mime type (not any type from the
// processor, since none is available on failure), followed by the
// processor's message, with no image content.
func TestReadToolImageProcessorNotOK(t *testing.T) {
	env, dir := newTestOSEnv(t)
	bmp := newTinyBMP()
	if err := os.WriteFile(filepath.Join(dir, "image.bmp"), bmp, 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	tool := NewReadTool(ReadToolOptions{
		Env: env,
		ImageProcessor: func(context.Context, []byte, string, bool) (ReadImageProcessorResult, error) {
			return ReadImageProcessorResult{OK: false, Message: "cannot convert this BMP variant"}, nil
		},
	})

	result, err := tool.Execute(context.Background(), "read-bmp-fail", mustMarshalReadParams(t, "image.bmp", 0, 0))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if result.IsError {
		t.Fatalf("result.IsError = true, want a non-error result carrying the processor's message; Content: %q", result.Content)
	}
	want := "Read image file [image/bmp]\ncannot convert this BMP variant"
	if result.Content != want {
		t.Errorf("result.Content = %q, want %q", result.Content, want)
	}
	if len(result.Images) != 0 {
		t.Errorf("result.Images = %+v, want none", result.Images)
	}
}

// TestReadToolImageProcessorErrorIsAnErrorResult pins the "errors are error
// RESULTS" contract (see write_test.go's analogous case) for the
// ImageProcessor's own error return, which upstream's ReadImageProcessor
// contract does not model (it never returns rejected) but this port's
// typed signature does (see image.go's ReadImageProcessor doc comment).
func TestReadToolImageProcessorErrorIsAnErrorResult(t *testing.T) {
	env, dir := newTestOSEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "image.png"), pngFixture, 0o644); err != nil {
		t.Fatalf("os.WriteFile error: %v", err)
	}
	wantErr := errors.New("processor boom")
	tool := NewReadTool(ReadToolOptions{
		Env: env,
		ImageProcessor: func(context.Context, []byte, string, bool) (ReadImageProcessorResult, error) {
			return ReadImageProcessorResult{}, wantErr
		},
	})

	result, err := tool.Execute(context.Background(), "read-proc-err", mustMarshalReadParams(t, "image.png", 0, 0))
	if err != nil {
		t.Fatalf("Execute error = %v, want nil (failure must surface as ToolResult.IsError)", err)
	}
	if !result.IsError {
		t.Fatalf("result.IsError = false, want true")
	}
	if result.Content != wantErr.Error() {
		t.Errorf("result.Content = %q, want %q", result.Content, wantErr.Error())
	}
}

// TestReadToolReadFailureIsAnErrorResult pins the "errors are error
// RESULTS, not bubbled Go errors" contract for a failing env.ReadFile.
func TestReadToolReadFailureIsAnErrorResult(t *testing.T) {
	env, _ := newTestOSEnv(t)
	wantErr := errors.New("boom: permission denied")
	failing := &erroringReadEnv{OSEnv: env, readErr: wantErr}
	tool := NewReadTool(ReadToolOptions{Env: failing})

	result, err := tool.Execute(context.Background(), "read-fail", mustMarshalReadParams(t, "f.txt", 0, 0))
	if err != nil {
		t.Fatalf("Execute error = %v, want nil (failure must surface as ToolResult.IsError)", err)
	}
	if !result.IsError {
		t.Fatalf("result.IsError = false, want true")
	}
	if result.Content != wantErr.Error() {
		t.Errorf("result.Content = %q, want %q", result.Content, wantErr.Error())
	}
}

// erroringReadEnv wraps *OSEnv, always failing ReadFile with readErr - the
// double used by TestReadToolReadFailureIsAnErrorResult.
type erroringReadEnv struct {
	*OSEnv
	readErr error
}

func (e *erroringReadEnv) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return nil, e.readErr
}

// lastN returns the last n bytes of s (or all of s if shorter), for
// compact failure messages against very large generated strings.
func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
