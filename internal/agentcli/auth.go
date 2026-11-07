package agentcli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	agentwire "github.com/trebi-ai/agent-wire"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/store"
)

// DefaultDriver is the vendor implementation of harness.Driver.
type DefaultDriver struct{}

func (DefaultDriver) ProbeAuth(ctx context.Context, name string) harness.AuthStatus {
	t, ok := ByName(name)
	if !ok {
		return harness.AuthStatus{Tool: name, Status: harness.AuthUnknown}
	}
	return t.ProbeAuth(ctx)
}

func (DefaultDriver) AuthCaps(name string) harness.AuthCaps {
	t, ok := ByName(name)
	if !ok {
		return harness.AuthCaps{}
	}
	return t.AuthCapabilities()
}

func (DefaultDriver) LoginAPIKey(ctx context.Context, name string, in harness.TokenLogin) error {
	switch {
	case name == OpenCode || name == OpenCode2:
		return writeOpenCodeKey(name, in.Provider, in.Token)
	case in.Provider != "":
		return fmt.Errorf("%s has no providers", name)
	case name == Codex:
		return loginCodexAPIKey(ctx, in.Token)
	}
	key := harness.TokenEnvKey(name)
	if key == "" {
		return fmt.Errorf("no token env key for %s", name)
	}
	return harness.WriteSecretEnv(name, map[string]string{key: in.Token})
}

func loginCodexAPIKey(ctx context.Context, token string) error {
	bin := lookPathTool(Codex)
	if bin == "" {
		return fmt.Errorf("codex not found")
	}
	cmd := exec.CommandContext(ctx, bin, "login", "--with-api-key")
	cmd.Env = append(os.Environ(), "CODEX_HOME="+config.HarnessDir(Codex))
	cmd.Stdin = strings.NewReader(token + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("codex login --with-api-key: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// lookPathTool gives the binary of name: the visor copy, else PATH.
func lookPathTool(name string) string {
	if r, ok := agentwire.Recipe(agentwire.Harness(name)); ok && r.Bin != "" {
		current := filepath.Join(config.ToolCurrentLink(name), "bin", r.Bin)
		if st, err := os.Stat(current); err == nil && !st.IsDir() {
			return current
		}
	}
	if name == Cursor {
		if p := findCursorBinary(); p != "" {
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

func secretsOK(tool, key string) bool {
	env, err := harness.LoadSecretEnv(tool)
	if err != nil || env == nil {
		return false
	}
	return env[key] != ""
}

// anySecretOK finds the first key with a value: in the secret env of tool,
// in the env of the daemon, or in the harness env in config.yaml.
func anySecretOK(tool string, keys ...string) (string, bool) {
	env := harnessEnv(tool)
	for _, k := range keys {
		if secretsOK(tool, k) || os.Getenv(k) != "" || env[k] != "" {
			return k, true
		}
	}
	return "", false
}

// harnessEnv is the harness env of tool in config.yaml with the secrets
// read. A value that names a missing secret is dropped.
func harnessEnv(tool string) map[string]string {
	cfg, err := config.Load()
	if err != nil {
		return nil
	}
	return secrets.ResolveRefs(cfg.HarnessEnv(tool))
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func authBase(tool string) harness.AuthStatus {
	return harness.AuthStatus{Tool: tool, Mode: currentAuthMode(tool), Status: harness.AuthUnknown}
}

func currentAuthMode(tool string) string {
	cfg, err := config.Load()
	if err != nil {
		return harness.AuthModeShared
	}
	return harness.AuthModeFromConfig(cfg, tool)
}

func loginHint(tool string) string {
	return "trebi harness auth login " + tool
}

// parseClaudeCred reads a Claude credentials JSON once. hasToken is true when
// claudeAiOauth.accessToken is a non-empty string (a real Linux-shaped file).
// A Keychain stub has expiresAt only — never treat its expiry as L0.
func parseClaudeCred(path string) (hasToken bool, expires *time.Time) {
	has, exp, _ := readClaudeCred(path)
	return has, exp
}

func readClaudeCred(path string) (hasToken bool, expires *time.Time, parsed bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, nil, false
	}
	var top map[string]any
	if json.Unmarshal(b, &top) != nil {
		return false, nil, false
	}
	oauth, _ := top["claudeAiOauth"].(map[string]any)
	if oauth == nil {
		return false, nil, true
	}
	if tok, ok := oauth["accessToken"].(string); ok && tok != "" {
		hasToken = true
	}
	if t := coerceTime(oauth["expiresAt"]); !t.IsZero() {
		expires = &t
	}
	return hasToken, expires, true
}

// parseExpiryMS reads a JSON file and returns the first matching nested field
// as milliseconds-since-epoch (or RFC3339 / unix seconds). Zero on failure.
func parseExpiryMS(path string, dotted string) *time.Time {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var top map[string]any
	if json.Unmarshal(b, &top) != nil {
		return nil
	}
	cur := any(top)
	for _, part := range strings.Split(dotted, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = m[part]
		if !ok {
			return nil
		}
	}
	t := coerceTime(cur)
	if t.IsZero() {
		return nil
	}
	return &t
}

func coerceTime(v any) time.Time {
	switch n := v.(type) {
	case float64:
		// ms if huge, else unix seconds.
		if n > 1e12 {
			return time.UnixMilli(int64(n)).UTC()
		}
		if n > 1e9 {
			return time.Unix(int64(n), 0).UTC()
		}
	case json.Number:
		f, _ := n.Float64()
		return coerceTime(f)
	case string:
		return store.ParseTS(n)
	}
	return time.Time{}
}

func runVendor(ctx context.Context, bin string, args, env []string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func l1Token(st harness.AuthStatus, keys ...string) (harness.AuthStatus, bool) {
	if k, ok := anySecretOK(st.Tool, keys...); ok {
		st.Status = harness.AuthOK
		st.Detail = "key (" + k + ")"
		return st, true
	}
	return st, false
}

func expiredOrMissing(st harness.AuthStatus, expires *time.Time) harness.AuthStatus {
	if expires != nil && expires.Before(time.Now()) {
		st.Status = harness.AuthExpired
		st.ExpiresAt = expires
		st.Hint = loginHint(st.Tool) + "  (or setup-token)"
		st.Detail = "credentials expired"
		return st
	}
	st.Status = harness.AuthMissing
	st.Hint = loginHint(st.Tool)
	return st
}

func unknownFile(st harness.AuthStatus, detail string) harness.AuthStatus {
	st.Status = harness.AuthUnknown
	st.Detail = detail
	st.Hint = loginHint(st.Tool)
	return st
}

// --- per-tool ProbeAuth / AuthCapabilities --------------------------------

func (t claudeTool) AuthCapabilities() harness.AuthCaps {
	return harness.AuthCaps{
		LoginModes:     []string{harness.LoginRelay, harness.LoginToken},
		TokenEnvKeys:   []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"},
		LocalCmd:       "claude auth login",
		CredFile:       filepath.Join(config.HarnessDir(t.home()), ".credentials.json"),
		SharedCredFile: filepath.Join(harness.UserHome(), ".claude", ".credentials.json"),
		ExpiryReader:   true,
		KeychainBound:  runtime.GOOS == "darwin",
	}
}

const claudeKeychainDetail = "credentials live in the macOS Keychain; file bridge cannot share them"

func (t claudeTool) ProbeAuth(ctx context.Context) harness.AuthStatus { //nolint:gocyclo,funlen // one rung per login source
	name := t.home()
	st := authBase(name)
	cred := filepath.Join(config.HarnessDir(name), ".credentials.json")
	userCred := filepath.Join(harness.UserHome(), ".claude", ".credentials.json")

	hHas, hExp, hParsed := readClaudeCred(cred)
	uHas, uExp, uParsed := readClaudeCred(userCred)
	hExists := fileExists(cred)
	uExists := fileExists(userCred)
	useUser := st.Mode != harness.AuthModeSeparate

	// L0: expiry is only meaningful on a real credential, never a Keychain stub.
	if hHas {
		st.ExpiresAt = hExp
	} else if useUser && uHas {
		st.ExpiresAt = uExp
	}

	if st, ok := l1Token(st, "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"); ok {
		return st
	}

	bin := lookPathTool(Claude)
	l2LoggedOut := false
	l2Unavailable := bin == ""
	if bin != "" {
		out, err := runVendor(ctx, bin, []string{"auth", "status"},
			envPairs(claudeIsolationEnvMap(config.HarnessDir(name), st.Mode)))
		var payload struct {
			LoggedIn         bool   `json:"loggedIn"`
			Email            string `json:"email"`
			SubscriptionType string `json:"subscriptionType"`
		}
		if json.Unmarshal([]byte(out), &payload) == nil && (err == nil || strings.Contains(out, "loggedIn")) {
			if payload.LoggedIn {
				st.Status = harness.AuthOK
				st.Account = accountLine(payload.Email, payload.SubscriptionType)
				return st
			}
			l2LoggedOut = true
		} else {
			low := strings.ToLower(out)
			if strings.Contains(low, "not logged") || strings.Contains(low, `"loggedin": false`) {
				l2LoggedOut = true
			} else if err == nil && strings.Contains(low, "logged in") {
				st.Status = harness.AuthOK
				return st
			} else if err != nil {
				st.Detail = "auth status probe failed"
				l2Unavailable = true
			}
		}
	}

	hasReal := hHas || (useUser && uHas)
	if hasReal {
		if l2LoggedOut {
			return expiredOrMissing(st, st.ExpiresAt)
		}
		if st.ExpiresAt != nil && st.ExpiresAt.Before(time.Now()) {
			return expiredOrMissing(st, st.ExpiresAt)
		}
		if hExists || (useUser && uExists) {
			return unknownFile(st, "credentials file present; auth status unconfirmed")
		}
	}

	hStub := hExists && !hHas && hParsed
	uStub := uExists && !uHas && uParsed
	stubOnly := (hStub || (useUser && uStub)) && !hasReal
	if stubOnly && (l2LoggedOut || l2Unavailable) {
		st.Status = harness.AuthMissing
		st.ExpiresAt = nil
		st.Detail = claudeKeychainDetail
		st.Hint = loginHint(name)
		return st
	}

	if l2LoggedOut {
		return expiredOrMissing(st, st.ExpiresAt)
	}
	if (hExists || (useUser && uExists)) && !hasReal && !stubOnly {
		return unknownFile(st, "credentials file present; auth status unconfirmed")
	}
	st.Status = harness.AuthMissing
	st.Hint = loginHint(name)
	return st
}

func (codexTool) AuthCapabilities() harness.AuthCaps {
	return harness.AuthCaps{
		LoginModes:     []string{harness.LoginRelay, harness.LoginToken},
		TokenEnvKeys:   []string{"OPENAI_API_KEY"},
		LocalCmd:       "codex login --device-auth",
		CredFile:       filepath.Join(config.HarnessDir(Codex), "auth.json"),
		SharedCredFile: filepath.Join(harness.UserHome(), ".codex", "auth.json"),
		ExpiryReader:   true,
	}
}

func (codexTool) ProbeAuth(ctx context.Context) harness.AuthStatus {
	st := authBase(Codex)
	authPath := filepath.Join(harness.UserHome(), ".codex", "auth.json")
	hPath := filepath.Join(config.HarnessDir(Codex), "auth.json")
	if st.Mode == harness.AuthModeSeparate {
		authPath = hPath
	} else if fileExists(hPath) {
		authPath = hPath
	}
	if t := parseExpiryMS(authPath, "expires_at"); t != nil {
		st.ExpiresAt = t
	} else if t := parseExpiryMS(authPath, "last_refresh"); t != nil {
		st.ExpiresAt = t
	}
	st.Account = codexAccount(authPath)
	if st, ok := l1Token(st, "OPENAI_API_KEY"); ok {
		return st
	}
	bin := lookPathTool(Codex)
	if bin != "" {
		out, err := runVendor(ctx, bin, []string{"login", "status"},
			[]string{"CODEX_HOME=" + config.HarnessDir(Codex)})
		low := strings.ToLower(out)
		if err == nil && strings.Contains(low, "logged in") && !strings.Contains(low, "not logged in") {
			st.Status = harness.AuthOK
			return st
		}
		if fileExists(authPath) && strings.Contains(low, "not logged in") {
			return loginEnded(st)
		}
	}
	if st.ExpiresAt != nil && st.ExpiresAt.Before(time.Now()) {
		return expiredOrMissing(st, st.ExpiresAt)
	}
	if fileExists(authPath) && bin == "" {
		return unknownFile(st, "auth.json present; login status unconfirmed")
	}
	st.Status = harness.AuthMissing
	st.Hint = loginHint(Codex) + "  (enable Device Code Authorization in ChatGPT settings once)"
	return st
}

// accountLine joins an email and a plan name for display: "a@b.c (Max)".
func accountLine(email, plan string) string {
	plan = strings.TrimSpace(plan)
	if plan == "" || email == "" {
		return email
	}
	return email + " (" + strings.ToUpper(plan[:1]) + plan[1:] + ")"
}

// codexAccount reads the email and plan claims of tokens.id_token. It does
// not check the signature: the value is for display only.
func codexAccount(authPath string) string {
	b, err := os.ReadFile(authPath)
	if err != nil {
		return ""
	}
	var f struct {
		Tokens struct {
			IDToken string `json:"id_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(b, &f) != nil {
		return ""
	}
	parts := strings.Split(f.Tokens.IDToken, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
		Auth  struct {
			Plan string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	return accountLine(claims.Email, claims.Auth.Plan)
}

func (grokTool) AuthCapabilities() harness.AuthCaps {
	return harness.AuthCaps{
		LoginModes:     []string{harness.LoginRelay, harness.LoginToken},
		TokenEnvKeys:   []string{"XAI_API_KEY"},
		LocalCmd:       "grok login --device-auth",
		CredFile:       filepath.Join(config.HarnessDir(Grok), "auth.json"),
		SharedCredFile: filepath.Join(harness.UserHome(), ".grok", "auth.json"),
		ExpiryReader:   true,
	}
}

func (grokTool) ProbeAuth(ctx context.Context) harness.AuthStatus {
	st := authBase(Grok)
	if st, ok := l1Token(st, "XAI_API_KEY"); ok {
		return st
	}
	authPath := filepath.Join(config.HarnessDir(Grok), "auth.json")
	acct, ok := firstGrokAccount(authPath)
	if !ok {
		st.Status = harness.AuthMissing
		st.Hint = loginHint(Grok)
		return st
	}
	st.Account = acct.Email
	if t := store.ParseTS(acct.ExpiresAt); !t.IsZero() {
		st.ExpiresAt = &t
	}
	// grok refreshes an expired access token by itself when it has a refresh token.
	if st.ExpiresAt != nil && st.ExpiresAt.Before(time.Now()) && acct.RefreshToken == "" {
		return expiredOrMissing(st, st.ExpiresAt)
	}
	if bin := lookPathTool(Grok); bin != "" {
		out, err := runVendor(ctx, bin, []string{"models"}, []string{"GROK_HOME=" + config.HarnessDir(Grok)})
		// At the expiry, grok can report "not authenticated" while it
		// refreshes the token. A second call sees the new token.
		if acct.RefreshToken != "" && strings.Contains(strings.ToLower(out), "not authenticated") {
			out, err = runVendor(ctx, bin, []string{"models"}, []string{"GROK_HOME=" + config.HarnessDir(Grok)})
		}
		low := strings.ToLower(out)
		switch {
		case strings.Contains(low, "not authenticated"):
			return loginEnded(st)
		case err == nil && strings.Contains(low, "logged in"):
			// After a refresh the file has a new expiry.
			if a, ok := firstGrokAccount(authPath); ok {
				if t := store.ParseTS(a.ExpiresAt); !t.IsZero() {
					st.ExpiresAt = &t
				}
			}
			st.Status = harness.AuthOK
			return st
		}
	}
	st.Status = harness.AuthOK
	st.Detail = "login file present; live check unavailable"
	return st
}

func (t opencodeTool) AuthCapabilities() harness.AuthCaps {
	return harness.AuthCaps{
		LoginModes:     []string{harness.LoginRelay, harness.LoginToken},
		LocalCmd:       t.name + " auth login",
		CredFile:       opencodeAuthPath(t.name),
		SharedCredFile: opencodeUserAuth(),
	}
}

// ProbeAuth reads the providers in the active auth.json, then asks `auth
// list` (L2) with the same data folder.
func (t opencodeTool) ProbeAuth(ctx context.Context) harness.AuthStatus {
	st := authBase(t.name)
	ids := opencodeProviderIDs(opencodeAuthPath(t.name))
	st.Account = opencodeAccount(ids)
	if bin := lookPathTool(t.name); bin != "" {
		out, err := runVendor(ctx, bin, []string{"auth", "list"},
			[]string{"XDG_DATA_HOME=" + opencodeDataHome(t.name)})
		if err == nil {
			if n, ok := opencodeCredentialCount(out); ok {
				if n > 0 {
					st.Status = harness.AuthOK
					return st
				}
				st.Status = harness.AuthMissing
				st.Hint = loginHint(t.name)
				return st
			}
		}
	}
	if len(ids) > 0 {
		return unknownFile(st, "auth.json present; auth list unconfirmed")
	}
	st.Status = harness.AuthMissing
	st.Hint = loginHint(t.name)
	return st
}

func (copilotTool) AuthCapabilities() harness.AuthCaps {
	return harness.AuthCaps{
		LoginModes:   []string{harness.LoginRelay, harness.LoginToken},
		TokenEnvKeys: []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"},
		LocalCmd:     "copilot  (then /login)",
		CredFile:     filepath.Join(harness.UserHome(), ".copilot"),
		AttachOnly:   true,
	}
}

func (copilotTool) ProbeAuth(ctx context.Context) harness.AuthStatus {
	st := authBase(Copilot)
	if st, ok := l1Token(st, "COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"); ok {
		return st
	}
	if entries, err := os.ReadDir(filepath.Join(harness.UserHome(), ".copilot")); err == nil && len(entries) > 0 {
		return unknownFile(st, "copilot home present; status unconfirmed")
	}
	st.Status = harness.AuthMissing
	st.Hint = loginHint(Copilot) + "  (or set COPILOT_GITHUB_TOKEN)"
	return st
}

func (cursorTool) AuthCapabilities() harness.AuthCaps {
	return harness.AuthCaps{
		LoginModes:   []string{harness.LoginToken},
		TokenEnvKeys: []string{"CURSOR_API_KEY"},
		LocalCmd:     "cursor-agent login",
	}
}

func (cursorTool) ProbeAuth(ctx context.Context) harness.AuthStatus {
	st := authBase(Cursor)
	if st, ok := l1Token(st, "CURSOR_API_KEY"); ok {
		return st
	}
	bin := lookPathTool(Cursor)
	if bin != "" {
		out, err := runVendor(ctx, bin, []string{"status"}, []string{
			"CURSOR_CONFIG_DIR=" + config.HarnessDir(Cursor),
			"CURSOR_DATA_DIR=" + config.HarnessDir(Cursor),
		})
		low := strings.ToLower(out)
		if err == nil && (strings.Contains(low, "logged in") || strings.Contains(out, "✓")) {
			st.Status = harness.AuthOK
			for _, line := range strings.Split(out, "\n") {
				line = strings.TrimSpace(line)
				if strings.Contains(strings.ToLower(line), "logged in") {
					st.Detail = line
					break
				}
			}
			return st
		}
		if strings.Contains(low, "not logged") {
			st.Status = harness.AuthMissing
			st.Hint = loginHint(Cursor) + "  (or export CURSOR_API_KEY)"
			st.Detail = "cursor status: not logged in"
			return st
		}
	}
	st.Status = harness.AuthMissing
	st.Hint = loginHint(Cursor) + "  (or export CURSOR_API_KEY)"
	return st
}

func (piTool) AuthCapabilities() harness.AuthCaps {
	return harness.AuthCaps{
		LoginModes: []string{harness.LoginRelay, harness.LoginToken},
		TokenEnvKeys: []string{
			"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "XAI_API_KEY", "GOOGLE_API_KEY", "OPENROUTER_API_KEY",
		},
		LocalCmd:   "pi  (then /login)",
		CredFile:   filepath.Join(config.HarnessDir(Pi), "auth.json"),
		AttachOnly: true,
	}
}

func (piTool) ProbeAuth(ctx context.Context) harness.AuthStatus {
	st := authBase(Pi)
	keys := []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "XAI_API_KEY", "GOOGLE_API_KEY", "OPENROUTER_API_KEY"}
	if st, ok := l1Token(st, keys...); ok {
		return st
	}
	authPath := filepath.Join(config.HarnessDir(Pi), "auth.json")
	if !fileExists(authPath) {
		authPath = filepath.Join(harness.UserHome(), ".pi", "agent", "auth.json")
	}
	bin := lookPathTool(Pi)
	if bin != "" {
		for _, provider := range []string{"xai", "anthropic", "openai", "google", "openrouter"} {
			out, err := runVendor(ctx, bin, []string{"auth", "check", "--provider", provider, "--json"},
				[]string{"PI_CODING_AGENT_DIR=" + config.HarnessDir(Pi), "PI_OFFLINE=1"})
			if err != nil {
				continue
			}
			var payload struct {
				Status string `json:"status"`
			}
			if json.Unmarshal([]byte(out), &payload) == nil && payload.Status == "ready" {
				st.Status = harness.AuthOK
				st.Detail = provider
				return st
			}
		}
	}
	if b, err := os.ReadFile(authPath); err == nil {
		var m map[string]any
		if json.Unmarshal(b, &m) == nil && len(m) > 0 {
			return unknownFile(st, "auth.json present; auth check unconfirmed")
		}
	}
	st.Status = harness.AuthMissing
	st.Hint = loginHint(Pi) + "  (or /login in pi TUI)"
	return st
}

func (geminiTool) AuthCapabilities() harness.AuthCaps {
	return harness.AuthCaps{
		LoginModes:   []string{harness.LoginRelay, harness.LoginToken},
		TokenEnvKeys: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"},
		LocalCmd:     "gemini  (NO_BROWSER=1)",
		CredFile:     filepath.Join(harness.UserHome(), ".gemini", "oauth_creds.json"),
		ExpiryReader: true,
		RelayFragile: true,
	}
}

func (geminiTool) ProbeAuth(ctx context.Context) harness.AuthStatus {
	st := authBase(Gemini)
	if st, ok := l1Token(st, "GEMINI_API_KEY", "GOOGLE_API_KEY"); ok {
		return st
	}
	cred := filepath.Join(harness.UserHome(), ".gemini", "oauth_creds.json")
	if t := parseExpiryMS(cred, "expiry_date"); t != nil {
		st.ExpiresAt = t
		if t.Before(time.Now()) {
			return expiredOrMissing(st, t)
		}
	}
	if fileExists(cred) {
		return unknownFile(st, "oauth_creds.json present; status unconfirmed")
	}
	st.Status = harness.AuthMissing
	st.Hint = loginHint(Gemini)
	return st
}

// loginEnded is the probe result for a login that the vendor rejected.
func loginEnded(st harness.AuthStatus) harness.AuthStatus {
	st.Status = harness.AuthExpired
	st.Detail = "The login ended. Log in again."
	st.Hint = loginHint(st.Tool)
	return st
}
