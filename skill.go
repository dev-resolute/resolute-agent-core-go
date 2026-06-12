package pi

import (
	"strings"

	"github.com/resolute-sh/pi-llm-go"
)

// Skill is a unit of model-invokable expertise carried on the Agent. It is part
// of the mutable runtime config and the turn snapshot; the model-visible index
// (name, description, location) is auto-rendered into the system prompt at
// turn-snapshot time by formatSkillsForSystemPrompt.
//
// Content-reader contract: the framework does NOT ship a tool that reads
// FilePath. The rendered index exposes only the skill's name, description, and
// FilePath — never Content — so the model fetches a skill's full instructions on
// demand through a user-supplied tool (e.g. a file-reader tool registered on the
// Agent) that resolves FilePath. Carrying Content here is purely informational
// for the host application; populating it does not make the framework deliver it
// to the model.
type Skill struct {
	// Name is the model-visible identifier rendered into the skill index.
	// When omitted from the SKILL.md frontmatter it defaults to the parent
	// directory name.
	Name string
	// Description is the one-line summary shown to the model in the skill
	// index. It is required; a skill without one is rejected with a Diagnostic.
	Description string
	// Content holds the full text of the SKILL.md body. The framework does
	// not deliver this to the model automatically; the host application must
	// supply a tool that resolves FilePath so the model can fetch it on demand.
	Content string
	// FilePath is the absolute path to the SKILL.md file. It is included in
	// the model-visible index so the model can request the file via a
	// host-supplied file-reader tool.
	FilePath string
	// DisableModelInvocation excludes the skill from the model-visible index
	// when true. The skill remains accessible to the host application.
	DisableModelInvocation bool
}

// formatSkillsForSystemPrompt builds the model-visible XML index of skills:
// name, description, and location (FilePath) — never Content. Skills with
// DisableModelInvocation set are excluded. It returns the empty string when no
// skill is model-visible, so callers can join it without producing blank
// sections. All field values are XML-escaped.
func formatSkillsForSystemPrompt(skills []Skill) string {
	var b strings.Builder
	visible := 0
	for _, s := range skills {
		if s.DisableModelInvocation {
			continue
		}
		if visible == 0 {
			b.WriteString("The following skills provide specialized instructions for specific tasks.\n")
			b.WriteString("Read the full skill file when the task matches its description.\n")
			b.WriteString("When a skill file references a relative path, resolve it against the skill directory (parent of SKILL.md / dirname of the path) and use that absolute path in tool commands.\n\n")
			b.WriteString("<available_skills>")
		}
		visible++
		b.WriteString("\n  <skill>")
		b.WriteString("\n    <name>" + escapeXML(s.Name) + "</name>")
		b.WriteString("\n    <description>" + escapeXML(s.Description) + "</description>")
		b.WriteString("\n    <location>" + escapeXML(s.FilePath) + "</location>")
		b.WriteString("\n  </skill>")
	}
	if visible == 0 {
		return ""
	}
	b.WriteString("\n</available_skills>")
	return b.String()
}

func escapeXML(value string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(value)
}

// applySkillsToSystemPrompt renders the model-visible skill index and appends it
// to the effective system prompt of a per-turn LLM message slice. It mutates the
// derived turn messages only — never the persisted transcript — so the index is
// recomputed each turn and reflects SetSkills hot-reloads without leaking into
// session storage. When no system message is present, it prepends one carrying
// the index. With no model-visible skills it returns msgs unchanged.
func applySkillsToSystemPrompt(msgs []llm.Message, skills []Skill) []llm.Message {
	index := formatSkillsForSystemPrompt(skills)
	if index == "" {
		return msgs
	}
	for i, m := range msgs {
		if m.Role != "system" {
			continue
		}
		text, ok := m.Content.(llm.TextContent)
		if !ok {
			continue
		}
		if text.Text == "" {
			msgs[i].Content = llm.TextContent{Text: index}
		} else {
			msgs[i].Content = llm.TextContent{Text: text.Text + "\n\n" + index}
		}
		return msgs
	}
	return append([]llm.Message{{Role: "system", Content: llm.TextContent{Text: index}}}, msgs...)
}
