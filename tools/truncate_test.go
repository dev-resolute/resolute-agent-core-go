package tools

import (
	"sort"
	"strings"
	"testing"
)

// This file ports packages/agent/test/harness/truncate.test.ts (upstream
// pi @0.82.0) case-by-case. Each ported test carries the upstream `it(...)`
// description in its name for traceability. Two cases could not be ported
// verbatim because they exercise JS's UTF-16-surrogate-repair behavior,
// which has no Go equivalent (Go strings are UTF-8 byte sequences, not
// UTF-16 code units); see TestTruncateTailMatchesByteBoundarySemantics for
// the adapted replacement and a full explanation. Two additional cases
// ("authored") were added per the task brief's explicit minimum-pinned-case
// list to cover pure line-limit truncation, which the upstream suite does
// not exercise directly.

func TestTruncateLineCounting(t *testing.T) {
	cases := []struct {
		name           string
		content        string
		wantTotalLines int
	}{
		{"empty is zero lines", "", 0},
		{"no trailing newline", "a\nb", 2},
		{"trailing newline not an extra line", "a\nb\n", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TruncateHead(tc.content, TruncationOptions{}); got.TotalLines != tc.wantTotalLines {
				t.Errorf("TruncateHead(%q).TotalLines = %d, want %d", tc.content, got.TotalLines, tc.wantTotalLines)
			}
			if got := TruncateTail(tc.content, TruncationOptions{}); got.TotalLines != tc.wantTotalLines {
				t.Errorf("TruncateTail(%q).TotalLines = %d, want %d", tc.content, got.TotalLines, tc.wantTotalLines)
			}
		})
	}
}

func TestTruncationOptionsDefaults(t *testing.T) {
	head := TruncateHead("a", TruncationOptions{})
	if head.MaxLines != DefaultMaxLines {
		t.Errorf("TruncateHead zero-value MaxLines = %d, want DefaultMaxLines (%d)", head.MaxLines, DefaultMaxLines)
	}
	if head.MaxBytes != DefaultMaxBytes {
		t.Errorf("TruncateHead zero-value MaxBytes = %d, want DefaultMaxBytes (%d)", head.MaxBytes, DefaultMaxBytes)
	}

	tail := TruncateTail("a", TruncationOptions{})
	if tail.MaxLines != DefaultMaxLines {
		t.Errorf("TruncateTail zero-value MaxLines = %d, want DefaultMaxLines (%d)", tail.MaxLines, DefaultMaxLines)
	}
	if tail.MaxBytes != DefaultMaxBytes {
		t.Errorf("TruncateTail zero-value MaxBytes = %d, want DefaultMaxBytes (%d)", tail.MaxBytes, DefaultMaxBytes)
	}
}

func TestTruncateHead(t *testing.T) {
	tests := []struct {
		name    string
		content string
		opts    TruncationOptions
		check   func(t *testing.T, got TruncationResult)
	}{
		{
			// upstream: "counts UTF-8 bytes without Node Buffer"
			name:    "counts UTF-8 bytes without Node Buffer",
			content: "aé🙂\nb",
			opts:    TruncationOptions{MaxBytes: 100, MaxLines: 10},
			check: func(t *testing.T, got TruncationResult) {
				if got.Truncated {
					t.Errorf("Truncated = true, want false")
				}
				if got.TotalBytes != len("aé🙂\nb") {
					t.Errorf("TotalBytes = %d, want %d", got.TotalBytes, len("aé🙂\nb"))
				}
				if got.OutputBytes != got.TotalBytes {
					t.Errorf("OutputBytes = %d, want %d (== TotalBytes)", got.OutputBytes, got.TotalBytes)
				}
				if got.TotalBytes != 9 {
					t.Errorf("TotalBytes = %d, want 9", got.TotalBytes)
				}
			},
		},
		{
			// upstream: "truncates head on UTF-8 byte limits without partial lines"
			name:    "truncates head on UTF-8 byte limits without partial lines",
			content: "éé\nabc",
			opts:    TruncationOptions{MaxBytes: 4, MaxLines: 10},
			check: func(t *testing.T, got TruncationResult) {
				if got.Content != "éé" {
					t.Errorf("Content = %q, want %q", got.Content, "éé")
				}
				if !got.Truncated {
					t.Errorf("Truncated = false, want true")
				}
				if got.TruncatedBy != "bytes" {
					t.Errorf("TruncatedBy = %q, want %q", got.TruncatedBy, "bytes")
				}
				if got.OutputBytes != 4 {
					t.Errorf("OutputBytes = %d, want 4", got.OutputBytes)
				}
				if got.FirstLineExceedsLimit {
					t.Errorf("FirstLineExceedsLimit = true, want false")
				}
			},
		},
		{
			// upstream: "reports head truncation when the first line exceeds the byte limit"
			name:    "reports head truncation when the first line exceeds the byte limit",
			content: "éé\nabc",
			opts:    TruncationOptions{MaxBytes: 3, MaxLines: 10},
			check: func(t *testing.T, got TruncationResult) {
				if got.Content != "" {
					t.Errorf("Content = %q, want empty", got.Content)
				}
				if !got.Truncated {
					t.Errorf("Truncated = false, want true")
				}
				if got.TruncatedBy != "bytes" {
					t.Errorf("TruncatedBy = %q, want %q", got.TruncatedBy, "bytes")
				}
				if !got.FirstLineExceedsLimit {
					t.Errorf("FirstLineExceedsLimit = false, want true")
				}
			},
		},
		{
			// authored: not in upstream truncate.test.ts. Added per the task
			// brief's minimum-pinned-case list ("head truncation by line
			// limit keeps FIRST maxLines lines") — the upstream suite never
			// exercises pure line-limit truncation in isolation.
			name:    "authored: head truncation by line limit keeps FIRST maxLines lines",
			content: "line1\nline2\nline3\nline4\nline5",
			opts:    TruncationOptions{MaxLines: 2, MaxBytes: 1000},
			check: func(t *testing.T, got TruncationResult) {
				want := "line1\nline2"
				if got.Content != want {
					t.Errorf("Content = %q, want %q", got.Content, want)
				}
				if !got.Truncated {
					t.Errorf("Truncated = false, want true")
				}
				if got.TruncatedBy != "lines" {
					t.Errorf("TruncatedBy = %q, want %q", got.TruncatedBy, "lines")
				}
				if got.OutputLines != 2 {
					t.Errorf("OutputLines = %d, want 2", got.OutputLines)
				}
				if got.TotalLines != 5 {
					t.Errorf("TotalLines = %d, want 5", got.TotalLines)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TruncateHead(tc.content, tc.opts)
			tc.check(t, got)
		})
	}
}

func TestTruncateTail(t *testing.T) {
	tests := []struct {
		name    string
		content string
		opts    TruncationOptions
		check   func(t *testing.T, got TruncationResult)
	}{
		{
			// upstream: "truncates tail on UTF-8 boundaries when only a partial last line fits"
			name:    "truncates tail on UTF-8 boundaries when only a partial last line fits",
			content: "aé🙂b",
			opts:    TruncationOptions{MaxBytes: 5, MaxLines: 10},
			check: func(t *testing.T, got TruncationResult) {
				if got.Content != "🙂b" {
					t.Errorf("Content = %q, want %q", got.Content, "🙂b")
				}
				if !got.Truncated {
					t.Errorf("Truncated = false, want true")
				}
				if got.TruncatedBy != "bytes" {
					t.Errorf("TruncatedBy = %q, want %q", got.TruncatedBy, "bytes")
				}
				if !got.LastLinePartial {
					t.Errorf("LastLinePartial = false, want true")
				}
				if got.OutputBytes != 5 {
					t.Errorf("OutputBytes = %d, want 5", got.OutputBytes)
				}
			},
		},
		{
			// upstream: "truncates an oversized single line with a trailing newline"
			// (the 0.75.4 fixture named in AGENT-15)
			name:    "truncates an oversized single line with a trailing newline",
			content: strings.Repeat("X", 300_000) + "\n",
			opts:    TruncationOptions{MaxBytes: 1024, MaxLines: 100},
			check: func(t *testing.T, got TruncationResult) {
				want := strings.Repeat("X", 1024)
				if got.Content != want {
					t.Errorf("Content length = %d, want %d (want == 1024 X's)", len(got.Content), len(want))
				}
				if got.OutputBytes != 1024 {
					t.Errorf("OutputBytes = %d, want 1024", got.OutputBytes)
				}
				if got.OutputLines != 1 {
					t.Errorf("OutputLines = %d, want 1", got.OutputLines)
				}
				if !got.LastLinePartial {
					t.Errorf("LastLinePartial = false, want true")
				}
				if got.TruncatedBy != "bytes" {
					t.Errorf("TruncatedBy = %q, want %q", got.TruncatedBy, "bytes")
				}
			},
		},
		{
			// upstream: "drops an oversized trailing character when it cannot fit in tail byte limit"
			name:    "drops an oversized trailing character when it cannot fit in tail byte limit",
			content: "abc🙂",
			opts:    TruncationOptions{MaxBytes: 3, MaxLines: 10},
			check: func(t *testing.T, got TruncationResult) {
				if got.Content != "" {
					t.Errorf("Content = %q, want empty", got.Content)
				}
				if !got.Truncated {
					t.Errorf("Truncated = false, want true")
				}
				if got.TruncatedBy != "bytes" {
					t.Errorf("TruncatedBy = %q, want %q", got.TruncatedBy, "bytes")
				}
				if !got.LastLinePartial {
					t.Errorf("LastLinePartial = false, want true")
				}
				if got.OutputBytes != 0 {
					t.Errorf("OutputBytes = %d, want 0", got.OutputBytes)
				}
			},
		},
		{
			// authored: not in upstream truncate.test.ts. Added per the task
			// brief's minimum-pinned-case list ("tail truncation by line
			// limit keeps LAST maxLines lines") — the upstream suite never
			// exercises pure line-limit truncation in isolation.
			name:    "authored: tail truncation by line limit keeps LAST maxLines lines",
			content: "line1\nline2\nline3\nline4\nline5",
			opts:    TruncationOptions{MaxLines: 2, MaxBytes: 1000},
			check: func(t *testing.T, got TruncationResult) {
				want := "line4\nline5"
				if got.Content != want {
					t.Errorf("Content = %q, want %q", got.Content, want)
				}
				if !got.Truncated {
					t.Errorf("Truncated = false, want true")
				}
				if got.TruncatedBy != "lines" {
					t.Errorf("TruncatedBy = %q, want %q", got.TruncatedBy, "lines")
				}
				if got.OutputLines != 2 {
					t.Errorf("OutputLines = %d, want 2", got.OutputLines)
				}
				if got.TotalLines != 5 {
					t.Errorf("TotalLines = %d, want 5", got.TotalLines)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TruncateTail(tc.content, tc.opts)
			tc.check(t, got)
		})
	}
}

// upstream: "does not count a trailing newline as an extra line"
// Ported as its own function (rather than a TestTruncateHead/TestTruncateTail
// table row) because the upstream case calls both truncateHead and
// truncateTail against the same input.
func TestTruncateDoesNotCountTrailingNewlineAsExtraLine(t *testing.T) {
	content := strings.Repeat("line\n", 3) // "line\nline\nline\n"

	head := TruncateHead(content, TruncationOptions{MaxBytes: 100, MaxLines: 3})
	if head.Truncated {
		t.Errorf("TruncateHead.Truncated = true, want false")
	}
	if head.TotalLines != 3 {
		t.Errorf("TruncateHead.TotalLines = %d, want 3", head.TotalLines)
	}
	if head.OutputLines != 3 {
		t.Errorf("TruncateHead.OutputLines = %d, want 3", head.OutputLines)
	}

	tail := TruncateTail(content, TruncationOptions{MaxBytes: 100, MaxLines: 3})
	if tail.Truncated {
		t.Errorf("TruncateTail.Truncated = true, want false")
	}
	if tail.TotalLines != 3 {
		t.Errorf("TruncateTail.TotalLines = %d, want 3", tail.TotalLines)
	}
	if tail.OutputLines != 3 {
		t.Errorf("TruncateTail.OutputLines = %d, want 3", tail.OutputLines)
	}
}

// referenceTailTruncate is a from-scratch reference implementation of tail
// byte-truncation: take the raw last maxBytes bytes of content, then skip
// forward over any leading UTF-8 continuation-byte prefix so the result
// never splits a multi-byte character. This mirrors upstream's own
// `bufferTail` test helper (which truncates via Node's Buffer, a UTF-8 byte
// buffer) — Go strings are already UTF-8 byte sequences, so the reference
// and the code under test share the same underlying model, unlike upstream
// where the implementation walks UTF-16 code units and the test helper
// walks raw UTF-8 bytes.
func referenceTailTruncate(content string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(content) <= maxBytes {
		return content
	}
	start := len(content) - maxBytes
	for start < len(content) && content[start]&0xc0 == 0x80 {
		start++
	}
	return content[start:]
}

// sampledByteLimits mirrors upstream's sampledByteLimits: a handful of byte
// limits clustered around the interesting boundaries (start, middle, end,
// and total length) of content, deduplicated and sorted.
//
// Deviation from upstream: upstream's candidate list starts at 0, since JS's
// `{ maxBytes: 0 }` is distinguishable from "omitted" (`??` only substitutes
// on null/undefined) and truncateTail(content, { maxBytes: 0 }) really does
// mean a zero-byte budget. TruncationOptions.MaxBytes is a bare Go int, so
// per this package's documented "zero → defaults" contract,
// TruncationOptions{MaxBytes: 0} is indistinguishable from an unset field
// and always resolves to DefaultMaxBytes — there is no way to request a
// literal zero-byte budget through this API. Candidates are floored at 1
// accordingly (any formula that lands on 0, e.g. total/2-1 for a 2-byte
// content, is excluded too, not just the literal 0 entry).
func sampledByteLimits(content string) []int {
	total := len(content)
	candidates := []int{
		1, 2, 3, 4, 5, 8,
		total/2 - 1, total / 2, total/2 + 1,
		total - 8, total - 5, total - 4, total - 3, total - 2, total - 1,
		total, total + 1, total + 4,
	}
	seen := make(map[int]bool, len(candidates))
	var out []int
	for _, v := range candidates {
		if v < 1 || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

// assertMatchesByteTail mirrors upstream's assertMatchesBufferTail: checks
// TruncateTail against referenceTailTruncate across every limit in
// maxByteValues, and checks the output never exceeds the limit.
func assertMatchesByteTail(t *testing.T, content string, maxByteValues []int) {
	t.Helper()
	for _, maxBytes := range maxByteValues {
		got := TruncateTail(content, TruncationOptions{MaxBytes: maxBytes, MaxLines: 10})
		want := referenceTailTruncate(content, maxBytes)
		if got.Content != want {
			t.Errorf("TruncateTail(%q, maxBytes=%d).Content = %q, want %q", content, maxBytes, got.Content, want)
		}
		if len(got.Content) > maxBytes {
			t.Errorf("TruncateTail(%q, maxBytes=%d).Content is %d bytes, exceeds limit", content, maxBytes, len(got.Content))
		}
	}
}

// TestTruncateTailMatchesByteBoundarySemantics adapts two upstream cases:
//
//   - "matches Buffer tail truncation semantics for surrogate edge cases"
//   - "matches Buffer tail truncation semantics across deterministic fuzz cases"
//
// Both upstream cases exist to prove truncateTail's UTF-16-surrogate-aware
// byte walk never splits a character and matches Node's Buffer-based UTF-8
// truncation, even for malformed input (lone UTF-16 surrogates, which JS
// strings can hold but which do not correspond to any single Unicode code
// point). Go strings hold arbitrary bytes, not UTF-16 code units, so a
// "lone surrogate" is not a concept that exists in Go: 5 of the 6 upstream
// surrogate-edge-case inputs are rejected outright — one lone high surrogate
// followed by "a", one lone low surrogate followed by "b", one "a" followed
// by a lone low surrogate then "b", one high-surrogate/high-surrogate/
// low-surrogate triple, and one high-surrogate/low-surrogate/low-surrogate
// triple — and 4 of the 15 upstream fuzz alphabet entries (a lone high
// surrogate used two ways, plus a lone low surrogate used two ways) are
// lone/unpaired surrogate code units on their own and are REJECTED BY THE GO
// COMPILER as invalid Unicode code points in a string/rune literal — they
// cannot be written in Go at all, let alone tested. This is the direct consequence of
// the task's translation note to skip replaceUnpairedSurrogates (see
// truncateStringToBytesFromEnd's doc comment).
//
// What's preserved: the one upstream surrogate-edge-case input that IS a
// valid, representable Unicode string ("👩‍💻", a ZWJ emoji sequence with no
// lone surrogates) and all 11 upstream fuzz alphabet entries that are valid
// standalone Unicode code points (ASCII, 2-byte, 3-byte, and 4-byte UTF-8
// sequences). The exhaustive-tree depth (3) and fuzz iteration count (1000)
// are ported as-is; the deterministic LCG mirrors upstream's algorithm
// (same recurrence, wrapped to 32 bits) for a stable, reproducible sequence
// on the Go side, but is not expected to reproduce upstream's exact byte
// sequence since the alphabet differs.
func TestTruncateTailMatchesByteBoundarySemantics(t *testing.T) {
	assertMatchesByteTail(t, "👩‍💻", sampledByteLimits("👩‍💻"))

	alphabet := []string{
		"a",
		"",
		"",
		"é",
		"߿",
		"ࠀ",
		"中",
		"퟿",
		"🙂",
		"",
		"￿",
	}

	var checkExhaustive func(prefix string, depth int)
	checkExhaustive = func(prefix string, depth int) {
		assertMatchesByteTail(t, prefix, sampledByteLimits(prefix))
		if depth == 0 {
			return
		}
		for _, ch := range alphabet {
			checkExhaustive(prefix+ch, depth-1)
		}
	}
	checkExhaustive("", 3)

	seed := uint32(0x12345678)
	random := func() float64 {
		seed = seed*1664525 + 1013904223
		return float64(seed) / float64(0x100000000)
	}
	for i := 0; i < 1_000; i++ {
		var b strings.Builder
		length := int(random() * 80)
		for j := 0; j < length; j++ {
			b.WriteString(alphabet[int(random()*float64(len(alphabet)))])
		}
		input := b.String()
		assertMatchesByteTail(t, input, sampledByteLimits(input))
	}
}

func TestFormatSize(t *testing.T) {
	// Expected strings cross-checked against upstream's actual formatSize()
	// (packages/agent/src/harness/utils/truncate.ts @0.82.0, lines 115-123)
	// executed under Node v26 for each input below, to pin exact JS
	// toFixed(1) rounding behavior (including 1048575, which rounds up to
	// the next whole KB: "1024.0KB").
	tests := []struct {
		name  string
		bytes int
		want  string
	}{
		{"zero bytes", 0, "0B"},
		{"one byte", 1, "1B"},
		{"512 bytes", 512, "512B"},
		{"1000 bytes", 1000, "1000B"},
		{"1023 bytes (just under 1KB)", 1023, "1023B"},
		{"1024 bytes (exactly 1KB)", 1024, "1.0KB"},
		{"1025 bytes", 1025, "1.0KB"},
		{"1536 bytes", 1536, "1.5KB"},
		{"1587 bytes", 1587, "1.5KB"},
		{"1638 bytes", 1638, "1.6KB"},
		{"2048 bytes", 2048, "2.0KB"},
		{"DefaultMaxBytes (50KB)", 51200, "50.0KB"},
		{"1048575 bytes (just under 1MB, rounds up to 1024.0KB)", 1048575, "1024.0KB"},
		{"1048576 bytes (exactly 1MB)", 1048576, "1.0MB"},
		{"1048577 bytes", 1048577, "1.0MB"},
		{"1258291 bytes (~1.2MB)", 1258291, "1.2MB"},
		{"5MB", 5 * 1024 * 1024, "5.0MB"},
		{"123456789 bytes", 123456789, "117.7MB"},
		{"10MB", 10485760, "10.0MB"},
		{"999424 bytes", 999424, "976.0KB"},
		{"99328 bytes", 99328, "97.0KB"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatSize(tc.bytes); got != tc.want {
				t.Errorf("FormatSize(%d) = %q, want %q", tc.bytes, got, tc.want)
			}
		})
	}
}
