package tools

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// editdiff.go ports packages/agent/src/harness/tools/edit-diff.ts from
// upstream pi @0.82.0: the diff engine shared by the edit tool (see Task 11)
// and any other tool that needs exact-text replacement with fuzzy fallback,
// BOM/CRLF-preserving I/O, and human- or patch-oriented diff rendering.
//
// Upstream leans on the "diff" npm package (jsdiff, pinned here at the same
// 8.0.4 the upstream repo depends on) for its line-level diff and unified
// patch formatting. Go has no equivalent in the standard library and this
// package intentionally takes on no new dependency, so computeLineDiff below
// is a direct port of jsdiff's Diff.diff base algorithm (Myers' O(ND)
// shortest-edit-script search, src/diff/base.js) specialized to line
// tokens (src/diff/line.js's tokenize), and buildUnifiedHunks is a port of
// jsdiff's structuredPatch/formatPatch (src/patch/create.js) restricted to
// the single call shape edit-diff.ts actually uses: two-file-name headers,
// no index/underline lines, no multi-file patches, no async callback API.
//
// One deliberate divergence from upstream, called out explicitly because it
// crosses the edit-diff.ts / edit.ts (Task 11) boundary: upstream's "edits
// must contain at least one replacement" check and its exact message live in
// edit.ts's validateEditInput, not in edit-diff.ts's
// applyEditsToNormalizedContent. ApplyEditsToNormalizedContent below
// reproduces that exact message as a guard of its own, so the engine is
// self-defending regardless of which caller invokes it - see that function's
// doc comment.

// Edit is one targeted oldText -> newText replacement. Ported from
// upstream's Edit interface.
type Edit struct {
	OldText string
	NewText string
}

// DetectLineEnding reports which line ending predominates at the START of
// content: it looks at whichever of the first "\r\n" or the first "\n"
// occurs earlier in content, and returns "\r\n" only if a CRLF occurs no
// later than the first bare LF. Ported from upstream's detectLineEnding.
func DetectLineEnding(content string) string {
	crlfIdx := strings.Index(content, "\r\n")
	lfIdx := strings.Index(content, "\n")
	if lfIdx == -1 {
		return "\n"
	}
	if crlfIdx == -1 {
		return "\n"
	}
	if crlfIdx < lfIdx {
		return "\r\n"
	}
	return "\n"
}

// NormalizeToLF collapses every "\r\n" and lone "\r" in text down to "\n".
// Ported from upstream's normalizeToLF.
func NormalizeToLF(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n")
}

// RestoreLineEndings expands every "\n" in text back to ending ("\r\n" or
// "\n"). Ported from upstream's restoreLineEndings.
func RestoreLineEndings(text, ending string) string {
	if ending == "\r\n" {
		return strings.ReplaceAll(text, "\n", "\r\n")
	}
	return text
}

// bom is the UTF-8 encoding of U+FEFF, the byte-order mark some editors
// prepend to text files.
const bom = "\uFEFF"

// StripBOM splits a leading UTF-8 BOM off content, if present. It returns
// the BOM (or "" if content had none) and the remaining text. Ported from
// upstream's stripBom.
func StripBOM(content string) (bomOut, text string) {
	if strings.HasPrefix(content, bom) {
		return bom, content[len(bom):]
	}
	return "", content
}

// Regexes ported verbatim (by codepoint) from upstream's
// normalizeForFuzzyMatch: smart single quotes, smart double quotes,
// Unicode dashes/hyphens, and special Unicode spaces, each normalized to
// their plain-ASCII equivalent.
var (
	fuzzySmartSingleQuotes = regexp.MustCompile("[\u2018\u2019\u201A\u201B]")
	fuzzySmartDoubleQuotes = regexp.MustCompile("[\u201C\u201D\u201E\u201F]")
	fuzzyDashes            = regexp.MustCompile("[\u2010\u2011\u2012\u2013\u2014\u2015\u2212]")
	fuzzySpaces            = regexp.MustCompile("[\u00A0\u2002-\u200A\u202F\u205F\u3000]")
)

// NormalizeForFuzzyMatch normalizes text for fuzzy matching by applying,
// in order: Unicode NFKC normalization, stripping trailing whitespace from
// each line, normalizing smart quotes to ASCII equivalents, normalizing
// Unicode dashes/hyphens to ASCII hyphen, and normalizing special Unicode
// spaces to a regular space. Ported from upstream's normalizeForFuzzyMatch.
func NormalizeForFuzzyMatch(text string) string {
	normalized := norm.NFKC.String(text)

	lines := strings.Split(normalized, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRightFunc(line, unicode.IsSpace)
	}
	result := strings.Join(lines, "\n")

	result = fuzzySmartSingleQuotes.ReplaceAllString(result, "'")
	result = fuzzySmartDoubleQuotes.ReplaceAllString(result, `"`)
	result = fuzzyDashes.ReplaceAllString(result, "-")
	result = fuzzySpaces.ReplaceAllString(result, " ")
	return result
}

// fuzzyMatchResult is the internal, upstream-shaped counterpart of
// upstream's FuzzyMatchResult - it additionally records whether fuzzy
// matching was needed and which content string (original vs.
// fuzzy-normalized) the offsets are relative to, both of which
// ApplyEditsToNormalizedContent needs but the exported FuzzyFindText
// signature (fixed by the task interface) has no room for.
type fuzzyMatchResult struct {
	found                 bool
	index                 int
	matchLength           int
	usedFuzzyMatch        bool
	contentForReplacement string
}

// fuzzyFindText finds oldText in content, trying an exact substring match
// first, then a fuzzy match in NormalizeForFuzzyMatch space. Ported from
// upstream's fuzzyFindText.
func fuzzyFindText(content, oldText string) fuzzyMatchResult {
	if idx := strings.Index(content, oldText); idx != -1 {
		return fuzzyMatchResult{
			found:                 true,
			index:                 idx,
			matchLength:           len(oldText),
			usedFuzzyMatch:        false,
			contentForReplacement: content,
		}
	}

	fuzzyContent := NormalizeForFuzzyMatch(content)
	fuzzyOldText := NormalizeForFuzzyMatch(oldText)
	idx := strings.Index(fuzzyContent, fuzzyOldText)
	if idx == -1 {
		return fuzzyMatchResult{found: false, index: -1, contentForReplacement: content}
	}

	return fuzzyMatchResult{
		found:                 true,
		index:                 idx,
		matchLength:           len(fuzzyOldText),
		usedFuzzyMatch:        true,
		contentForReplacement: fuzzyContent,
	}
}

// FuzzyFindText locates oldText within content and reports the byte range
// [start, end) of the match. It tries an exact substring match first; on
// exact match, start/end are byte offsets into content itself. If no exact
// match exists, it retries against NormalizeForFuzzyMatch(content) and
// NormalizeForFuzzyMatch(oldText) - on a fuzzy match, start/end are byte
// offsets into NormalizeForFuzzyMatch(content), NOT into content, since
// fuzzy normalization can change byte length (trimming trailing whitespace,
// collapsing multi-byte Unicode quotes/dashes/spaces to single-byte ASCII).
// found is false, with start=end=0, when neither match succeeds. Ported
// from upstream's fuzzyFindText.
func FuzzyFindText(content, oldText string) (start, end int, found bool) {
	m := fuzzyFindText(content, oldText)
	if !m.found {
		return 0, 0, false
	}
	return m.index, m.index + m.matchLength, true
}

// lineSpan is the [start, end) byte range of one line (including its
// trailing newline, if any) within a content string.
type lineSpan struct {
	start, end int
}

// splitLinesWithEndings splits content into lines, each retaining its
// trailing "\n" (the final line has none if content doesn't end in "\n").
// Returns nil for "". Ported from upstream's splitLinesWithEndings (regex
// /[^\n]*\n|[^\n]+/g).
func splitLinesWithEndings(content string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			lines = append(lines, content[start:i+1])
			start = i + 1
		}
	}
	if start < len(content) {
		lines = append(lines, content[start:])
	}
	return lines
}

// getLineSpans returns the byte-offset span of each line in content, in the
// same partitioning as splitLinesWithEndings. Ported from upstream's
// getLineSpans.
func getLineSpans(content string) []lineSpan {
	lines := splitLinesWithEndings(content)
	spans := make([]lineSpan, len(lines))
	offset := 0
	for i, line := range lines {
		spans[i] = lineSpan{start: offset, end: offset + len(line)}
		offset = spans[i].end
	}
	return spans
}

// getReplacementLineRange finds the [startLine, endLine) range (endLine
// exclusive) of lines a replacement's match region touches. Returns an
// error if the replacement's [matchIndex, matchIndex+matchLength) range
// isn't fully covered by lines. Ported from upstream's
// getReplacementLineRange.
func getReplacementLineRange(lines []lineSpan, r matchedEdit) (startLine, endLine int, err error) {
	replacementStart := r.matchIndex
	replacementEnd := r.matchIndex + r.matchLength

	startLine = -1
	for i, line := range lines {
		if replacementStart >= line.start && replacementStart < line.end {
			startLine = i
			break
		}
	}
	if startLine == -1 {
		return 0, 0, errors.New("Replacement range is outside the base content.")
	}

	endLine = startLine
	for endLine < len(lines) && lines[endLine].end < replacementEnd {
		endLine++
	}
	if endLine >= len(lines) {
		return 0, 0, errors.New("Replacement range is outside the base content.")
	}

	return startLine, endLine + 1, nil
}

// applyReplacements applies replacements (already sorted ascending by
// matchIndex, offsets relative to offset) to content in reverse order so
// earlier offsets stay valid as later ones are consumed. Ported from
// upstream's applyReplacements.
func applyReplacements(content string, replacements []matchedEdit, offset int) string {
	result := content
	for i := len(replacements) - 1; i >= 0; i-- {
		r := replacements[i]
		matchIndex := r.matchIndex - offset
		result = result[:matchIndex] + r.newText + result[matchIndex+r.matchLength:]
	}
	return result
}

// applyReplacementsPreservingUnchangedLines applies replacements matched
// against baseContent to originalContent, preserving originalContent's
// exact bytes on every line the replacements don't touch. baseContent must
// have the same line count as originalContent (e.g. as a fuzzy-normalized
// view of it); replacements' offsets are relative to baseContent. Ported
// from upstream's applyReplacementsPreservingUnchangedLines.
func applyReplacementsPreservingUnchangedLines(originalContent, baseContent string, replacements []matchedEdit) (string, error) {
	originalLines := splitLinesWithEndings(originalContent)
	baseLines := getLineSpans(baseContent)
	if len(originalLines) != len(baseLines) {
		return "", errors.New("Cannot preserve unchanged lines because the base content has a different line count.")
	}

	type replacementGroup struct {
		startLine, endLine int
		replacements       []matchedEdit
	}

	sorted := make([]matchedEdit, len(replacements))
	copy(sorted, replacements)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].matchIndex < sorted[j].matchIndex })

	var groups []replacementGroup
	for _, r := range sorted {
		startLine, endLine, err := getReplacementLineRange(baseLines, r)
		if err != nil {
			return "", err
		}
		if n := len(groups); n > 0 && startLine < groups[n-1].endLine {
			if endLine > groups[n-1].endLine {
				groups[n-1].endLine = endLine
			}
			groups[n-1].replacements = append(groups[n-1].replacements, r)
			continue
		}
		groups = append(groups, replacementGroup{startLine: startLine, endLine: endLine, replacements: []matchedEdit{r}})
	}

	originalLineIndex := 0
	var result strings.Builder
	for _, g := range groups {
		for _, line := range originalLines[originalLineIndex:g.startLine] {
			result.WriteString(line)
		}

		groupStartOffset := baseLines[g.startLine].start
		groupEndOffset := baseLines[g.endLine-1].end
		result.WriteString(applyReplacements(baseContent[groupStartOffset:groupEndOffset], g.replacements, groupStartOffset))
		originalLineIndex = g.endLine
	}
	for _, line := range originalLines[originalLineIndex:] {
		result.WriteString(line)
	}

	return result.String(), nil
}

// matchedEdit is one edit that has been located in the replacement-base
// content, carrying enough to apply it and to report its origin (editIndex)
// in error messages. Ported from upstream's MatchedEdit (narrowed to what
// TextReplacement / applyReplacements need, matching upstream's own
// TextReplacement = Pick<MatchedEdit, "matchIndex" | "matchLength" |
// "newText"> split).
type matchedEdit struct {
	editIndex   int
	matchIndex  int
	matchLength int
	newText     string
}

func getNotFoundError(path string, editIndex, totalEdits int) error {
	if totalEdits == 1 {
		return fmt.Errorf("Could not find the exact text in %s. The old text must match exactly including all whitespace and newlines.", path)
	}
	return fmt.Errorf("Could not find edits[%d] in %s. The oldText must match exactly including all whitespace and newlines.", editIndex, path)
}

func getDuplicateError(path string, editIndex, totalEdits, occurrences int) error {
	if totalEdits == 1 {
		return fmt.Errorf("Found %d occurrences of the text in %s. The text must be unique. Please provide more context to make it unique.", occurrences, path)
	}
	return fmt.Errorf("Found %d occurrences of edits[%d] in %s. Each oldText must be unique. Please provide more context to make it unique.", occurrences, editIndex, path)
}

func getEmptyOldTextError(path string, editIndex, totalEdits int) error {
	if totalEdits == 1 {
		return fmt.Errorf("oldText must not be empty in %s.", path)
	}
	return fmt.Errorf("edits[%d].oldText must not be empty in %s.", editIndex, path)
}

func getNoChangeError(path string, totalEdits int) error {
	if totalEdits == 1 {
		return fmt.Errorf("No changes made to %s. The replacement produced identical content. This might indicate an issue with special characters or the text not existing as expected.", path)
	}
	return fmt.Errorf("No changes made to %s. The replacements produced identical content.", path)
}

// countOccurrences counts non-overlapping occurrences of oldText in content,
// both compared in fuzzy-normalized space. Ported from upstream's
// countOccurrences.
func countOccurrences(content, oldText string) int {
	return strings.Count(NormalizeForFuzzyMatch(content), NormalizeForFuzzyMatch(oldText))
}

// ApplyEditsToNormalizedContent applies one or more exact-text replacements
// to LF-normalized content. All edits are matched against the same original
// content; replacements are then applied in reverse offset order so offsets
// stay stable. If any edit needs fuzzy matching, the operation runs in
// fuzzy-normalized content space and then overlays those line-level changes
// onto content so unchanged line blocks keep their original bytes. path is
// used only to name the file in error messages. Ported from upstream's
// applyEditsToNormalizedContent.
//
// Deliberate addition beyond the upstream port: upstream's "edits must
// contain at least one replacement" check lives in edit.ts's
// validateEditInput (the Task 11 edit tool), not in this engine function -
// applyEditsToNormalizedContent given a zero-length edits array falls
// through to the generic "No changes made" error instead. Task 9's pinned
// test set requires this engine to reject a zero-length edits array with
// that exact tool-level message, so this guard reproduces it verbatim here
// too, making the engine self-defending independent of which caller invokes
// it.
func ApplyEditsToNormalizedContent(content string, edits []Edit, path string) (base, updated string, err error) {
	if len(edits) == 0 {
		return "", "", errors.New("Edit tool input is invalid. edits must contain at least one replacement.")
	}

	normalizedEdits := make([]Edit, len(edits))
	for i, e := range edits {
		normalizedEdits[i] = Edit{OldText: NormalizeToLF(e.OldText), NewText: NormalizeToLF(e.NewText)}
	}

	for i, e := range normalizedEdits {
		if len(e.OldText) == 0 {
			return "", "", getEmptyOldTextError(path, i, len(normalizedEdits))
		}
	}

	usedFuzzyMatch := false
	for _, e := range normalizedEdits {
		if fuzzyFindText(content, e.OldText).usedFuzzyMatch {
			usedFuzzyMatch = true
			break
		}
	}

	replacementBaseContent := content
	if usedFuzzyMatch {
		replacementBaseContent = NormalizeForFuzzyMatch(content)
	}

	matchedEdits := make([]matchedEdit, 0, len(normalizedEdits))
	for i, e := range normalizedEdits {
		matchResult := fuzzyFindText(replacementBaseContent, e.OldText)
		if !matchResult.found {
			return "", "", getNotFoundError(path, i, len(normalizedEdits))
		}

		occurrences := countOccurrences(replacementBaseContent, e.OldText)
		if occurrences > 1 {
			return "", "", getDuplicateError(path, i, len(normalizedEdits), occurrences)
		}

		matchedEdits = append(matchedEdits, matchedEdit{
			editIndex:   i,
			matchIndex:  matchResult.index,
			matchLength: matchResult.matchLength,
			newText:     e.NewText,
		})
	}

	sort.SliceStable(matchedEdits, func(i, j int) bool { return matchedEdits[i].matchIndex < matchedEdits[j].matchIndex })
	for i := 1; i < len(matchedEdits); i++ {
		previous, current := matchedEdits[i-1], matchedEdits[i]
		if previous.matchIndex+previous.matchLength > current.matchIndex {
			return "", "", fmt.Errorf("edits[%d] and edits[%d] overlap in %s. Merge them into one edit or target disjoint regions.", previous.editIndex, current.editIndex, path)
		}
	}

	baseContent := content
	var newContent string
	if usedFuzzyMatch {
		newContent, err = applyReplacementsPreservingUnchangedLines(content, replacementBaseContent, matchedEdits)
		if err != nil {
			return "", "", err
		}
	} else {
		newContent = applyReplacements(replacementBaseContent, matchedEdits, 0)
	}

	if baseContent == newContent {
		return "", "", getNoChangeError(path, len(normalizedEdits))
	}

	return baseContent, newContent, nil
}

// --- Line-level diff (Myers' O(ND) algorithm) -------------------------
//
// Port of jsdiff 8.0.4's Diff.diff base algorithm (src/diff/base.js),
// specialized to line tokens the way jsdiff's own diffLines specializes it
// (src/diff/line.js's tokenize). Only the parts edit-diff.ts's own call
// sites exercise are ported: plain string equality (no ignoreWhitespace /
// ignoreCase / comparator options - edit-diff.ts never sets them), and
// synchronous (non-callback, non-timeout-bounded) execution.

// diffComponent is one run of consecutive lines that are either unchanged,
// added, or removed, with its concatenated line text. Ported from
// upstream's Change/Component objects as diffLines produces them.
type diffComponent struct {
	value          string
	added, removed bool
}

// diffPathComponent is one link in the reversed linked list of components
// that base.js's addToPath/extractCommon build up while searching the edit
// graph; count is the number of tokens this run covers.
type diffPathComponent struct {
	count          int
	added, removed bool
	previous       *diffPathComponent
}

// diffPathNode is a candidate path through the edit graph on some diagonal:
// oldPos is how far into oldTokens the path has advanced, last is the tip
// of its component chain. Ported from upstream's bestPath entries.
type diffPathNode struct {
	oldPos int
	last   *diffPathComponent
}

// tokenizeLines splits value into line tokens, each retaining its line
// separator ("\n" or "\r\n"); the final token has none if value doesn't end
// in one. Ported from upstream's tokenize (src/diff/line.js), specialized
// to the newlineIsToken=false, stripTrailingCr=false defaults edit-diff.ts
// always uses.
func tokenizeLines(value string) []string {
	parts := splitOnNewlines(value)
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}

	var lines []string
	for i, p := range parts {
		if i%2 == 1 {
			lines[len(lines)-1] += p
		} else {
			lines = append(lines, p)
		}
	}
	return lines
}

// splitOnNewlines mirrors JavaScript's value.split(/(\n|\r\n)/): it returns
// content segments interleaved with their separators ("\n" or "\r\n"),
// preferring to match a lone "\n" unless it's immediately preceded by "\r"
// (in which case the pair is captured together as one "\r\n" separator).
func splitOnNewlines(value string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(value); i++ {
		if value[i] != '\n' {
			continue
		}
		if i > start && value[i-1] == '\r' {
			parts = append(parts, value[start:i-1], "\r\n")
		} else {
			parts = append(parts, value[start:i], "\n")
		}
		start = i + 1
	}
	return append(parts, value[start:])
}

// computeLineDiff computes the minimal line-level diff between oldContent
// and newContent as an ordered sequence of unchanged/added/removed runs.
// Ported from upstream's Diff.diffWithOptionsObj (src/diff/base.js),
// specialized to line tokens and plain equality.
func computeLineDiff(oldContent, newContent string) []diffComponent {
	oldTokens := tokenizeLines(oldContent)
	newTokens := tokenizeLines(newContent)
	oldLen, newLen := len(oldTokens), len(newTokens)

	extractCommon := func(basePath *diffPathNode, diagonalPath int) int {
		oldPos := basePath.oldPos
		newPos := oldPos - diagonalPath
		commonCount := 0
		for newPos+1 < newLen && oldPos+1 < oldLen && oldTokens[oldPos+1] == newTokens[newPos+1] {
			newPos++
			oldPos++
			commonCount++
		}
		if commonCount > 0 {
			basePath.last = &diffPathComponent{count: commonCount, previous: basePath.last}
		}
		basePath.oldPos = oldPos
		return newPos
	}

	addToPath := func(path *diffPathNode, added, removed bool, oldPosInc int) *diffPathNode {
		last := path.last
		if last != nil && last.added == added && last.removed == removed {
			return &diffPathNode{
				oldPos: path.oldPos + oldPosInc,
				last:   &diffPathComponent{count: last.count + 1, added: added, removed: removed, previous: last.previous},
			}
		}
		return &diffPathNode{
			oldPos: path.oldPos + oldPosInc,
			last:   &diffPathComponent{count: 1, added: added, removed: removed, previous: last},
		}
	}

	bestPath := map[int]*diffPathNode{0: {oldPos: -1}}
	newPos := extractCommon(bestPath[0], 0)
	if bestPath[0].oldPos+1 >= oldLen && newPos+1 >= newLen {
		return buildDiffValues(bestPath[0].last, newTokens, oldTokens)
	}

	// -Infinity/+Infinity in upstream: once a path reaches an edge of the
	// edit graph, moves past that edge are pruned. Represented here as
	// generously large sentinels rather than true infinities.
	const sentinel = 1 << 30
	minDiagonalToConsider, maxDiagonalToConsider := -sentinel, sentinel
	maxEditLength := newLen + oldLen

	for editLength := 1; editLength <= maxEditLength; editLength++ {
		lowerBound := -editLength
		if minDiagonalToConsider > lowerBound {
			lowerBound = minDiagonalToConsider
		}
		upperBound := editLength
		if maxDiagonalToConsider < upperBound {
			upperBound = maxDiagonalToConsider
		}

		for diagonalPath := lowerBound; diagonalPath <= upperBound; diagonalPath += 2 {
			removePath := bestPath[diagonalPath-1]
			addPath := bestPath[diagonalPath+1]
			if removePath != nil {
				delete(bestPath, diagonalPath-1)
			}

			canAdd := false
			if addPath != nil {
				addPathNewPos := addPath.oldPos - diagonalPath
				canAdd = addPathNewPos >= 0 && addPathNewPos < newLen
			}
			canRemove := removePath != nil && removePath.oldPos+1 < oldLen

			if !canAdd && !canRemove {
				delete(bestPath, diagonalPath)
				continue
			}

			var basePath *diffPathNode
			if !canRemove || (canAdd && removePath.oldPos < addPath.oldPos) {
				basePath = addToPath(addPath, true, false, 0)
			} else {
				basePath = addToPath(removePath, false, true, 1)
			}

			newPos = extractCommon(basePath, diagonalPath)
			if basePath.oldPos+1 >= oldLen && newPos+1 >= newLen {
				return buildDiffValues(basePath.last, newTokens, oldTokens)
			}

			bestPath[diagonalPath] = basePath
			if basePath.oldPos+1 >= oldLen && diagonalPath-1 < maxDiagonalToConsider {
				maxDiagonalToConsider = diagonalPath - 1
			}
			if newPos+1 >= newLen && diagonalPath+1 > minDiagonalToConsider {
				minDiagonalToConsider = diagonalPath + 1
			}
		}
	}

	// Unreachable: maxEditLength bounds the true shortest edit script length
	// (at most oldLen+newLen edits), so the loop above always returns.
	panic("computeLineDiff: no edit script found within maxEditLength")
}

// buildDiffValues walks the reversed linked list of path components rooted
// at last, reverses it into file order, and materializes each component's
// concatenated line text. Ported from upstream's buildValues (with
// useLongestToken always false, matching LineDiff's default).
func buildDiffValues(last *diffPathComponent, newTokens, oldTokens []string) []diffComponent {
	var chain []*diffPathComponent
	for c := last; c != nil; c = c.previous {
		chain = append(chain, c)
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}

	components := make([]diffComponent, len(chain))
	newPos, oldPos := 0, 0
	for i, c := range chain {
		components[i].added = c.added
		components[i].removed = c.removed
		if !c.removed {
			components[i].value = strings.Join(newTokens[newPos:newPos+c.count], "")
			newPos += c.count
			if !c.added {
				oldPos += c.count
			}
		} else {
			components[i].value = strings.Join(oldTokens[oldPos:oldPos+c.count], "")
			oldPos += c.count
		}
	}
	return components
}

// --- Unified patch generation ------------------------------------------

// unifiedHunk is one @@ ... @@ block of a unified diff.
type unifiedHunk struct {
	oldStart, oldLines int
	newStart, newLines int
	lines              []string
}

// splitLinesKeepEnds splits text on "\n", re-appending "\n" to every piece;
// if text didn't end in "\n", the final piece is left without one. Ported
// from upstream's splitLines (src/patch/create.js).
func splitLinesKeepEnds(text string) []string {
	hasTrailingNL := strings.HasSuffix(text, "\n")
	parts := strings.Split(text, "\n")
	for i := range parts {
		parts[i] += "\n"
	}
	if hasTrailingNL {
		return parts[:len(parts)-1]
	}
	last := parts[len(parts)-1]
	parts[len(parts)-1] = last[:len(last)-1]
	return parts
}

// buildUnifiedHunks groups a line diff into unified-diff hunks with up to
// context lines of surrounding, unchanged content, merging hunks whose
// context would otherwise overlap. Ported from upstream's structuredPatch /
// diffLinesResultToPatch (src/patch/create.js), specialized to the single
// options shape edit-diff.ts uses (no oldHeader/newHeader, no callback).
func buildUnifiedHunks(parts []diffComponent, context int) []unifiedHunk {
	type entry struct {
		added, removed bool
		lines          []string
	}
	entries := make([]entry, 0, len(parts)+1)
	for _, c := range parts {
		entries = append(entries, entry{added: c.added, removed: c.removed, lines: splitLinesKeepEnds(c.value)})
	}
	// Append a sentinel empty entry to make hunk-closing logic below
	// uniform at the end of the diff, matching upstream's
	// diff.push({ value: "", lines: [] }).
	entries = append(entries, entry{})

	prefixLines := func(lines []string, prefix string) []string {
		out := make([]string, len(lines))
		for i, l := range lines {
			out[i] = prefix + l
		}
		return out
	}

	var hunks []unifiedHunk
	oldRangeStart, newRangeStart := 0, 0
	var curRange []string
	oldLine, newLine := 1, 1

	for i, current := range entries {
		lines := current.lines
		if current.added || current.removed {
			if oldRangeStart == 0 {
				oldRangeStart = oldLine
				newRangeStart = newLine
				if i > 0 {
					prevLines := entries[i-1].lines
					if context > 0 {
						start := len(prevLines) - context
						if start < 0 {
							start = 0
						}
						curRange = prefixLines(prevLines[start:], " ")
					} else {
						curRange = nil
					}
					oldRangeStart -= len(curRange)
					newRangeStart -= len(curRange)
				}
			}

			prefix := "-"
			if current.added {
				prefix = "+"
			}
			curRange = append(curRange, prefixLines(lines, prefix)...)

			if current.added {
				newLine += len(lines)
			} else {
				oldLine += len(lines)
			}
		} else {
			if oldRangeStart != 0 {
				if len(lines) <= context*2 && i < len(entries)-2 {
					curRange = append(curRange, prefixLines(lines, " ")...)
				} else {
					contextSize := len(lines)
					if context < contextSize {
						contextSize = context
					}
					curRange = append(curRange, prefixLines(lines[:contextSize], " ")...)
					hunks = append(hunks, unifiedHunk{
						oldStart: oldRangeStart,
						oldLines: oldLine - oldRangeStart + contextSize,
						newStart: newRangeStart,
						newLines: newLine - newRangeStart + contextSize,
						lines:    curRange,
					})
					oldRangeStart, newRangeStart = 0, 0
					curRange = nil
				}
			}
			oldLine += len(lines)
			newLine += len(lines)
		}
	}

	// Strip the trailing "\n" from every hunk line; where a line has none
	// (only possible on the very last line of the whole diff), insert a
	// "\ No newline at end of file" marker after it.
	for h := range hunks {
		lines := hunks[h].lines
		for i := 0; i < len(lines); i++ {
			if strings.HasSuffix(lines[i], "\n") {
				lines[i] = lines[i][:len(lines[i])-1]
				continue
			}
			lines = append(lines[:i+1], append([]string{"\\ No newline at end of file"}, lines[i+1:]...)...)
			i++
		}
		hunks[h].lines = lines
	}

	return hunks
}

// GenerateUnifiedPatch renders a standard unified diff of oldContent vs.
// newContent, with "--- <path>" / "+++ <path>" file headers (no index or
// underline lines) and up to contextLines of surrounding context per hunk.
// contextLines of 0 selects upstream's default of 4. Ported from upstream's
// generateUnifiedPatch, which always calls jsdiff's createTwoFilesPatch with
// headerOptions: Diff.FILE_HEADERS_ONLY and no oldHeader/newHeader.
func GenerateUnifiedPatch(path, oldContent, newContent string, contextLines int) string {
	if contextLines == 0 {
		contextLines = 4
	}
	if contextLines < 0 {
		contextLines = 0
	}

	hunks := buildUnifiedHunks(computeLineDiff(oldContent, newContent), contextLines)

	lines := make([]string, 0, 2+2*len(hunks))
	lines = append(lines, "--- "+path, "+++ "+path)
	for _, h := range hunks {
		oldStart, oldLines := h.oldStart, h.oldLines
		if oldLines == 0 {
			// Unified diff format quirk: a zero-length chunk reports the
			// line just before where it would have started.
			oldStart--
		}
		newStart, newLines := h.newStart, h.newLines
		if newLines == 0 {
			newStart--
		}
		lines = append(lines, fmt.Sprintf("@@ -%d,%d +%d,%d @@", oldStart, oldLines, newStart, newLines))
		lines = append(lines, h.lines...)
	}
	return strings.Join(lines, "\n") + "\n"
}

// --- Display-oriented diff string ---------------------------------------

// generateDiffStringContextLines is the fixed context window
// GenerateDiffString uses; upstream defaults generateDiffString's
// contextLines parameter to 4 and nothing in this codebase calls it with
// any other value, so the Go signature (fixed by the task interface) drops
// the parameter entirely.
const generateDiffStringContextLines = 4

// GenerateDiffString renders a display-oriented diff of oldContent vs.
// newContent: each changed line is prefixed with "+" or "-" and its line
// number (in the new or old file respectively); unchanged lines are shown
// as up to generateDiffStringContextLines of context around each change,
// with "..." marking elided runs. firstChangedLine is the 1-indexed line
// number (in newContent) of the first change, or 0 if oldContent and
// newContent produce no changes. Ported from upstream's generateDiffString.
func GenerateDiffString(oldContent, newContent string) (diff string, firstChangedLine int) {
	parts := computeLineDiff(oldContent, newContent)

	oldLineCount := len(strings.Split(oldContent, "\n"))
	newLineCount := len(strings.Split(newContent, "\n"))
	maxLineNum := oldLineCount
	if newLineCount > maxLineNum {
		maxLineNum = newLineCount
	}
	lineNumWidth := len(strconv.Itoa(maxLineNum))

	padNum := func(n int) string {
		s := strconv.Itoa(n)
		if pad := lineNumWidth - len(s); pad > 0 {
			s = strings.Repeat(" ", pad) + s
		}
		return s
	}
	blankPad := strings.Repeat(" ", lineNumWidth)

	var output []string
	oldLineNum, newLineNum := 1, 1
	lastWasChange := false

	for i, part := range parts {
		raw := strings.Split(part.value, "\n")
		if len(raw) > 0 && raw[len(raw)-1] == "" {
			raw = raw[:len(raw)-1]
		}

		if part.added || part.removed {
			if firstChangedLine == 0 {
				firstChangedLine = newLineNum
			}
			for _, line := range raw {
				if part.added {
					output = append(output, "+"+padNum(newLineNum)+" "+line)
					newLineNum++
				} else {
					output = append(output, "-"+padNum(oldLineNum)+" "+line)
					oldLineNum++
				}
			}
			lastWasChange = true
			continue
		}

		nextPartIsChange := i < len(parts)-1 && (parts[i+1].added || parts[i+1].removed)
		hasLeadingChange := lastWasChange
		hasTrailingChange := nextPartIsChange

		switch {
		case hasLeadingChange && hasTrailingChange:
			if len(raw) <= generateDiffStringContextLines*2 {
				for _, line := range raw {
					output = append(output, " "+padNum(oldLineNum)+" "+line)
					oldLineNum++
					newLineNum++
				}
			} else {
				leading := raw[:generateDiffStringContextLines]
				trailing := raw[len(raw)-generateDiffStringContextLines:]
				skipped := len(raw) - len(leading) - len(trailing)

				for _, line := range leading {
					output = append(output, " "+padNum(oldLineNum)+" "+line)
					oldLineNum++
					newLineNum++
				}
				output = append(output, " "+blankPad+" ...")
				oldLineNum += skipped
				newLineNum += skipped
				for _, line := range trailing {
					output = append(output, " "+padNum(oldLineNum)+" "+line)
					oldLineNum++
					newLineNum++
				}
			}
		case hasLeadingChange:
			shown := raw
			if len(shown) > generateDiffStringContextLines {
				shown = raw[:generateDiffStringContextLines]
			}
			skipped := len(raw) - len(shown)
			for _, line := range shown {
				output = append(output, " "+padNum(oldLineNum)+" "+line)
				oldLineNum++
				newLineNum++
			}
			if skipped > 0 {
				output = append(output, " "+blankPad+" ...")
				oldLineNum += skipped
				newLineNum += skipped
			}
		case hasTrailingChange:
			skipped := len(raw) - generateDiffStringContextLines
			if skipped < 0 {
				skipped = 0
			}
			if skipped > 0 {
				output = append(output, " "+blankPad+" ...")
				oldLineNum += skipped
				newLineNum += skipped
			}
			for _, line := range raw[skipped:] {
				output = append(output, " "+padNum(oldLineNum)+" "+line)
				oldLineNum++
				newLineNum++
			}
		default:
			oldLineNum += len(raw)
			newLineNum += len(raw)
		}
		lastWasChange = false
	}

	return strings.Join(output, "\n"), firstChangedLine
}
