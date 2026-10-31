package harness

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	agentvisor "github.com/trebi-ai/agent-visor"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/store"
)

// Auth statuses (ladder output).
const (
	AuthOK      = "ok"
	AuthMissing = "missing"
	AuthExpired = "expired"
	AuthUnknown = "unknown"
)

// Login mode tokens declared on AuthCaps.LoginModes.
const (
	LoginRelay = "relay"
	LoginToken = "token"
)

// Stamp sources. "logout" suppresses a fresh auth_missing alert.
const (
	SourceLaunch = "launch"
	SourceProbe  = "probe"
	SourceLogout = "logout"
	SourceLogin  = "login"
	SourceReaper = "mid-session"
)

// ExpiryGrace is how long after auth_expires_at the prober waits before probing.
const ExpiryGrace = 30 * time.Second

// AuthStatus is the result of one probe ladder run.
type AuthStatus struct {
	Tool      string     `json:"tool"`
	Mode      string     `json:"mode"`
	Status    string     `json:"status"` // ok|missing|expired|unknown
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Hint      string     `json:"hint,omitempty"`
	Detail    string     `json:"detail,omitempty"`
	Account   string     `json:"account,omitempty"` // email or provider list, for display only
	ProbedAt  time.Time  `json:"probed_at,omitempty"`
}

// OK reports whether the queue gate should pass.
func (s AuthStatus) OK() bool { return s.Status == AuthOK }

// AuthCaps is the per-driver declaration of the auth matrix.
type AuthCaps struct {
	LoginModes   []string `json:"login_modes"`
	TokenEnvKeys []string `json:"token_env_keys,omitempty"`
	LocalCmd     string   `json:"local_cmd,omitempty"`
	CredFile     string   `json:"cred_file,omitempty"`
	// SharedCredFile is the user's own credential file that shared mode links.
	SharedCredFile string `json:"shared_cred_file,omitempty"`
	ExpiryReader   bool   `json:"expiry_reader,omitempty"`
	RelayFragile   bool   `json:"relay_fragile,omitempty"`
	AttachOnly     bool   `json:"attach_only,omitempty"`
	KeychainBound  bool   `json:"keychain_bound,omitempty"`
}

// HasLoginMode reports whether mode is in LoginModes.
func (c AuthCaps) HasLoginMode(mode string) bool {
	for _, m := range c.LoginModes {
		if m == mode {
			return true
		}
	}
	return false
}

// State is the durable harness_state auth row plus config mode.
type State struct {
	Name      string
	AuthOK    sql.NullInt64
	Status    string
	Detail    string
	ProbedAt  string
	ExpiresAt string
	Account   string
	UpdatedAt string
}

// StampResult is the outcome of Stamp. Flipped gates the queue; Changed
// drives harness.updated.
type StampResult struct {
	Flipped bool
	Changed bool
	PrevOK  bool
	HadRow  bool
	OK      bool
}

// Driver is the vendor seam. It is here so that harness does not import agentcli.
type Driver interface {
	ProbeAuth(ctx context.Context, name string) AuthStatus
	AuthCaps(name string) AuthCaps
	LoginAPIKey(ctx context.Context, name string, in TokenLogin) error
}

// Provider is one OpenCode model provider that Trebi can log in to.
type Provider struct {
	ID       string   `json:"id"`    // "anthropic"
	Label    string   `json:"label"` // "Anthropic"
	Group    string   `json:"group"` // "opencode" or "other"
	Note     string   `json:"note,omitempty"`
	LoggedIn bool     `json:"logged_in"`
	Methods  []string `json:"methods"` // "browser", "key"
	KeyURL   string   `json:"key_url,omitempty"`
}

// TokenLogin is the body of an API key login.
type TokenLogin struct {
	Token    string `json:"token"`
	Provider string `json:"provider,omitempty"` // OpenCode only
}

// Options for New.
type Options struct {
	// Visor gives the agent-visor that owns the harness installs, logins,
	// and homes. Nil leaves those calls with ErrNoVisor (tests).
	Visor func(ctx context.Context) (Visor, error)
	// LoginPoll is the time between two reads of a login state in the
	// visor. Zero is 500 ms.
	LoginPoll time.Duration
}

// Service owns harness auth stamps and probes. The visor owns the installs,
// the logins, and the harness homes.
type Service struct {
	st     *store.Store
	driver Driver
	log    *slog.Logger
	opts   Options

	mu sync.Mutex
	// lastLogin is the last login state on the bus, by harness.
	lastLogin map[string]string
	// watching marks the harnesses whose login a goroutine watches.
	watching map[string]bool
}

// New builds a harness service. driver may be nil only in tests that never probe.
func New(st *store.Store, driver Driver, opts Options) *Service {
	return &Service{
		st:     st,
		driver: driver,
		log:    slog.Default().With("source", "harness"),
		opts:   opts,

		lastLogin: map[string]string{},
		watching:  map[string]bool{},
	}
}

// Caps returns declared capabilities for name.
func (s *Service) Caps(name string) AuthCaps {
	if s.driver == nil {
		return AuthCaps{}
	}
	return s.driver.AuthCaps(name)
}

// Probe runs the ladder and stamps. source is recorded on the bus attrs.
func (s *Service) Probe(ctx context.Context, name, source string) (AuthStatus, error) {
	if s.driver == nil {
		return AuthStatus{Tool: name, Status: AuthUnknown}, fmt.Errorf("no auth driver")
	}
	st := s.driver.ProbeAuth(ctx, name)
	st.Tool = name
	if st.Mode == "" {
		st.Mode = currentMode(name)
	}
	st.ProbedAt = time.Now().UTC()
	res, err := s.Stamp(ctx, st, source)
	if err != nil {
		return st, err
	}
	if !st.OK() && source != SourceLogout && (res.Flipped || !res.HadRow) {
		if _, err := s.st.Append(ctx, store.BusEvent{
			Topic: "auth.probe.failed", Entity: "harness", EntityID: name, Action: "updated",
			Attrs: map[string]string{"harness": name, "source": source, "status": st.Status},
		}); err != nil {
			s.log.Warn("auth.probe.failed", "error", err)
		}
	}
	return st, nil
}

// GetState returns the stored stamp (zero State if no row).
func (s *Service) GetState(ctx context.Context, name string) (State, error) {
	var st State
	st.Name = name
	err := s.st.DB().QueryRowContext(ctx, `
		SELECT auth_ok, COALESCE(auth_status,''), COALESCE(auth_detail,''),
			COALESCE(auth_probed_at,''), COALESCE(auth_expires_at,''), COALESCE(auth_account,''), updated_at
		FROM harness_state WHERE name=?`, name).
		Scan(&st.AuthOK, &st.Status, &st.Detail, &st.ProbedAt, &st.ExpiresAt, &st.Account, &st.UpdatedAt)
	if err == sql.ErrNoRows {
		return st, nil
	}
	return st, err
}

// States returns every harness_state row keyed by name.
func (s *Service) States(ctx context.Context) (map[string]State, error) {
	rows, err := s.st.DB().QueryContext(ctx, `
		SELECT name, auth_ok, COALESCE(auth_status,''), COALESCE(auth_detail,''),
			COALESCE(auth_probed_at,''), COALESCE(auth_expires_at,''), COALESCE(auth_account,''), updated_at
		FROM harness_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]State{}
	for rows.Next() {
		var st State
		if err := rows.Scan(&st.Name, &st.AuthOK, &st.Status, &st.Detail, &st.ProbedAt, &st.ExpiresAt, &st.Account, &st.UpdatedAt); err != nil {
			return nil, err
		}
		out[st.Name] = st
	}
	return out, rows.Err()
}

// Stamp writes harness_state auth columns. It emits harness.updated when the
// status, the detail, the expiry, or the account changes.
func (s *Service) Stamp(ctx context.Context, status AuthStatus, source string) (StampResult, error) {
	var res StampResult
	if status.Tool == "" {
		return res, nil
	}
	if status.Mode == "" {
		status.Mode = currentMode(status.Tool)
	}
	if status.ProbedAt.IsZero() {
		status.ProbedAt = time.Now().UTC()
	}
	res.OK = status.OK()
	flag := 0
	if res.OK {
		flag = 1
	}
	var expires any
	expiresStr := ""
	if status.ExpiresAt != nil && !status.ExpiresAt.IsZero() {
		expiresStr = status.ExpiresAt.UTC().Format(time.RFC3339Nano)
		expires = expiresStr
	}

	var prev sql.NullInt64
	var prevStatus, prevDetail, prevExpires, prevAccount string
	err := s.st.DB().QueryRowContext(ctx, `
		SELECT auth_ok, COALESCE(auth_status,''), COALESCE(auth_detail,''),
			COALESCE(auth_expires_at,''), COALESCE(auth_account,'')
		FROM harness_state WHERE name=?`, status.Tool).
		Scan(&prev, &prevStatus, &prevDetail, &prevExpires, &prevAccount)
	switch {
	case err == nil:
		res.HadRow = prev.Valid
		res.PrevOK = prev.Valid && prev.Int64 == 1
	case err == sql.ErrNoRows:
		// first stamp
	default:
		return res, err
	}
	res.Flipped = !res.HadRow || res.PrevOK != res.OK
	res.Changed = res.Flipped || prevStatus != status.Status || prevDetail != status.Detail ||
		prevExpires != expiresStr || prevAccount != status.Account

	now := time.Now().UTC().Format(time.RFC3339Nano)
	probed := status.ProbedAt.UTC().Format(time.RFC3339Nano)
	_, err = s.st.DB().ExecContext(ctx, `INSERT INTO harness_state
		(name, auth_ok, auth_status, auth_detail, auth_probed_at, auth_expires_at, auth_account, updated_at)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET
			auth_ok=excluded.auth_ok,
			auth_status=excluded.auth_status,
			auth_detail=excluded.auth_detail,
			auth_probed_at=excluded.auth_probed_at,
			auth_expires_at=excluded.auth_expires_at,
			auth_account=excluded.auth_account,
			updated_at=excluded.updated_at`,
		status.Tool, flag, status.Status, status.Detail, probed, expires, store.NullStr(status.Account), now)
	if err != nil {
		return res, err
	}
	if !res.Changed {
		return res, nil
	}
	attrs := map[string]string{
		"harness":     status.Tool,
		"auth_ok":     fmt.Sprintf("%t", res.OK),
		"auth_status": status.Status,
		"auth_detail": status.Detail,
		"account":     status.Account,
		"source":      source,
		"mode":        status.Mode,
	}
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "harness.updated", Entity: "harness", EntityID: status.Tool, Action: "updated",
		Attrs: attrs,
	}); err != nil {
		return res, err
	}
	return res, nil
}

// SetMode persists tools.<name>.auth, gives it to the visor, and re-probes.
func (s *Service) SetMode(ctx context.Context, name, mode string) (AuthStatus, error) {
	if err := s.persistMode(ctx, name, mode); err != nil {
		return AuthStatus{}, err
	}
	return s.Probe(ctx, name, SourceProbe)
}

// Logout clears credentials. shared requires confirm.
func (s *Service) Logout(ctx context.Context, name, mode string, confirm bool) (AuthStatus, error) {
	if !Known(name) {
		return AuthStatus{}, fmt.Errorf("unknown harness %q", name)
	}
	if mode == "" {
		mode = currentMode(name)
	}
	if mode != AuthModeShared && mode != AuthModeSeparate {
		return AuthStatus{}, fmt.Errorf("auth mode must be shared or separate")
	}
	if mode == AuthModeShared && !confirm {
		return AuthStatus{}, errSharedLogout
	}
	if mode == AuthModeSeparate {
		if err := RemoveSecretEnv(name); err != nil {
			return AuthStatus{}, err
		}
	}
	// The visor logs out in the login home of its auth mode.
	if _, err := s.persistModeIfChanged(ctx, name, mode); err != nil {
		return AuthStatus{}, err
	}
	v, err := s.visor(ctx)
	if err != nil {
		return AuthStatus{}, err
	}
	if err := v.LogoutHarness(ctx, name); err != nil {
		return AuthStatus{}, err
	}
	st := AuthStatus{
		Tool: name, Mode: mode, Status: AuthMissing,
		Hint: "trebi harness auth login " + name, Detail: "logout",
		ProbedAt: time.Now().UTC(),
	}
	_, err = s.Stamp(ctx, st, SourceLogout)
	return st, err
}

// ErrSharedLogout is returned when shared logout is missing confirm.
var errSharedLogout = fmt.Errorf("shared logout affects interactive CLI — re-run with confirm")

// ErrSharedLogout reports the confirm-required error (API maps to 400 validation).
func ErrSharedLogout() error { return errSharedLogout }

// LoginToken stores an API key, forces separate mode (except for an OpenCode
// provider key, which goes in the active auth.json), and probes.
func (s *Service) LoginToken(ctx context.Context, name string, in TokenLogin) (AuthStatus, error) {
	if !Known(name) {
		return AuthStatus{}, fmt.Errorf("unknown harness %q", name)
	}
	in.Token = trimSpace(in.Token)
	if in.Token == "" {
		return AuthStatus{}, fmt.Errorf("empty token")
	}
	if in.Provider == "" {
		if err := s.persistMode(ctx, name, AuthModeSeparate); err != nil {
			return AuthStatus{}, err
		}
	}
	if s.driver != nil {
		if err := s.driver.LoginAPIKey(ctx, name, in); err != nil {
			if in.Provider != "" {
				return AuthStatus{}, err
			}
			// Fall through to writing the env file ourselves.
			s.log.Debug("vendor api-key login", "harness", name, "error", err)
			if err := writeTokenEnv(name, in.Token); err != nil {
				return AuthStatus{}, err
			}
		}
	} else if err := writeTokenEnv(name, in.Token); err != nil {
		return AuthStatus{}, err
	}
	st, err := s.Probe(ctx, name, SourceLogin)
	if err != nil {
		return st, err
	}
	if st.Status != AuthOK {
		return st, fmt.Errorf("login completed but auth status is %s", st.Status)
	}
	return st, nil
}

func writeTokenEnv(name, token string) error {
	key := TokenEnvKey(name)
	if key == "" {
		return fmt.Errorf("no token env key for %s", name)
	}
	return WriteSecretEnv(name, map[string]string{key: token})
}

// TokenEnvKey maps a harness to its primary API-key env var.
func TokenEnvKey(tool string) string {
	switch tool {
	case "claude":
		return "ANTHROPIC_API_KEY"
	case "grok":
		return "XAI_API_KEY"
	case "codex":
		return "OPENAI_API_KEY"
	case "copilot":
		return "COPILOT_GITHUB_TOKEN"
	case "cursor":
		return "CURSOR_API_KEY"
	case "gemini":
		return "GEMINI_API_KEY"
	case "pi":
		return "ANTHROPIC_API_KEY"
	default:
		return ""
	}
}

// StoreOAuthToken writes a long-lived OAuth token (claude setup-token) into secret env.
func StoreOAuthToken(tool, key, token string) error {
	env, err := LoadSecretEnv(tool)
	if err != nil {
		return err
	}
	if env == nil {
		env = map[string]string{}
	}
	env[key] = token
	return WriteSecretEnv(tool, env)
}

func currentMode(name string) string {
	cfg, err := config.Load()
	if err != nil {
		return AuthModeShared
	}
	return AuthModeFromConfig(cfg, name)
}

func trimSpace(s string) string {
	i, j := 0, len(s)
	for i < j && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t' || s[j-1] == '\n' || s[j-1] == '\r') {
		j--
	}
	return s[i:j]
}

// EarliestExpiry returns the soonest stored auth_expires_at among harnesses,
// or zero if none.
func (s *Service) EarliestExpiry(ctx context.Context) (name string, at time.Time, err error) {
	rows, err := s.st.DB().QueryContext(ctx, `
		SELECT name, auth_expires_at FROM harness_state
		WHERE auth_expires_at IS NOT NULL AND auth_expires_at != ''`)
	if err != nil {
		return "", time.Time{}, err
	}
	defer rows.Close()
	var soonest time.Time
	var soonestName string
	for rows.Next() {
		var n, raw string
		if err := rows.Scan(&n, &raw); err != nil {
			return "", time.Time{}, err
		}
		t := store.ParseTS(raw)
		if t.IsZero() {
			continue
		}
		if soonest.IsZero() || t.Before(soonest) {
			soonest = t
			soonestName = n
		}
	}
	return soonestName, soonest, rows.Err()
}

// BlockedNames returns harnesses with auth_ok=0.
func (s *Service) BlockedNames(ctx context.Context) ([]string, error) {
	rows, err := s.st.DB().QueryContext(ctx, `SELECT name FROM harness_state WHERE auth_ok=0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// persistModeIfChanged persists mode when config.yaml has another one.
func (s *Service) persistModeIfChanged(ctx context.Context, name, mode string) (bool, error) {
	if currentMode(name) == mode {
		return false, nil
	}
	return true, s.persistMode(ctx, name, mode)
}

// persistMode writes tools.<name>.auth and gives the mode to the visor,
// which builds the harness home for it.
func (s *Service) persistMode(ctx context.Context, name, mode string) error {
	if mode != AuthModeShared && mode != AuthModeSeparate {
		return fmt.Errorf("auth mode must be shared or separate")
	}
	if !Known(name) {
		return fmt.Errorf("unknown harness %q", name)
	}
	cfg, err := config.Load()
	if err != nil {
		cfg = &config.Config{}
	}
	pin := cfg.Tools.Pin(name)
	if pin == nil {
		pin = &config.ToolPin{}
	}
	pin.Auth = mode
	cfg.Tools.SetPin(name, pin)
	if err := cfg.Save(); err != nil {
		return err
	}
	v, err := s.visor(ctx)
	if errors.Is(err, ErrNoVisor) {
		return nil
	}
	if err == nil {
		_, err = v.SetHarnessAuth(ctx, name, agentvisor.AuthMode(mode))
	}
	if err != nil {
		return fmt.Errorf("auth mode of %s: %w", name, err)
	}
	return nil
}

// LocalOnlyDetails is the typed 422 payload for a remote login refusal.
func LocalOnlyDetails(name string, caps AuthCaps) map[string]any {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "the daemon machine"
	}
	cmd := caps.LocalCmd
	if cmd == "" {
		cmd = name + " login"
	}
	hint := "run it on " + host
	if caps.HasLoginMode(LoginToken) {
		hint += ", or POST {token} with a " + TokenEnvKey(name)
	}
	return map[string]any{
		"login_modes": caps.LoginModes,
		"local_cmd":   cmd,
		"hint":        hint,
	}
}
