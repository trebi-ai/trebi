// Package agents embeds the first-class agent templates. One catalog serves
// the API, CLI, MCP, and UI onboarding chips.
package agents

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/trebi-ai/trebi/internal/job"
	"gopkg.in/yaml.v3"
)

//go:embed templates
var embedded embed.FS

// Template is the template.yaml wire shape.
type Template struct {
	ID          string          `json:"id" yaml:"id"`
	Title       string          `json:"title" yaml:"title"`
	Description string          `json:"description" yaml:"description"`
	Icon        string          `json:"icon" yaml:"icon"`
	Inputs      []TemplateInput `json:"inputs,omitempty" yaml:"inputs,omitempty"`
	Jobs        []string        `json:"jobs" yaml:"jobs"`
}

// TemplateInput is one user-supplied template value.
type TemplateInput struct {
	Key      string `json:"key" yaml:"key"`
	Label    string `json:"label" yaml:"label"`
	Required bool   `json:"required" yaml:"required"`
}

// File is one rendered file relative to agents/<id>/.
type File struct {
	Rel     string
	Content []byte
}

// Catalog reads the embedded template files.
type Catalog struct{ fs fs.FS }

// NewCatalog returns the embedded catalog.
func NewCatalog() *Catalog { return &Catalog{fs: embedded} }

// List returns templates sorted by id.
func (c *Catalog) List() []Template {
	entries, err := fs.ReadDir(c.fs, "templates")
	if err != nil {
		return []Template{}
	}
	out := []Template{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if template, ok := c.Get(entry.Name()); ok {
			out = append(out, template)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get reads one template manifest.
func (c *Catalog) Get(id string) (Template, bool) {
	data, err := fs.ReadFile(c.fs, path.Join("templates", id, "template.yaml"))
	if err != nil {
		return Template{}, false
	}
	var t Template
	if err := yaml.Unmarshal(data, &t); err != nil {
		return Template{}, false
	}
	if t.ID == "" {
		t.ID = id
	}
	if t.Jobs == nil {
		t.Jobs = []string{}
	}
	return t, true
}

// Render materializes AGENT.md, MEMORY.md, and disabled starter jobs.
type RenderOpts struct {
	Template string
	AgentID  string
	Title    string
	Accept   []string
	Inputs   map[string]string
}

func (c *Catalog) Render(o RenderOpts) ([]File, error) {
	id := o.Template
	if _, ok := c.Get(id); !ok {
		return nil, fmt.Errorf("unknown template %q", id)
	}
	var files []File
	err := fs.WalkDir(c.fs, path.Join("templates", id), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(c.fs, p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, path.Join("templates", id)+"/")
		files = append(files, File{Rel: filepath.ToSlash(rel), Content: renderContent(string(data), o.Title, o.Accept, o.Inputs)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := range files {
		kind := ""
		switch {
		case strings.HasSuffix(files[i].Rel, "/JOB.md") || files[i].Rel == "JOB.md":
			files[i].Content = forceDisabled(files[i].Content)
			kind = "job"
		case files[i].Rel == "AGENT.md":
			kind = "agent"
		}
		if kind != "" {
			out, err := job.WithID(files[i].Content, job.NewID(kind))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", files[i].Rel, err)
			}
			files[i].Content = out
		}
	}
	return files, nil
}

func renderContent(text, title string, accept []string, inputs map[string]string) []byte {
	display := title
	if display == "" {
		display = "Agent"
	}
	text = strings.ReplaceAll(text, "{{agent_title}}", display)
	for key, value := range inputs {
		text = strings.ReplaceAll(text, "{{input."+key+"}}", value)
	}
	if idx := strings.Index(text, "email:\n"); idx >= 0 {
		// Replace through the next top-level key or frontmatter terminator.
		if end := nextTopLevel(text, idx+len("email:\n")); end >= 0 {
			text = text[:idx] + acceptYAML(accept) + "\n" + text[end:]
		}
	}
	return []byte(text)
}

func acceptYAML(accept []string) string {
	if len(accept) == 0 {
		return "email:\n  accept: [] # admins always pass; add more senders here"
	}
	var b strings.Builder
	b.WriteString("email:\n  accept:\n")
	for _, entry := range accept {
		fmt.Fprintf(&b, "    - %q\n", entry)
	}
	return strings.TrimRight(b.String(), "\n")
}

func nextTopLevel(text string, from int) int {
	for i := from; i < len(text); i++ {
		lineStart := i == 0 || text[i-1] == '\n'
		if lineStart && (text[i] == '-' || (text[i] >= 'a' && text[i] <= 'z')) {
			return i
		}
	}
	return -1
}

func forceDisabled(data []byte) []byte {
	text := string(data)
	if strings.HasPrefix(text, "---\n") {
		if end := strings.Index(text[4:], "\n---"); end >= 0 {
			fm := text[4 : 4+end]
			if strings.Contains(fm, "\ndisabled:") || strings.HasPrefix(fm, "disabled:") {
				lines := strings.Split(fm, "\n")
				for i, line := range lines {
					if strings.HasPrefix(line, "disabled:") {
						lines[i] = "disabled: true"
					}
				}
				return []byte("---\n" + strings.Join(lines, "\n") + text[4+end:])
			}
			return []byte("---\ndisabled: true\n" + fm + text[4+end:])
		}
	}
	return []byte("---\ndisabled: true\n---\n\n" + text)
}
