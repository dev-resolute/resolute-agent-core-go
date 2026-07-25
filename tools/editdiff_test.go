package tools

import (
	"fmt"
	"strings"
	"testing"
)

// editdiff_test.go exercises editdiff.go, the port of
// packages/agent/src/harness/tools/edit-diff.ts @0.82.0.
//
// Cases ported from packages/agent/test/harness/tools.test.ts's "edit"
// describe block are noted per-case below; that suite drives the edit TOOL
// (createEditTool, mutation queue, param parsing) which is Task 11's
// concern - only the engine-level behavior each case actually exercises is
// ported here.

func TestDetectLineEnding(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"LF only", "a\nb\nc\n", "\n"},
		{"CRLF only", "a\r\nb\r\nc\r\n", "\r\n"},
		{"CRLF occurs before first bare LF", "a\r\nb\nc", "\r\n"},
		{"bare LF occurs before first CRLF", "a\nb\r\nc", "\n"},
		{"no newlines at all", "abc", "\n"},
		{"empty content", "", "\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectLineEnding(tc.content); got != tc.want {
				t.Errorf("DetectLineEnding(%q) = %q, want %q", tc.content, got, tc.want)
			}
		})
	}
}

func TestNormalizeToLF(t *testing.T) {
	tests := []struct{ name, text, want string }{
		{"CRLF to LF", "a\r\nb\r\n", "a\nb\n"},
		{"lone CR to LF", "a\rb\r", "a\nb\n"},
		{"mixed CRLF and lone CR", "a\r\nb\rc\n", "a\nb\nc\n"},
		{"already LF", "a\nb\n", "a\nb\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeToLF(tc.text); got != tc.want {
				t.Errorf("NormalizeToLF(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

func TestRestoreLineEndings(t *testing.T) {
	tests := []struct {
		name, text, ending, want string
	}{
		{"restores CRLF", "a\nb\n", "\r\n", "a\r\nb\r\n"},
		{"leaves LF alone", "a\nb\n", "\n", "a\nb\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RestoreLineEndings(tc.text, tc.ending); got != tc.want {
				t.Errorf("RestoreLineEndings(%q, %q) = %q, want %q", tc.text, tc.ending, got, tc.want)
			}
		})
	}
}

func TestStripBOM(t *testing.T) {
	tests := []struct {
		name, content, wantBOM, wantText string
	}{
		{"BOM present", "\uFEFFhello", "\uFEFF", "hello"},
		{"no BOM", "hello", "", "hello"},
		{"BOM only", "\uFEFF", "\uFEFF", ""},
		{"empty content", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotBOM, gotText := StripBOM(tc.content)
			if gotBOM != tc.wantBOM || gotText != tc.wantText {
				t.Errorf("StripBOM(%q) = (%q, %q), want (%q, %q)", tc.content, gotBOM, gotText, tc.wantBOM, tc.wantText)
			}
		})
	}
}

func TestNormalizeForFuzzyMatch(t *testing.T) {
	tests := []struct{ name, text, want string }{
		{"strips trailing whitespace per line", "foo   \nbar\t\n", "foo\nbar\n"},
		{"smart single quotes to ASCII", "\u2018hi\u2019 \u201Ait\u201B", "'hi' 'it'"},
		{"smart double quotes to ASCII", "\u201Chi\u201D \u201Eit\u201F", "\"hi\" \"it\""},
		{"unicode dashes to ASCII hyphen", "a\u2010b\u2011c\u2012d\u2013e\u2014f\u2015g\u2212h", "a-b-c-d-e-f-g-h"},
		{"unicode spaces to ASCII space", "a\u00A0b\u2002c\u2007d\u202Fe\u3000f", "a b c d e f"},
		{"NFKC normalizes fullwidth to ASCII", "\uFF21\uFF22\uFF23", "ABC"},
		{"unchanged plain text", "hello world", "hello world"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeForFuzzyMatch(tc.text); got != tc.want {
				t.Errorf("NormalizeForFuzzyMatch(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

func TestFuzzyFindText(t *testing.T) {
	t.Run("exact match returns offsets into content", func(t *testing.T) {
		start, end, found := FuzzyFindText("hello world", "world")
		if !found || start != 6 || end != 11 {
			t.Errorf("FuzzyFindText = (%d, %d, %v), want (6, 11, true)", start, end, found)
		}
	})

	t.Run("fuzzy match returns offsets into the fuzzy-normalized content", func(t *testing.T) {
		content := "hello   \nworld"
		oldText := "hello\nworld"
		start, end, found := FuzzyFindText(content, oldText)
		if !found {
			t.Fatalf("FuzzyFindText(%q, %q) found = false, want true", content, oldText)
		}
		normalized := NormalizeForFuzzyMatch(content)
		if got := normalized[start:end]; got != "hello\nworld" {
			t.Errorf("normalized[%d:%d] = %q, want %q", start, end, got, "hello\nworld")
		}
	})

	t.Run("not found", func(t *testing.T) {
		start, end, found := FuzzyFindText("hello", "xyz")
		if found || start != 0 || end != 0 {
			t.Errorf("FuzzyFindText = (%d, %d, %v), want (0, 0, false)", start, end, found)
		}
	})
}

func TestApplyEditsToNormalizedContent(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		edits       []Edit
		path        string
		wantBase    string
		wantUpdated string
		wantErrIs   string // exact error message, when the case expects an error
	}{
		{
			// tools.test.ts "applies disjoint edits and returns both diff
			// formats" - engine-level essence: two disjoint exact-text
			// edits against the original content both land correctly.
			name:        "exact unique match replaces, multiple disjoint edits",
			content:     "alpha\nbeta\ngamma\ndelta\n",
			edits:       []Edit{{OldText: "alpha\n", NewText: "ALPHA\n"}, {OldText: "gamma\n", NewText: "GAMMA\n"}},
			path:        "edit.txt",
			wantBase:    "alpha\nbeta\ngamma\ndelta\n",
			wantUpdated: "ALPHA\nbeta\nGAMMA\ndelta\n",
		},
		{
			// Pinned case: whitespace-differing oldText still matches via
			// fuzzy normalization, AND the file's other unchanged lines
			// (here, line 2's own trailing whitespace) are preserved
			// byte-for-byte rather than being silently reformatted.
			name:        "fuzzy whitespace match preserves other unchanged lines verbatim",
			content:     "function foo() {   \n    return 1;   \n}\n",
			edits:       []Edit{{OldText: "function foo() {\n", NewText: "function bar() {\n"}},
			path:        "edit.txt",
			wantBase:    "function foo() {   \n    return 1;   \n}\n",
			wantUpdated: "function bar() {\n    return 1;   \n}\n",
		},
		{
			// tools.test.ts "matches all edits against the original and
			// rejects overlaps".
			name:    "overlapping edits are rejected",
			content: "one\ntwo\nthree\n",
			edits:   []Edit{{OldText: "one\ntwo\n", NewText: "ONE\nTWO\n"}, {OldText: "two\nthree\n", NewText: "TWO\nTHREE\n"}},
			path:    "edit.txt",
			wantErrIs: "edits[0] and edits[1] overlap in edit.txt. " +
				"Merge them into one edit or target disjoint regions.",
		},
		{
			// tools.test.ts "rejects missing and duplicate target text"
			// (missing half), totalEdits==1 message variant.
			name:      "not found, single edit message",
			content:   "foo foo foo",
			edits:     []Edit{{OldText: "bar", NewText: "baz"}},
			path:      "edit.txt",
			wantErrIs: "Could not find the exact text in edit.txt. The old text must match exactly including all whitespace and newlines.",
		},
		{
			name:    "not found, multi-edit message names the index",
			content: "one two three",
			edits:   []Edit{{OldText: "one", NewText: "ONE"}, {OldText: "missing", NewText: "X"}},
			path:    "edit.txt",
			wantErrIs: "Could not find edits[1] in edit.txt. " +
				"The oldText must match exactly including all whitespace and newlines.",
		},
		{
			// tools.test.ts "rejects missing and duplicate target text"
			// (duplicate half), totalEdits==1 message variant.
			name:      "duplicate match, single edit message names the count",
			content:   "foo foo foo",
			edits:     []Edit{{OldText: "foo", NewText: "bar"}},
			path:      "edit.txt",
			wantErrIs: "Found 3 occurrences of the text in edit.txt. The text must be unique. Please provide more context to make it unique.",
		},
		{
			name:    "duplicate match, multi-edit message names the index and count",
			content: "aaa bbb bbb",
			edits:   []Edit{{OldText: "aaa", NewText: "AAA"}, {OldText: "bbb", NewText: "BBB"}},
			path:    "edit.txt",
			wantErrIs: "Found 2 occurrences of edits[1] in edit.txt. " +
				"Each oldText must be unique. Please provide more context to make it unique.",
		},
		{
			// Pinned case: zero edits rejected with the tool-level message,
			// verbatim - see ApplyEditsToNormalizedContent's doc comment for
			// why this guard lives in the engine.
			name:      "zero edits rejected",
			content:   "anything",
			edits:     nil,
			path:      "edit.txt",
			wantErrIs: "Edit tool input is invalid. edits must contain at least one replacement.",
		},
		{
			name:      "empty oldText rejected, single edit message",
			content:   "anything",
			edits:     []Edit{{OldText: "", NewText: "X"}},
			path:      "edit.txt",
			wantErrIs: "oldText must not be empty in edit.txt.",
		},
		{
			name:      "empty oldText rejected, multi-edit message names the index",
			content:   "anything",
			edits:     []Edit{{OldText: "foo", NewText: "bar"}, {OldText: "", NewText: "X"}},
			path:      "edit.txt",
			wantErrIs: "edits[1].oldText must not be empty in edit.txt.",
		},
		{
			name:      "no-op replacement rejected, single edit message",
			content:   "same\n",
			edits:     []Edit{{OldText: "same\n", NewText: "same\n"}},
			path:      "edit.txt",
			wantErrIs: "No changes made to edit.txt. The replacement produced identical content. This might indicate an issue with special characters or the text not existing as expected.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base, updated, err := ApplyEditsToNormalizedContent(tc.content, tc.edits, tc.path)
			if tc.wantErrIs != "" {
				if err == nil {
					t.Fatalf("ApplyEditsToNormalizedContent() err = nil, want %q", tc.wantErrIs)
				}
				if err.Error() != tc.wantErrIs {
					t.Errorf("ApplyEditsToNormalizedContent() err = %q, want %q", err.Error(), tc.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("ApplyEditsToNormalizedContent() unexpected err = %v", err)
			}
			if base != tc.wantBase {
				t.Errorf("ApplyEditsToNormalizedContent() base = %q, want %q", base, tc.wantBase)
			}
			if updated != tc.wantUpdated {
				t.Errorf("ApplyEditsToNormalizedContent() updated = %q, want %q", updated, tc.wantUpdated)
			}
		})
	}
}

// TestApplyEditsToNormalizedContent_CRLFAndBOMRoundTrip ports tools.test.ts's
// "preserves BOM and CRLF line endings" case at the engine level: the edit
// tool's own pipeline is StripBOM -> DetectLineEnding -> NormalizeToLF ->
// ApplyEditsToNormalizedContent -> RestoreLineEndings, with the BOM
// reattached last. This exercises exactly that pipeline without the edit
// TOOL/mutation-queue/file-I/O layer (Task 11).
func TestApplyEditsToNormalizedContent_CRLFAndBOMRoundTrip(t *testing.T) {
	original := "\uFEFFone\r\ntwo\r\n"

	bomOut, content := StripBOM(original)
	if bomOut != "\uFEFF" {
		t.Fatalf("StripBOM bom = %q, want %q", bomOut, "\uFEFF")
	}

	ending := DetectLineEnding(content)
	if ending != "\r\n" {
		t.Fatalf("DetectLineEnding = %q, want %q", ending, "\r\n")
	}

	normalized := NormalizeToLF(content)
	base, updated, err := ApplyEditsToNormalizedContent(normalized, []Edit{{OldText: "two", NewText: "TWO"}}, "edit.txt")
	if err != nil {
		t.Fatalf("ApplyEditsToNormalizedContent() unexpected err = %v", err)
	}
	if base != "one\ntwo\n" {
		t.Fatalf("base = %q, want %q", base, "one\ntwo\n")
	}

	final := bomOut + RestoreLineEndings(updated, ending)
	want := "\uFEFFone\r\nTWO\r\n"
	if final != want {
		t.Errorf("final content = %q, want %q", final, want)
	}
}

func TestGenerateUnifiedPatch(t *testing.T) {
	t.Run("headers and single hunk covering the whole (short) file", func(t *testing.T) {
		old := "alpha\nbeta\ngamma\ndelta\n"
		newC := "ALPHA\nbeta\nGAMMA\ndelta\n"

		patch := GenerateUnifiedPatch("edit.txt", old, newC, 0)

		// NOTE: upstream's own generateUnifiedPatch (edit-diff.ts) calls
		// Diff.createTwoFilesPatch(path, path, ...) - i.e. BOTH file names
		// are the bare path, with no git-style "a/"/"b/" prefix. Ported
		// verbatim; see this task's report for the discrepancy against the
		// plan brief's paraphrase.
		wantHeader := "--- edit.txt\n+++ edit.txt\n"
		if !strings.HasPrefix(patch, wantHeader) {
			t.Fatalf("GenerateUnifiedPatch() = %q, want prefix %q", patch, wantHeader)
		}
		if !strings.Contains(patch, "@@ -1,4 +1,4 @@\n") {
			t.Errorf("GenerateUnifiedPatch() = %q, want hunk header @@ -1,4 +1,4 @@", patch)
		}
		for _, want := range []string{"-alpha", "+ALPHA", "-gamma", "+GAMMA", " beta", " delta"} {
			if !strings.Contains(patch, want) {
				t.Errorf("GenerateUnifiedPatch() = %q, want to contain %q", patch, want)
			}
		}
		if !strings.HasSuffix(patch, "\n") {
			t.Errorf("GenerateUnifiedPatch() = %q, want trailing newline", patch)
		}
	})

	t.Run("contextLines 0 defaults to 4", func(t *testing.T) {
		old := "alpha\nbeta\ngamma\ndelta\n"
		newC := "ALPHA\nbeta\nGAMMA\ndelta\n"
		got0 := GenerateUnifiedPatch("edit.txt", old, newC, 0)
		got4 := GenerateUnifiedPatch("edit.txt", old, newC, 4)
		if got0 != got4 {
			t.Errorf("GenerateUnifiedPatch(..., 0) = %q, GenerateUnifiedPatch(..., 4) = %q, want equal", got0, got4)
		}
	})

	t.Run("distant changes produce separate hunks", func(t *testing.T) {
		var oldLines, newLines []string
		for i := 1; i <= 20; i++ {
			line := fmt.Sprintf("line%d", i)
			if i == 5 {
				oldLines = append(oldLines, line)
				newLines = append(newLines, "LINE5")
				continue
			}
			oldLines = append(oldLines, line)
			newLines = append(newLines, line)
		}
		newLines[14] = "LINE15" // 0-indexed line 15
		old := strings.Join(oldLines, "\n") + "\n"
		newC := strings.Join(newLines, "\n") + "\n"

		patch := GenerateUnifiedPatch("edit.txt", old, newC, 4)
		if got := strings.Count(patch, "@@ "); got != 2 {
			t.Errorf("GenerateUnifiedPatch() hunk count = %d, want 2 in:\n%s", got, patch)
		}
	})

	t.Run("no trailing newline gets a marker on both sides of the change", func(t *testing.T) {
		old := "line1\nline2"
		newC := "line1\nLINE2"
		patch := GenerateUnifiedPatch("edit.txt", old, newC, 4)

		if !strings.Contains(patch, "@@ -1,2 +1,2 @@\n") {
			t.Errorf("GenerateUnifiedPatch() = %q, want hunk header @@ -1,2 +1,2 @@", patch)
		}
		if got := strings.Count(patch, "\\ No newline at end of file"); got != 2 {
			t.Errorf("GenerateUnifiedPatch() \"\\ No newline\" marker count = %d, want 2 in:\n%s", got, patch)
		}
	})
}

func TestGenerateDiffString(t *testing.T) {
	t.Run("firstChangedLine is 1-indexed into the new content", func(t *testing.T) {
		old := "alpha\nbeta\ngamma\ndelta\n"
		newC := "ALPHA\nbeta\nGAMMA\ndelta\n"

		diff, firstChangedLine := GenerateDiffString(old, newC)
		if firstChangedLine != 1 {
			t.Errorf("firstChangedLine = %d, want 1", firstChangedLine)
		}
		for _, want := range []string{"-1 alpha", "+1 ALPHA"} {
			if !strings.Contains(diff, want) {
				t.Errorf("diff = %q, want to contain %q", diff, want)
			}
		}
	})

	t.Run("identical content produces an empty diff and firstChangedLine 0", func(t *testing.T) {
		content := "alpha\nbeta\ngamma\n"
		diff, firstChangedLine := GenerateDiffString(content, content)
		if diff != "" {
			t.Errorf("diff = %q, want empty", diff)
		}
		if firstChangedLine != 0 {
			t.Errorf("firstChangedLine = %d, want 0", firstChangedLine)
		}
	})

	t.Run("large unchanged runs on both sides of a single change are elided", func(t *testing.T) {
		var oldLines, newLines []string
		for i := 1; i <= 30; i++ {
			line := fmt.Sprintf("line%d", i)
			oldLines = append(oldLines, line)
			if i == 15 {
				newLines = append(newLines, "CHANGED")
			} else {
				newLines = append(newLines, line)
			}
		}
		old := strings.Join(oldLines, "\n") + "\n"
		newC := strings.Join(newLines, "\n") + "\n"

		diff, firstChangedLine := GenerateDiffString(old, newC)
		if firstChangedLine != 15 {
			t.Errorf("firstChangedLine = %d, want 15", firstChangedLine)
		}
		if got := strings.Count(diff, "..."); got != 2 {
			t.Errorf("elision marker count = %d, want 2 in:\n%s", got, diff)
		}
		// The window immediately around the change (contextLines=4) must be
		// present...
		for _, want := range []string{"11 line11", "14 line14", "-15 line15", "+15 CHANGED", "16 line16", "19 line19"} {
			if !strings.Contains(diff, want) {
				t.Errorf("diff = %q, want to contain %q", diff, want)
			}
		}
		// ...and lines far from the change must have been elided, not shown.
		for _, notWant := range []string{"line1\n", " line5 ", "line25"} {
			if strings.Contains(diff, notWant) {
				t.Errorf("diff = %q, want NOT to contain %q", diff, notWant)
			}
		}
	})
}
