package tmux

import (
	"os"
	"path/filepath"

	"github.com/trebi-ai/trebi/internal/harness"
)

// HarnessLocator finds harness binaries and terminal logins for harness.List.
type HarnessLocator struct{}

// Locate returns the binary that a Trebi run uses, or "".
func (HarnessLocator) Locate(name string) string {
	if name == Fake {
		return ""
	}
	return lookPathTool(name)
}

// TerminalLogin reports a vendor login in the user home: the shared
// credential file, or for Claude on macOS the account in ~/.claude.json.
func (HarnessLocator) TerminalLogin(name string) bool {
	if name == Fake {
		return false
	}
	if f := (DefaultDriver{}).AuthCaps(name).SharedCredFile; f != "" {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	if name == Claude {
		_, err := os.Stat(filepath.Join(harness.UserHome(), ".claude.json"))
		return err == nil
	}
	return false
}
