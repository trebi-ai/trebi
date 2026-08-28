package harness

import (
	"os"
	"path/filepath"

	config "github.com/trebi-ai/trebi/internal/config"
)

// Info is a harness status row.
type Info struct {
	Name    string `json:"name"`
	Home    string `json:"home"`
	BinPath string `json:"bin_path,omitempty"`
	Present bool   `json:"present"`
}

// Names returns the supported harness names.
func Names() []string {
	return []string{"claude", "grok", "codex", "opencode", "gemini", "cursor", "copilot", "pi"}
}

// List returns harness status under TREBI_HOME.
func List() []Info {
	var out []Info
	for _, n := range Names() {
		h := config.HarnessDir(n)
		_, err := os.Stat(h)
		bin := filepath.Join(config.BinDir(), n, "current")
		_, berr := os.Stat(bin)
		info := Info{Name: n, Home: h, Present: err == nil}
		if berr == nil {
			info.BinPath = bin
		}
		out = append(out, info)
	}
	return out
}

// Known reports whether name is a supported harness. Callers that build a path
// from request input must check this first: HarnessDir joins the name, so an
// unchecked "../.." escapes TREBI_HOME.
func Known(name string) bool {
	for _, n := range Names() {
		if n == name {
			return true
		}
	}
	return false
}
