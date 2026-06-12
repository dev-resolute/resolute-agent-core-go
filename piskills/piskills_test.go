package piskills

import (
	"os"
	"path/filepath"
	"testing"

	pi "github.com/resolute-sh/pi-core-agent-go"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func findSkill(skills []pi.Skill, name string) (pi.Skill, bool) {
	for _, s := range skills {
		if s.Name == name {
			return s, true
		}
	}
	return pi.Skill{}, false
}

func diagnosticFor(diags []Diagnostic, pathSuffix string) (Diagnostic, bool) {
	for _, d := range diags {
		if filepath.Base(filepath.Dir(d.Path)) == pathSuffix || filepath.Base(d.Path) == pathSuffix {
			return d, true
		}
	}
	return Diagnostic{}, false
}

func TestLoad(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, root string)
		check func(t *testing.T, root string, skills []pi.Skill, diags []Diagnostic, err error)
	}{
		{
			name: "well-formed skill loads with frontmatter",
			setup: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "research", "SKILL.md"),
					"---\nname: research\ndescription: Helps with research tasks\n---\n# Research\nbody here")
			},
			check: func(t *testing.T, root string, skills []pi.Skill, diags []Diagnostic, err error) {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if len(diags) != 0 {
					t.Errorf("diagnostics = %v, want none", diags)
				}
				s, ok := findSkill(skills, "research")
				if !ok {
					t.Fatalf("skill %q not loaded; got %v", "research", skills)
				}
				if s.Description != "Helps with research tasks" {
					t.Errorf("Description = %q", s.Description)
				}
				if s.Content != "# Research\nbody here" {
					t.Errorf("Content = %q", s.Content)
				}
				if s.FilePath != filepath.Join(root, "research", "SKILL.md") {
					t.Errorf("FilePath = %q", s.FilePath)
				}
				if s.DisableModelInvocation {
					t.Errorf("DisableModelInvocation = true, want false")
				}
			},
		},
		{
			name: "disable-model-invocation frontmatter parsed",
			setup: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "secret", "SKILL.md"),
					"---\nname: secret\ndescription: a secret skill\ndisable-model-invocation: true\n---\nbody")
			},
			check: func(t *testing.T, root string, skills []pi.Skill, diags []Diagnostic, err error) {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				s, ok := findSkill(skills, "secret")
				if !ok {
					t.Fatalf("skill %q not loaded; got %v", "secret", skills)
				}
				if !s.DisableModelInvocation {
					t.Errorf("DisableModelInvocation = false, want true")
				}
			},
		},
		{
			name: "name defaults to parent directory when omitted",
			setup: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "summarize", "SKILL.md"),
					"---\ndescription: summarizes text\n---\nbody")
			},
			check: func(t *testing.T, root string, skills []pi.Skill, diags []Diagnostic, err error) {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if _, ok := findSkill(skills, "summarize"); !ok {
					t.Fatalf("skill name did not default to parent dir; got %v", skills)
				}
			},
		},
		{
			name: "malformed skill without description yields diagnostic and is skipped",
			setup: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "broken", "SKILL.md"), "---\nname: broken\n---\nbody only")
			},
			check: func(t *testing.T, root string, skills []pi.Skill, diags []Diagnostic, err error) {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if _, ok := findSkill(skills, "broken"); ok {
					t.Errorf("malformed skill should not be loaded; got %v", skills)
				}
				if _, ok := diagnosticFor(diags, "broken"); !ok {
					t.Errorf("expected a diagnostic for the malformed skill; got %v", diags)
				}
			},
		},
		{
			name: "skill ignored via .gitignore is skipped",
			setup: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, ".gitignore"), "ignored/\n")
				writeFile(t, filepath.Join(root, "ignored", "SKILL.md"),
					"---\nname: ignored\ndescription: should be skipped\n---\nbody")
				writeFile(t, filepath.Join(root, "kept", "SKILL.md"),
					"---\nname: kept\ndescription: should be kept\n---\nbody")
			},
			check: func(t *testing.T, root string, skills []pi.Skill, diags []Diagnostic, err error) {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if _, ok := findSkill(skills, "ignored"); ok {
					t.Errorf("gitignored skill should be skipped; got %v", skills)
				}
				if _, ok := findSkill(skills, "kept"); !ok {
					t.Errorf("non-ignored skill should be loaded; got %v", skills)
				}
			},
		},
		{
			name: "skill ignored via .ignore is skipped",
			setup: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, ".ignore"), "draft\n")
				writeFile(t, filepath.Join(root, "draft", "SKILL.md"),
					"---\nname: draft\ndescription: skip me\n---\nbody")
			},
			check: func(t *testing.T, root string, skills []pi.Skill, diags []Diagnostic, err error) {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if _, ok := findSkill(skills, "draft"); ok {
					t.Errorf(".ignore'd skill should be skipped; got %v", skills)
				}
			},
		},
		{
			name: "quoted \"true\" is string not bool — does not disable model invocation",
			setup: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "quoted-bool", "SKILL.md"),
					"---\nname: quoted-bool\ndescription: a skill\ndisable-model-invocation: \"true\"\n---\nbody")
			},
			check: func(t *testing.T, root string, skills []pi.Skill, diags []Diagnostic, err error) {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if len(diags) != 0 {
					t.Errorf("unexpected diagnostics: %v", diags)
				}
				s, ok := findSkill(skills, "quoted-bool")
				if !ok {
					t.Fatalf("skill not loaded; got %v", skills)
				}
				if s.DisableModelInvocation {
					t.Errorf(`quoted "true" should not disable model invocation`)
				}
			},
		},
		{
			name: "unquoted false does not disable model invocation",
			setup: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "explicit-false", "SKILL.md"),
					"---\nname: explicit-false\ndescription: a skill\ndisable-model-invocation: false\n---\nbody")
			},
			check: func(t *testing.T, root string, skills []pi.Skill, diags []Diagnostic, err error) {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				s, ok := findSkill(skills, "explicit-false")
				if !ok {
					t.Fatalf("skill not loaded; got %v", skills)
				}
				if s.DisableModelInvocation {
					t.Errorf("unquoted false should not disable model invocation")
				}
			},
		},
		{
			name:  "missing directory returns empty without error",
			setup: func(t *testing.T, root string) {},
			check: func(t *testing.T, root string, skills []pi.Skill, diags []Diagnostic, err error) {
				if err != nil {
					t.Fatalf("Load on missing dir returned error: %v", err)
				}
				if len(skills) != 0 || len(diags) != 0 {
					t.Errorf("missing dir: skills=%v diags=%v, want both empty", skills, diags)
				}
			},
		},
		{
			name: "BOM-prefixed SKILL.md loads successfully",
			setup: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "bom-skill", "SKILL.md"),
					"\uFEFF---\nname: bom-skill\ndescription: skill with BOM\n---\nbody here")
			},
			check: func(t *testing.T, root string, skills []pi.Skill, diags []Diagnostic, err error) {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if len(diags) != 0 {
					t.Errorf("diagnostics = %v, want none", diags)
				}
				s, ok := findSkill(skills, "bom-skill")
				if !ok {
					t.Fatalf("BOM-prefixed skill not loaded; got %v", skills)
				}
				if s.Description != "skill with BOM" {
					t.Errorf("Description = %q, want %q", s.Description, "skill with BOM")
				}
			},
		},
		{
			name: "skill directory is a leaf and nested skills are not recursed",
			setup: func(t *testing.T, root string) {
				writeFile(t, filepath.Join(root, "outer", "SKILL.md"),
					"---\nname: outer\ndescription: outer skill\n---\nbody")
				writeFile(t, filepath.Join(root, "outer", "inner", "SKILL.md"),
					"---\nname: inner\ndescription: inner skill\n---\nbody")
			},
			check: func(t *testing.T, root string, skills []pi.Skill, diags []Diagnostic, err error) {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if _, ok := findSkill(skills, "outer"); !ok {
					t.Errorf("outer skill should be loaded; got %v", skills)
				}
				if _, ok := findSkill(skills, "inner"); ok {
					t.Errorf("nested skill under a skill dir should not be discovered; got %v", skills)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			dir := filepath.Join(root, "skills")
			tt.setup(t, dir)
			skills, diags, err := Load(dir)
			tt.check(t, dir, skills, diags, err)
		})
	}
}
