package tools

import (
	"embed"
	"regexp"
	"strings"
)

//go:embed mcpdocs/*.md
var docsFS embed.FS

// Instructions is the front door for every catalog client: MCP initialize
// text and the native assistant system prompt both reuse this constant.
const Instructions = `Trebi is an LLM task runner (daemon + /v1 API). Call each tool as {"action": "<name>", "input": {…}}.

Call docs on a tool before its first use: docs carries the field signature of every action and is authoritative.

Three rules:
1. Validate after you write a JOB.md (job.validate).
2. Dry-run before the first launch of a job you wrote or edited (job.run with dry_run).
3. Ask the user before a destructive action. Such an action answers "validation" with details.confirm_required and a message that names what it deletes; repeat the call with confirm: true once the user agrees.
4. A job, agent, or assistant with memory keeps a structured MEMORY.md with three sections: Context, Lessons, Log. Read it with memory_show before you act, and append Log entries with memory_add as you work. Keep records in the state tool, not in memory.

Call system.guide with topic mcp-basics before your first workflow, and system.guides for the index. Always pass workspace explicitly on workspace-scoped calls.`

// ReadDoc returns the embedded markdown for one tool.
func ReadDoc(tool string) ([]byte, error) {
	return docsFS.ReadFile("mcpdocs/" + tool + ".md")
}

// Docs returns the field reference for one tool: a generated signature
// block, then the embedded prose.
func Docs(tool string) (string, error) {
	doc, err := ReadDoc(tool)
	if err != nil {
		return "", err
	}
	return DocSignatures(string(doc)) + string(doc), nil
}

// DocSignatures renders one line per documented action, so a client reads the
// whole call shape of a tool at a glance. A ? marks an optional field.
func DocSignatures(doc string) string {
	var b strings.Builder
	b.WriteString("## Signatures\n\n```\n")
	for _, action := range DocActions(doc) {
		names := make([]string, 0, 8)
		for _, f := range DocFields(DocSection(doc, action)) {
			if f.Required {
				names = append(names, f.Name)
				continue
			}
			names = append(names, f.Name+"?")
		}
		b.WriteString(action + "(" + strings.Join(names, ", ") + ")\n")
	}
	b.WriteString("docs()\n```\n\n")
	return b.String()
}

// DocActions lists the action names of one tool document, in document order.
func DocActions(doc string) []string {
	var out []string
	for _, line := range strings.Split(doc, "\n") {
		if name, ok := strings.CutPrefix(line, "### "); ok {
			out = append(out, strings.TrimSpace(name))
		}
	}
	return out
}

// DocField is one input field of one documented action.
type DocField struct {
	Name     string
	Type     string
	Required bool
}

var docFieldRE = regexp.MustCompile(`^- ` + "`" + `([a-z0-9_]+)` + "`" + ` \(([\w ]+?), (required|optional)([^)]*)\)`) //nolint:gochecknoglobals // compiled pattern

// DocSection returns the body of one "### <action>" section.
func DocSection(doc, action string) string {
	_, rest, ok := strings.Cut(doc, "### "+action+"\n")
	if !ok {
		return ""
	}
	if next := strings.Index(rest, "\n### "); next >= 0 {
		return rest[:next]
	}
	return rest
}

// DocFields parses the field list of one action section. A conditional
// "required for owner job" is not a bare requirement, so it reads as optional.
func DocFields(section string) []DocField {
	var out []DocField
	for _, line := range strings.Split(section, "\n") {
		// An indented bullet is a field inside an array item, not an input key.
		m := docFieldRE.FindStringSubmatch(strings.TrimRight(line, " \r"))
		if m == nil {
			continue
		}
		out = append(out, DocField{Name: m[1], Type: m[2], Required: m[3] == "required" && m[4] == ""})
	}
	return out
}
