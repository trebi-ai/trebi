package tmux

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/harness"
	"github.com/trebi-ai/trebi/internal/store"
)

// DefaultDriver is the tmux/vendor implementation of harness.Driver.
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

func (DefaultDriver) ApplyAuthMode(name, mode string) error {
	return applyAuthMode(name, mode)
}

// LoginCommand ends with "; exit", so the login session ends when the vendor
// CLI ends.
func (DefaultDriver) LoginCommand(name, mode, provider string) (string, []string, error) {
	bin := lookPathTool(name)
	if bin == "" {
		return "", nil, fmt.Errorf("%s binary not found", name)
	}
	args, env := vendorLoginArgsFor(name, mode, runtime.GOOS)
	if name == OpenCode || name == OpenCode2 {
		extra, err := opencodeLoginArgs(provider)
		if err != nil {
			return "", nil, err
		}
		args = append(args, extra...)
	}
	cmd := bin
	if len(args) > 0 {
		cmd += " " + strings.Join(args, " ")
	}
	return cmd + "; exit", env, nil
}

func (DefaultDriver) ExtractLogin(name, screen string) harness.LoginExtract {
	t, ok := ByName(name)
	if !ok {
		return harness.LoginExtract{NoExtract: true}
	}
	return extractLogin(t.Name(), screen)
}

func (DefaultDriver) ExecLogout(ctx context.Context, name string) error {
	bin := lookPathTool(name)
	if bin == "" {
		return fmt.Errorf("%s not found", name)
	}
	args, ok := vendorLogoutArgs(name)
	if !ok {
		return fmt.Errorf("shared logout not automated for %s — use vendor CLI", name)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s logout: %w (%s)", name, err, strings.TrimSpace(string(out)))
	}
	return nil
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

func (DefaultDriver) NewNamedSession(session, cwd string) error {
	return NewNamedSession(session, cwd)
}
func (DefaultDriver) ReadScreen(session string) (string, error) { return ReadScreen(session) }
func (DefaultDriver) Send(session, text string) error           { return Send(session, text) }
func (DefaultDriver) SendKey(session, key string) error         { return SendKey(session, key) }
func (DefaultDriver) KillSession(session string) error          { return KillSession(session) }
func (DefaultDriver) HasSession(session string) bool            { return HasSession(session) }

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

func applyAuthMode(name, mode string) error {
	switch name {
	case Claude:
		return (harnessHome{tool: Claude}).bridgeAuth(filepath.Join(".claude", ".credentials.json"), mode)
	case Grok:
		return (harnessHome{tool: Grok}).bridgeAuth(filepath.Join(".grok", "auth.json"), mode)
	case Codex:
		return (harnessHome{tool: Codex}).bridgeAuth(filepath.Join(".codex", "auth.json"), mode)
	case Pi:
		return (harnessHome{tool: Pi}).bridgeAuth(filepath.Join(".pi", "agent", "auth.json"), mode)
	case OpenCode, OpenCode2:
		src := opencodeUserAuth()
		return harness.SymlinkAuth(src, opencodeAuthPath(name), sharedUnlessEnded(name, mode, src))
	default:
		return nil
	}
}

// sharedUnlessEnded keeps a dead shared login from being linked again: with
// the login-ended marker, shared mode bridges like separate mode.
func sharedUnlessEnded(name, mode, userFile string) string {
	if mode != harness.AuthModeSeparate && harness.LoginEnded(name, userFile) {
		return harness.AuthModeSeparate
	}
	return mode
}

func vendorLoginArgsFor(tool, mode, goos string) (args []string, env []string) {
	switch tool {
	case Fake:
		// tmux does not inherit the daemon env, so name the home explicitly.
		return []string{"login"}, []string{"TREBI_HOME=" + config.BaseDir()}
	case Claude:
		dir := config.HarnessDir(Claude)
		if mode == harness.AuthModeSeparate {
			args = []string{"setup-token"}
		} else {
			args = []string{"auth", "login"}
		}
		env = envPairs(claudeIsolationEnvMap(dir, mode))
		_ = goos // login args no longer differ by OS; shared uses auth login.
	case Grok:
		args = []string{"login", "--device-auth"}
		env = []string{"GROK_HOME=" + config.HarnessDir(Grok)}
	case Codex:
		args = []string{"login", "--device-auth"}
		env = []string{"CODEX_HOME=" + config.HarnessDir(Codex)}
	case OpenCode, OpenCode2:
		// V2 keeps credentials in the same XDG data store as 1.x.
		args = []string{"auth", "login"}
		env = []string{"XDG_DATA_HOME=" + opencodeDataHome(tool)}
	case Gemini:
		args = []string{}
		env = []string{"NO_BROWSER=1"}
	case Cursor:
		args = []string{"login"}
		env = []string{
			"CURSOR_CONFIG_DIR=" + config.HarnessDir(Cursor),
			"CURSOR_DATA_DIR=" + config.HarnessDir(Cursor),
		}
	case Pi:
		env = []string{"PI_CODING_AGENT_DIR=" + config.HarnessDir(Pi)}
	case Copilot:
		// TUI /login — attach-only.
	}
	return args, env
}

func vendorLogoutArgs(tool string) ([]string, bool) {
	switch tool {
	case Claude:
		return []string{"auth", "logout"}, true
	case Codex:
		return []string{"logout"}, true
	case OpenCode, OpenCode2:
		return []string{"auth", "logout"}, true
	case Cursor:
		return []string{"logout"}, true
	case Fake:
		return []string{"logout"}, true
	default:
		return nil, false
	}
}

func lookPathTool(name string) string {
	current := filepath.Join(config.BinDir(), name, "current", name)
	if st, err := os.Stat(current); err == nil && !st.IsDir() {
		return current
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

func anySecretOK(tool string, keys ...string) (string, bool) {
	for _, k := range keys {
		if secretsOK(tool, k) || os.Getenv(k) != "" {
			return k, true
		}
	}
	return "", false
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

func (claudeTool) AuthCapabilities() harness.AuthCaps {
	return harness.AuthCaps{
		LoginModes:     []string{harness.LoginRelay, harness.LoginToken},
		TokenEnvKeys:   []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"},
		LocalCmd:       "claude auth login",
		CredFile:       filepath.Join(config.HarnessDir(Claude), ".credentials.json"),
		SharedCredFile: filepath.Join(harness.UserHome(), ".claude", ".credentials.json"),
		ExpiryReader:   true,
		KeychainBound:  runtime.GOOS == "darwin",
	}
}

const claudeKeychainDetail = "credentials live in the macOS Keychain; file bridge cannot share them"

func (claudeTool) ProbeAuth(ctx context.Context) harness.AuthStatus {
	st := authBase(Claude)
	_ = EnsureSharedAuth(Claude, st.Mode)
	cred := filepath.Join(config.HarnessDir(Claude), ".credentials.json")
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

	if st, ok := l1Token(st, "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"); ok {
		return st
	}

	bin := lookPathTool(Claude)
	l2LoggedOut := false
	l2Unavailable := bin == ""
	if bin != "" {
		out, err := runVendor(ctx, bin, []string{"auth", "status"},
			envPairs(claudeIsolationEnvMap(config.HarnessDir(Claude), st.Mode)))
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
		st.Hint = loginHint(Claude)
		return st
	}

	if l2LoggedOut {
		return expiredOrMissing(st, st.ExpiresAt)
	}
	if (hExists || (useUser && uExists)) && !hasReal && !stubOnly {
		return unknownFile(st, "credentials file present; auth status unconfirmed")
	}
	st.Status = harness.AuthMissing
	st.Hint = loginHint(Claude)
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
	_ = EnsureSharedAuth(Codex, st.Mode)
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

// loginEnded is the probe result for a login that the vendor rejected. In
// shared mode it also stops the bridge from linking the dead login again.
func loginEnded(st harness.AuthStatus) harness.AuthStatus {
	st.Status = harness.AuthExpired
	st.Detail = "The login ended. Log in again."
	st.Hint = loginHint(st.Tool)
	if st.Mode != harness.AuthModeSeparate {
		_ = harness.MarkLoginEnded(st.Tool)
		_ = applyAuthMode(st.Tool, st.Mode)
	}
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
	_ = EnsureSharedAuth(Grok, st.Mode)
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
	_ = EnsureSharedAuth(t.name, st.Mode)
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
	_ = EnsureSharedAuth(Copilot, st.Mode)
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
	_ = EnsureSharedAuth(Cursor, st.Mode)
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
	_ = EnsureSharedAuth(Pi, st.Mode)
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
	_ = EnsureSharedAuth(Gemini, st.Mode)
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

// --- screen extraction -----------------------------------------------------

var (
	urlRe        = regexp.MustCompile(`https://[^\s<>"']+`)
	deviceCodeRe = regexp.MustCompile(`\b([A-Z0-9]{4}-[A-Z0-9]{4,6})\b`)
	pasteRe      = regexp.MustCompile(`(?i)(paste|enter).{0,40}(code|token|authorization)`)
	oatRe        = regexp.MustCompile(`sk-ant-oat01-[A-Za-z0-9_-]+`)
	// Codex 0.162 texts. The device code request fails with a 4xx status when
	// the ChatGPT account has device code login off.
	codexDeviceOffRe = regexp.MustCompile(`(?i)device code (login|authorization) is (not enabled|disabled|off)|device code request failed with status 40[0-9]`)
	codexLoginErrRe  = regexp.MustCompile(`(?i)Error logging in with device code:\s*(.+)`)
)

func extractLogin(name, screen string) harness.LoginExtract {
	switch name {
	case Pi, Copilot:
		return harness.LoginExtract{NoExtract: true}
	}
	var ex harness.LoginExtract
	if m := urlRe.FindString(screen); m != "" {
		ex.URL = strings.TrimRight(m, ".,);")
	}
	if m := deviceCodeRe.FindString(screen); m != "" {
		ex.UserCode = m
	}
	if pasteRe.MatchString(screen) {
		ex.AwaitCode = true
	}
	if name == Claude {
		if m := oatRe.FindString(screen); m != "" {
			ex.Token = m
		}
	}
	if name == Codex {
		switch {
		case codexDeviceOffRe.MatchString(screen):
			ex.Error = "Device code login is off for your ChatGPT account."
			ex.Hint = "In ChatGPT, open Settings → Security and turn on device code login."
		case codexLoginErrRe.MatchString(screen):
			ex.Error = strings.TrimSpace(codexLoginErrRe.FindStringSubmatch(screen)[1])
		}
	}
	return ex
}
