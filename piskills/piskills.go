// Package piskills loads pi.Skill values from a directory tree of SKILL.md
// files. It is opt-in: the core agent package does not import it, so importing
// the core pulls in no filesystem or skill-loading code. Loading reads the
// filesystem directly via the os package.
package piskills

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	gitignore "github.com/sabhiram/go-gitignore"

	pi "github.com/resolute-sh/pi-core-agent-go"
)

const skillFileName = "SKILL.md"

var ignoreFileNames = []string{".gitignore", ".ignore"}

// Diagnostic is a non-fatal warning produced while loading skills, such as a
// SKILL.md that could not be read or that is missing a required field. Loading
// continues past a diagnostic; the offending skill is simply skipped.
type Diagnostic struct {
	// Path is the SKILL.md (or ignore/skills directory) the warning concerns.
	Path string
	// Message describes what was wrong.
	Message string
}

// Load walks dir recursively for SKILL.md files, parses their YAML-style
// frontmatter (name, description, disable-model-invocation), and honors
// .gitignore/.ignore files encountered along the way. A directory containing a
// SKILL.md is treated as a skill leaf and is not descended into further.
//
// It returns the loaded skills together with diagnostics for malformed or
// unreadable entries. A missing dir is not an error: it yields no skills and no
// diagnostics. The error return is reserved for a dir that exists but cannot be
// inspected at all.
func Load(dir string) ([]pi.Skill, []Diagnostic, error) {
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("stat skills dir %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, nil, nil
	}

	var skills []pi.Skill
	var diags []Diagnostic
	loadDir(dir, dir, nil, &skills, &diags)
	return skills, diags, nil
}

func loadDir(dir, root string, inherited []string, skills *[]pi.Skill, diags *[]Diagnostic) {
	patterns := append(slices.Clone(inherited), readIgnoreRules(dir, root)...)
	matcher := gitignore.CompileIgnoreLines(patterns...)

	entries, err := os.ReadDir(dir)
	if err != nil {
		*diags = append(*diags, Diagnostic{Path: dir, Message: fmt.Sprintf("list directory: %v", err)})
		return
	}

	for _, e := range entries {
		if e.IsDir() || e.Name() != skillFileName {
			continue
		}
		full := filepath.Join(dir, skillFileName)
		if matcher.MatchesPath(relSlash(root, full)) {
			return
		}
		skill, ds := loadSkillFile(full)
		*diags = append(*diags, ds...)
		if skill != nil {
			*skills = append(*skills, *skill)
		}
		return
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") || name == "node_modules" {
			continue
		}
		full := filepath.Join(dir, name)
		if matcher.MatchesPath(relSlash(root, full) + "/") {
			continue
		}
		loadDir(full, root, patterns, skills, diags)
	}
}

func readIgnoreRules(dir, root string) []string {
	prefix := ""
	if rel := relSlash(root, dir); rel != "" {
		prefix = rel + "/"
	}

	var out []string
	for _, name := range ignoreFileNames {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		content := strings.ReplaceAll(string(data), "\r\n", "\n")
		for _, line := range strings.Split(content, "\n") {
			if pattern, ok := prefixIgnorePattern(line, prefix); ok {
				out = append(out, pattern)
			}
		}
	}
	return out
}

func prefixIgnorePattern(line, prefix string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "", false
	}
	if strings.HasPrefix(trimmed, "#") {
		return "", false
	}

	pattern := trimmed
	negated := false
	if strings.HasPrefix(pattern, "!") {
		negated = true
		pattern = pattern[1:]
	} else if strings.HasPrefix(pattern, `\!`) {
		pattern = pattern[1:]
	}
	pattern = strings.TrimPrefix(pattern, "/")
	if prefix != "" {
		pattern = prefix + pattern
	}
	if negated {
		pattern = "!" + pattern
	}
	return pattern, true
}

func loadSkillFile(path string) (*pi.Skill, []Diagnostic) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, []Diagnostic{{Path: path, Message: fmt.Sprintf("read failed: %v", err)}}
	}

	frontmatter, body := parseFrontmatter(string(data))
	if frontmatter["description"] == "" {
		return nil, []Diagnostic{{Path: path, Message: "description is required"}}
	}

	name := frontmatter["name"]
	if name == "" {
		name = filepath.Base(filepath.Dir(path))
	}

	return &pi.Skill{
		Name:                   name,
		Description:            frontmatter["description"],
		Content:                body,
		FilePath:               path,
		DisableModelInvocation: strings.EqualFold(frontmatter["disable-model-invocation"], "true"),
	}, nil
}

func parseFrontmatter(content string) (map[string]string, string) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")

	frontmatter := map[string]string{}
	if !strings.HasPrefix(normalized, "---") {
		return frontmatter, strings.TrimSpace(normalized)
	}
	end := strings.Index(normalized[3:], "\n---")
	if end == -1 {
		return frontmatter, strings.TrimSpace(normalized)
	}
	end += 3

	block := normalized[4:end]
	body := strings.TrimSpace(normalized[end+4:])
	for _, line := range strings.Split(block, "\n") {
		if key, value, ok := parseFrontmatterLine(line); ok {
			frontmatter[key] = value
		}
	}
	return frontmatter, body
}

func parseFrontmatterLine(line string) (key, value string, ok bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}
	idx := strings.Index(trimmed, ":")
	if idx <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(trimmed[:idx])
	value = unquote(strings.TrimSpace(trimmed[idx+1:]))
	return key, value, true
}

func unquote(s string) string {
	if len(s) >= 2 {
		first, last := s[0], s[len(s)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func relSlash(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	if rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}
