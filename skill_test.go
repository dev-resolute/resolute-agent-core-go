package pi

import "testing"

func TestFormatSkillsForSystemPrompt(t *testing.T) {
	t.Parallel()

	visible := Skill{
		Name:        "visible",
		Description: "Use <this> & that",
		Content:     "visible content",
		FilePath:    "/skills/visible/SKILL.md",
	}
	second := Skill{
		Name:        "second",
		Description: "Second skill",
		Content:     "second content",
		FilePath:    "/skills/second/SKILL.md",
	}
	disabled := Skill{
		Name:                   "hidden",
		Description:            "Hidden",
		Content:                "hidden content",
		FilePath:               "/skills/hidden/SKILL.md",
		DisableModelInvocation: true,
	}
	escaping := Skill{
		Name:        "a&b",
		Description: `Quote "double" and 'single'`,
		Content:     "content",
		FilePath:    `/skills/<bad>&"quote"/SKILL.md`,
	}

	const preamble = "The following skills provide specialized instructions for specific tasks.\n" +
		"Read the full skill file when the task matches its description.\n" +
		"When a skill file references a relative path, resolve it against the skill directory (parent of SKILL.md / dirname of the path) and use that absolute path in tool commands.\n\n"

	tests := []struct {
		name   string
		skills []Skill
		want   string
	}{
		{
			name:   "empty input",
			skills: nil,
			want:   "",
		},
		{
			name:   "only disabled skills",
			skills: []Skill{disabled},
			want:   "",
		},
		{
			name:   "mixed visibility preserves order and skips disabled",
			skills: []Skill{visible, disabled, second},
			want: preamble + "<available_skills>\n" +
				"  <skill>\n" +
				"    <name>visible</name>\n" +
				"    <description>Use &lt;this&gt; &amp; that</description>\n" +
				"    <location>/skills/visible/SKILL.md</location>\n" +
				"  </skill>\n" +
				"  <skill>\n" +
				"    <name>second</name>\n" +
				"    <description>Second skill</description>\n" +
				"    <location>/skills/second/SKILL.md</location>\n" +
				"  </skill>\n" +
				"</available_skills>",
		},
		{
			name:   "escapes XML in all fields",
			skills: []Skill{escaping},
			want: preamble + "<available_skills>\n" +
				"  <skill>\n" +
				"    <name>a&amp;b</name>\n" +
				"    <description>Quote &quot;double&quot; and &apos;single&apos;</description>\n" +
				"    <location>/skills/&lt;bad&gt;&amp;&quot;quote&quot;/SKILL.md</location>\n" +
				"  </skill>\n" +
				"</available_skills>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := formatSkillsForSystemPrompt(tt.skills)
			if got != tt.want {
				t.Errorf("formatSkillsForSystemPrompt() =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}
