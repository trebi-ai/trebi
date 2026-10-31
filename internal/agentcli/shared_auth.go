package agentcli

import (
	"sort"

	"github.com/trebi-ai/trebi/internal/harness"
)

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
