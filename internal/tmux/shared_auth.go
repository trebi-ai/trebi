package tmux

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
)

// EnsureSharedAuth prepares the harness credential bridge for probe and launch.
// Shared mode reuses the user's interactive login; separate mode does not.
func EnsureSharedAuth(tool, mode string) error {
	if err := applyAuthMode(tool, mode); err != nil {
		return err
	}
	if tool == Claude && mode != harness.AuthModeSeparate {
		return syncClaudeOAuthAccount(config.HarnessDir(Claude))
	}
	return nil
}

func claudeIsolationEnvMap(dir, mode string) map[string]string {
	m := map[string]string{
		"CLAUDE_CONFIG_DIR": dir,
	}
	if mode != harness.AuthModeSeparate {
		// Empty value pins the default credential store (macOS Keychain /
		// ~/.claude/.credentials.json) while CLAUDE_CONFIG_DIR stays isolated.
		m["CLAUDE_SECURESTORAGE_CONFIG_DIR"] = ""
	}
	return m
}

func envPairs(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(m))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

// syncClaudeOAuthAccount copies oauthAccount from ~/.claude.json into the
// harness home so Claude bills the same org the Keychain token belongs to.
func syncClaudeOAuthAccount(configDir string) error {
	if configDir == "" {
		return nil
	}
	userOA, userHas := readClaudeOAuthAccount(filepath.Join(harness.UserHome(), ".claude.json"))
	path := filepath.Join(configDir, ".claude.json")
	m := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &m)
		if m == nil {
			m = map[string]any{}
		}
	}
	_, harnessHas := m["oauthAccount"]
	if !userHas {
		if !harnessHas {
			return nil
		}
		delete(m, "oauthAccount")
	} else if jsonEqual(m["oauthAccount"], userOA) {
		return nil
	} else {
		m["oauthAccount"] = userOA
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o600)
}

func readClaudeOAuthAccount(path string) (any, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var top map[string]any
	if json.Unmarshal(b, &top) != nil {
		return nil, false
	}
	oa, ok := top["oauthAccount"]
	if !ok || oa == nil {
		return nil, false
	}
	return oa, true
}

func jsonEqual(a, b any) bool {
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(ab) == string(bb)
}
