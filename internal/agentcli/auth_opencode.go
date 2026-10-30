package agentcli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
)

// Provider methods.
const (
	MethodBrowser = "browser"
	MethodKey     = "key"
)

// opencodeProviders is the short list that Trebi shows first. OpenCode
// 1.18.29 has no browser login for Anthropic; OpenAI has a device code
// login ("ChatGPT Pro/Plus (headless)").
var opencodeProviders = []harness.Provider{ //nolint:gochecknoglobals // fixed table, never mutated
	{ID: "opencode-go", Label: "OpenCode Go", Group: "opencode", Methods: []string{MethodKey}, KeyURL: "https://opencode.ai/auth",
		Note: "Monthly plan, open coding models. Usage limits each 5 hours, week, and month."},
	{ID: "opencode", Label: "OpenCode Zen", Group: "opencode", Methods: []string{MethodKey}, KeyURL: "https://opencode.ai/auth",
		Note: "Pay per use. Some models are free."},
	{ID: "anthropic", Label: "Anthropic", Group: "other", Methods: []string{MethodKey}, KeyURL: "https://console.anthropic.com"},
	{ID: "openai", Label: "OpenAI", Group: "other", Methods: []string{MethodBrowser, MethodKey}, KeyURL: "https://platform.openai.com/api-keys"},
	{ID: "openrouter", Label: "OpenRouter", Group: "other", Methods: []string{MethodKey}, KeyURL: "https://openrouter.ai/keys"},
}

// opencodeBrowserMethod is the --method label of the browser login per provider.
var opencodeBrowserMethod = map[string]string{ //nolint:gochecknoglobals // fixed table, never mutated
	"openai": "ChatGPT Pro/Plus (headless)",
}

var (
	providerIDRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	credentialsRe = regexp.MustCompile(`(\d+)\s+credentials?`)
	storedRowRe   = regexp.MustCompile(`(?m)\S\s{2,}stored\s*$`)
	ansiEscapeRe  = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
)

// OpenCodeProviders lists the fixed providers plus the ones that the active
// auth.json has, with LoggedIn set.
func OpenCodeProviders(name string) []harness.Provider {
	have := map[string]bool{}
	for _, id := range opencodeProviderIDs(opencodeAuthPath(name)) {
		have[id] = true
	}
	out := make([]harness.Provider, 0, len(opencodeProviders)+len(have))
	known := map[string]bool{}
	for _, p := range opencodeProviders {
		p.LoggedIn = have[p.ID]
		known[p.ID] = true
		out = append(out, p)
	}
	extra := []string{}
	for id := range have {
		if !known[id] {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	for _, id := range extra {
		out = append(out, harness.Provider{ID: id, Label: id, Group: "other", LoggedIn: true, Methods: []string{MethodKey}})
	}
	return out
}

// opencodeDataHome is XDG_DATA_HOME for every Trebi run, login, and probe. A
// shared login uses the user folder: OpenCode 2.x keeps its keys in
// opencode.db there and does not read auth.json.
func opencodeDataHome(name string) string {
	if sharedUnlessEnded(name, currentAuthMode(name), opencodeUserAuth()) != harness.AuthModeSeparate {
		return opencodeUserData()
	}
	return opencodeOwnData(name)
}

// opencodeOwnData is the data folder of a separate login.
func opencodeOwnData(name string) string { return filepath.Join(config.HarnessDir(name), "data") }

func opencodeUserData() string { return filepath.Join(harness.UserHome(), ".local", "share") }

// opencodeAuthPath is the auth.json in the Trebi data folder. In shared mode
// it links to the user file.
func opencodeAuthPath(name string) string {
	return filepath.Join(opencodeOwnData(name), "opencode", "auth.json")
}

func opencodeUserAuth() string {
	return filepath.Join(opencodeUserData(), "opencode", "auth.json")
}

// opencodeLoginArgs picks the provider and the browser method, so the relay
// never sees the picker.
func opencodeLoginArgs(provider string) ([]string, error) {
	if provider == "" {
		return nil, fmt.Errorf("choose a provider for the OpenCode login")
	}
	method, ok := opencodeBrowserMethod[provider]
	if !ok {
		return nil, fmt.Errorf("%s has no browser login; use a key", provider)
	}
	return []string{"--provider", provider, "--method", "'" + method + "'"}, nil
}

// writeOpenCodeKey writes {"<provider>": {"type": "api", "key": "<token>"}}
// into the active auth.json. It keeps the other entries. In shared mode it
// writes the user file, which the active path links to.
func writeOpenCodeKey(name, provider, token string) error {
	if !providerIDRe.MatchString(provider) {
		return fmt.Errorf("choose a provider for the key")
	}
	path := opencodeAuthPath(name)
	if currentAuthMode(name) != harness.AuthModeSeparate {
		path = opencodeUserAuth()
	} else if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(path)
	}
	m := map[string]json.RawMessage{}
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
	}
	entry, err := json.Marshal(map[string]string{"type": "api", "key": token})
	if err != nil {
		return err
	}
	m[provider] = entry
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := harness.WriteFileIfChanged(path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return applyAuthMode(name, currentAuthMode(name))
}

// opencodeProviderIDs returns the provider ids in an OpenCode auth.json.
func opencodeProviderIDs(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// opencodeAccount is the display list of providers: "Anthropic, xAI".
func opencodeAccount(ids []string) string {
	labels := make([]string, 0, len(ids))
	for _, id := range ids {
		label := id
		for _, p := range opencodeProviders {
			if p.ID == id {
				label = p.Label
			}
		}
		labels = append(labels, label)
	}
	return strings.Join(labels, ", ")
}

// opencodeCredentialCount reads "N credentials" (1.x) or counts the "stored"
// table rows (2.x) from `auth list`.
func opencodeCredentialCount(out string) (int, bool) {
	out = ansiEscapeRe.ReplaceAllString(out, "")
	m := credentialsRe.FindStringSubmatch(out)
	if m == nil {
		n := len(storedRowRe.FindAllString(out, -1))
		return n, n > 0
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}
