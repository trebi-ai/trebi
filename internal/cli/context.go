package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/trebi-ai/trebi/internal/home"
	"gopkg.in/yaml.v3"
)

// StickyContext is the active workspace for CLI calls (~/.trebi/context).
// When URL is set, the workspace lives on a remote trebid and all API
// traffic goes there — no separate --remote flag.
type StickyContext struct {
	WorkspaceID string `yaml:"workspace_id" json:"workspace_id"`
	Name        string `yaml:"name,omitempty" json:"name,omitempty"`
	Path        string `yaml:"path,omitempty" json:"path,omitempty"`
	URL         string `yaml:"url,omitempty" json:"url,omitempty"`     // empty = local daemon
	Token       string `yaml:"token,omitempty" json:"token,omitempty"` // with URL only
}

// LinkedWorkspace is a client-side pointer to a workspace on a remote daemon.
type LinkedWorkspace struct {
	ID    string `yaml:"id"`
	Name  string `yaml:"name,omitempty"`
	Path  string `yaml:"path,omitempty"`
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
}

type linkedFile struct {
	Workspaces []LinkedWorkspace `yaml:"workspaces"`
}

// IsRemote reports a sticky context that targets a non-local daemon.
func (s *StickyContext) IsRemote() bool {
	return s != nil && strings.TrimSpace(s.URL) != ""
}

// Save writes ~/.trebi/context (0600).
func (s *StickyContext) Save() error {
	if s == nil {
		return fmt.Errorf("no sticky context")
	}
	if err := home.EnsureDirs(); err != nil {
		return err
	}
	cp := *s
	cp.URL = strings.TrimRight(strings.TrimSpace(cp.URL), "/")
	data, err := yaml.Marshal(&cp)
	if err != nil {
		return err
	}
	return os.WriteFile(home.ContextPath(), data, 0o600)
}

// LoadStickyContext reads ~/.trebi/context (nil if missing).
func LoadStickyContext() (*StickyContext, error) {
	data, err := os.ReadFile(home.ContextPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var s StickyContext
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse context: %w", err)
	}
	if s.WorkspaceID == "" && s.URL == "" {
		return nil, nil
	}
	return &s, nil
}

// ClearStickyContext removes the sticky context file.
func ClearStickyContext() error {
	err := os.Remove(home.ContextPath())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// LoadLinkedWorkspaces returns client-side remote workspace links.
func LoadLinkedWorkspaces() ([]LinkedWorkspace, error) {
	data, err := os.ReadFile(home.LinkedWorkspacesPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var doc linkedFile
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse linked-workspaces.yaml: %w", err)
	}
	return doc.Workspaces, nil
}

// FindLinked matches by id or name (case-sensitive id, case-insensitive name).
func FindLinked(idOrName string) (LinkedWorkspace, bool) {
	idOrName = strings.TrimSpace(idOrName)
	if idOrName == "" {
		return LinkedWorkspace{}, false
	}
	all, err := LoadLinkedWorkspaces()
	if err != nil {
		return LinkedWorkspace{}, false
	}
	lower := strings.ToLower(idOrName)
	for _, w := range all {
		if w.ID == idOrName {
			return w, true
		}
		if w.Name != "" && strings.ToLower(w.Name) == lower {
			return w, true
		}
	}
	return LinkedWorkspace{}, false
}

// Save upserts a remote link by id.
func (w LinkedWorkspace) Save() error {
	w.URL = strings.TrimRight(strings.TrimSpace(w.URL), "/")
	if w.ID == "" || w.URL == "" {
		return fmt.Errorf("linked workspace requires id and url")
	}
	all, err := LoadLinkedWorkspaces()
	if err != nil {
		return err
	}
	found := false
	for i := range all {
		if all[i].ID == w.ID && all[i].URL == w.URL {
			all[i] = w
			found = true
			break
		}
	}
	if !found {
		// Replace same id on different URL, or append.
		out := all[:0]
		for _, x := range all {
			if x.ID == w.ID {
				continue
			}
			out = append(out, x)
		}
		out = append(out, w)
		all = out
	}
	return writeLinked(all)
}

// Sticky builds sticky context from a linked remote workspace.
func (w LinkedWorkspace) Sticky() StickyContext {
	return StickyContext{
		WorkspaceID: w.ID,
		Name:        w.Name,
		Path:        w.Path,
		URL:         w.URL,
		Token:       w.Token,
	}
}

// RemoveLinkedWorkspace deletes a client-side link (id or name).
func RemoveLinkedWorkspace(idOrName string) error {
	all, err := LoadLinkedWorkspaces()
	if err != nil {
		return err
	}
	w, ok := FindLinked(idOrName)
	if !ok {
		return fmt.Errorf("linked workspace %q not found", idOrName)
	}
	out := all[:0]
	for _, x := range all {
		if x.ID == w.ID && x.URL == w.URL {
			continue
		}
		out = append(out, x)
	}
	return writeLinked(out)
}

func writeLinked(all []LinkedWorkspace) error {
	if err := home.EnsureDirs(); err != nil {
		return err
	}
	if all == nil {
		all = []LinkedWorkspace{}
	}
	data, err := yaml.Marshal(&linkedFile{Workspaces: all})
	if err != nil {
		return err
	}
	return os.WriteFile(home.LinkedWorkspacesPath(), data, 0o600)
}
