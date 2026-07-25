package tools

import (
	"context"
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// path.go ports packages/agent/src/harness/tools/path-utils.ts from upstream
// pi @0.82.0: tool-facing path normalization, plus a "healing" resolution
// pass for the read tool that tries a handful of Unicode look-alike
// spellings before giving up. The healing pass exists because an LLM
// composing a path from what it "read" (e.g. from a directory listing, or
// from its own training data's notion of typography) commonly produces a
// plain-ASCII spelling that differs from what's actually on disk only in
// which Unicode code point was used for a space, an apostrophe, or how an
// accented character is normalized.
//
// Unicode code points below are spelled with explicit \u escapes rather
// than embedded literally, since several of them (spaces, in particular)
// are visually indistinguishable from each other and from ASCII in a
// rendered diff.

// unicodeSpaces matches the Unicode space characters NormalizeToolPath
// collapses to a plain ASCII space: no-break space (U+00A0), the general
// punctuation space block (U+2000-U+200A), narrow no-break space (U+202F),
// medium mathematical space (U+205F), and ideographic space (U+3000).
// Verbatim port of upstream's UNICODE_SPACES.
var unicodeSpaces = regexp.MustCompile("[\u00A0\u2000-\u200A\u202F\u205F\u3000]")

// narrowNoBreakSpace is U+202F - the character macOS (Finder, Spotlight,
// screenshot filenames) inserts before "AM"/"PM" in timestamped filenames,
// which an LLM will typically render as a plain ASCII space instead. Ported
// from upstream's NARROW_NO_BREAK_SPACE.
const narrowNoBreakSpace = "\u202F"

// curlyRightSingleQuote is U+2019, the typographic apostrophe some
// filesystems/exports use in place of the ASCII "'" (U+0027) - e.g. a file
// named after a person's name with a "proper" apostrophe.
const curlyRightSingleQuote = "\u2019"

// amPmSuffix matches an ASCII space immediately before "AM." or "PM."
// (case-insensitive) so ResolveReadToolPath can try swapping it for
// narrowNoBreakSpace - the healing counterpart to NormalizeToolPath
// collapsing that same narrow no-break space back down to an ASCII space.
// Verbatim port of upstream's / (AM|PM)\./gi.
var amPmSuffix = regexp.MustCompile(`(?i) (AM|PM)\.`)

// NormalizeToolPath collapses Unicode space variants (see unicodeSpaces) to
// a plain ASCII space, and strips a single leading "@" (a mention-style
// prefix some clients prepend to pasted file paths). Ported from upstream's
// normalizeToolPath.
func NormalizeToolPath(path string) string {
	normalized := unicodeSpaces.ReplaceAllString(path, " ")
	return strings.TrimPrefix(normalized, "@")
}

// ResolveToolPath normalizes path (via NormalizeToolPath) and resolves it to
// an absolute path via env.AbsolutePath. Ported from upstream's
// resolveToolPath.
func ResolveToolPath(ctx context.Context, env ExecutionEnv, path string) (string, error) {
	return env.AbsolutePath(ctx, NormalizeToolPath(path))
}

// ResolveReadToolPath resolves path like ResolveToolPath, then - if that
// exact resolved path doesn't exist - tries a fixed, ordered set of Unicode
// "healing" variants and returns the first one that exists on disk:
//
//  1. the resolved path itself
//  2. narrowNoBreakSpace substituted for the ASCII space before "AM."/"PM."
//  3. NFD (canonical decomposition) Unicode normalization
//  4. curlyRightSingleQuote in place of every ASCII apostrophe
//  5. both (3) and (4) combined
//
// Duplicate variants (e.g. when none of the substitutions apply) are only
// checked once. If none of the variants exist, the plain resolved path is
// returned unchanged - ResolveReadToolPath never itself decides a path
// doesn't exist; that's left to the caller's subsequent read. Ported from
// upstream's resolveReadToolPath.
func ResolveReadToolPath(ctx context.Context, env ExecutionEnv, path string) (string, error) {
	resolved, err := ResolveToolPath(ctx, env, path)
	if err != nil {
		return "", err
	}

	nfd := norm.NFD.String(resolved)
	variants := []string{
		resolved,
		amPmSuffix.ReplaceAllString(resolved, narrowNoBreakSpace+"$1."),
		nfd,
		strings.ReplaceAll(resolved, "'", curlyRightSingleQuote),
		strings.ReplaceAll(nfd, "'", curlyRightSingleQuote),
	}

	seen := make(map[string]bool, len(variants))
	for _, variant := range variants {
		if seen[variant] {
			continue
		}
		seen[variant] = true
		exists, err := env.Exists(ctx, variant)
		if err != nil {
			return "", err
		}
		if exists {
			return variant, nil
		}
	}
	return resolved, nil
}
