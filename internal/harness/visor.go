package harness

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	agentvisor "github.com/trebi-ai/agent-visor"
	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/store"
)

// Visor is the harness part of agent-visor that trebi uses.
// *agentvisor.Visor and *agentvisor.Client both have it.
type Visor interface {
	Harness(ctx context.Context, name string) (agentvisor.Harness, error)
	InstallHarness(ctx context.Context, name, version string) (agentvisor.Harness, error)
	UpdateHarness(ctx context.Context, name, version string) (agentvisor.Harness, error)
	SetHarnessAuth(ctx context.Context, name string, mode agentvisor.AuthMode) (agentvisor.Harness, error)
	SetHarnessUpdates(ctx context.Context, name string, mode agentvisor.UpdateMode, version string) (agentvisor.Harness, error)
	CheckHarnesses(ctx context.Context) error
	LoginHarness(ctx context.Context, name string, spec agentvisor.LoginSpec) (agentvisor.Login, error)
	HarnessLogin(ctx context.Context, name string) (agentvisor.Login, error)
	HarnessLoginCode(ctx context.Context, name, code string) (agentvisor.Login, error)
	CancelHarnessLogin(ctx context.Context, name string) error
	LogoutHarness(ctx context.Context, name string) error
	HarnessFiles(ctx context.Context, name string) ([]agentvisor.HomeFile, error)
	ShareHarnessFile(ctx context.Context, name, key string) (agentvisor.HomeFile, error)
	OwnHarnessFile(ctx context.Context, name, key string, start agentvisor.OwnStart) (agentvisor.HomeFile, error)
	ReadHarnessFile(ctx context.Context, name, key, side string) (agentvisor.FileContent, error)
	WriteHarnessFile(ctx context.Context, name, key, content, ifMatch string) (agentvisor.FileContent, error)
}

// ErrNoVisor means that no visor is set (tests).
var ErrNoVisor = errors.New("no agent-visor for the harnesses")

// Update modes of tools.<name>.updates.
const (
	UpdatesAuto = string(agentvisor.UpdatesAuto)
	UpdatesKeep = string(agentvisor.UpdatesKeep)
)

// Login phases. They are the agent-visor phases.
const (
	PhaseStarting        = string(agentvisor.LoginStarting)
	PhaseAwaitingBrowser = string(agentvisor.LoginBrowser)
	PhaseAwaitingCode    = string(agentvisor.LoginCode)
	PhaseVerifying       = string(agentvisor.LoginVerifying)
	PhaseDone            = string(agentvisor.LoginDone)
	PhaseFailed          = string(agentvisor.LoginFailed)
)

// LoginSession is the wire shape of GET /login.
type LoginSession struct {
	Active     bool      `json:"active"`
	Phase      string    `json:"phase"`
	URL        string    `json:"url,omitempty"`
	UserCode   string    `json:"user_code,omitempty"`
	AwaitCode  bool      `json:"await_code,omitempty"`
	ScreenTail string    `json:"screen_tail,omitempty"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	Error      string    `json:"error,omitempty"`
	Hint       string    `json:"hint,omitempty"`
	// Session is the terminal session of the login. A client can attach.
	Session string `json:"session,omitempty"`
}

// errLocalOnly refuses a login that needs a browser on this machine.
type errLocalOnly struct {
	name string
	caps AuthCaps
}

func (e errLocalOnly) Error() string {
	return e.name + " login needs a browser on the daemon machine"
}

// IsLocalOnly reports a typed local_only refusal.
func IsLocalOnly(err error) (AuthCaps, bool) {
	var e errLocalOnly
	if !errors.As(err, &e) {
		return AuthCaps{}, false
	}
	return e.caps, true
}

func loginWire(l agentvisor.Login) LoginSession {
	return LoginSession{
		Active: !l.Done && l.Phase != agentvisor.LoginDone && l.Phase != agentvisor.LoginFailed,
		Phase:  string(l.Phase), URL: l.URL, UserCode: l.Code, AwaitCode: l.AwaitCode,
		ScreenTail: l.Screen, StartedAt: l.StartedAt, Error: l.Error, Hint: l.Hint, Session: l.Session,
	}
}

// FileRow is one config file of a harness.
type FileRow struct {
	Key                 string     `json:"key"`
	Label               string     `json:"label"`
	Format              string     `json:"format"`
	Mode                string     `json:"mode"` // yours | own
	Inherit             string     `json:"inherit"`
	UserPath            string     `json:"user_path"`
	TrebiPath           string     `json:"trebi_path"`
	UserExists          bool       `json:"user_exists"`
	UserModifiedAt      *time.Time `json:"user_modified_at,omitempty"`
	OwnCreatedAt        *time.Time `json:"own_created_at,omitempty"`
	UserChangedSinceOwn bool       `json:"user_changed_since_own"`
	BackupUntil         *time.Time `json:"backup_until,omitempty"`
}

// ErrUnknownFile is a file key that the harness spec does not list.
var ErrUnknownFile = errors.New("unknown config file")

func (s *Service) visor(ctx context.Context) (Visor, error) {
	if s.opts.Visor == nil {
		return nil, ErrNoVisor
	}
	return s.opts.Visor(ctx)
}

// Sync gives the visor the auth mode and the update mode of each harness in
// config.yaml. It builds the harness homes. The daemon calls it at start.
func (s *Service) Sync(ctx context.Context) error {
	v, err := s.visor(ctx)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range AllNames(cfg) {
		h, err := v.Harness(ctx, name)
		if errors.Is(err, agentvisor.ErrNotFound) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		// SetHarnessAuth also builds the home, so it runs each time.
		if _, err := v.SetHarnessAuth(ctx, name, agentvisor.AuthMode(AuthModeFromConfig(cfg, name))); err != nil {
			errs = append(errs, fmt.Errorf("auth mode of %s: %w", name, err))
		}
		if h.Base != "" {
			continue // a variant uses the update mode of its base
		}
		if mode, version := updatesFromConfig(cfg, name); mode != h.Updates {
			if _, err := v.SetHarnessUpdates(ctx, name, mode, version); err != nil {
				errs = append(errs, fmt.Errorf("update mode of %s: %w", name, err))
			}
		}
	}
	return errors.Join(errs...)
}

func updatesFromConfig(cfg *config.Config, name string) (agentvisor.UpdateMode, string) {
	if p := cfg.Tools.Pin(name); p != nil && p.Updates == UpdatesKeep {
		return agentvisor.UpdatesKeep, p.Version
	}
	return agentvisor.UpdatesAuto, ""
}

// Version gives the version state of a harness from the visor.
func (s *Service) Version(ctx context.Context, name string) (agentvisor.Harness, error) {
	v, err := s.visor(ctx)
	if err != nil {
		return agentvisor.Harness{}, err
	}
	return v.Harness(ctx, name)
}

// Updating reports that the visor updates the harness now.
func (s *Service) Updating(ctx context.Context, name string) bool {
	h, err := s.Version(ctx, name)
	return err == nil && (h.Updating || h.Pending)
}

// Install installs a harness, or the version given.
func (s *Service) Install(ctx context.Context, name, version string) (agentvisor.Harness, error) {
	v, err := s.visor(ctx)
	if err != nil {
		return agentvisor.Harness{}, err
	}
	h, err := v.InstallHarness(ctx, name, version)
	if err == nil {
		err = s.PublishUpdated(ctx, name, map[string]string{"version": h.Version})
	}
	return h, err
}

// Update updates a harness now. A live session makes it wait; the visor
// then gives agentvisor.ErrBusy and updates at the end of the session.
func (s *Service) Update(ctx context.Context, name string) (agentvisor.Harness, error) {
	v, err := s.visor(ctx)
	if err != nil {
		return agentvisor.Harness{}, err
	}
	h, err := v.UpdateHarness(ctx, name, "")
	if errors.Is(err, agentvisor.ErrBusy) {
		return h, err
	}
	if err != nil {
		if h, herr := v.Harness(ctx, name); herr == nil {
			h.Pending = false
			_ = s.PublishUpdated(ctx, name, updateAttrs(h))
		}
		return h, err
	}
	return h, s.PublishUpdated(ctx, name, updateAttrs(h))
}

// updateAttrs gives the harness.updated attrs of the last version change.
func updateAttrs(h agentvisor.Harness) map[string]string {
	a := map[string]string{"version": h.Version, "latest_version": h.Latest}
	if n := len(h.History); n > 0 {
		u := h.History[n-1]
		a["update_ok"] = fmt.Sprint(u.OK)
		a["update_from"], a["update_to"], a["update_error"] = u.From, u.To, u.Error
	}
	return a
}

// CheckVersions looks for new releases of the harnesses.
func (s *Service) CheckVersions(ctx context.Context) error {
	v, err := s.visor(ctx)
	if err != nil {
		return err
	}
	return v.CheckHarnesses(ctx)
}

// SetUpdates sets the update mode of a harness in the visor.
func (s *Service) SetUpdates(ctx context.Context, name, mode, version string) error {
	v, err := s.visor(ctx)
	if err != nil {
		return err
	}
	_, err = v.SetHarnessUpdates(ctx, name, agentvisor.UpdateMode(mode), version)
	return err
}

// StartLogin starts a vendor login in the visor, or gives the one that runs.
// A mode sets the auth mode first.
func (s *Service) StartLogin(ctx context.Context, name, mode, provider string) (LoginSession, error) {
	if !Known(name) {
		return LoginSession{}, fmt.Errorf("unknown harness %q", name)
	}
	if s.driver != nil {
		if caps := s.driver.AuthCaps(name); !caps.HasLoginMode(LoginRelay) {
			return LoginSession{}, errLocalOnly{name: name, caps: caps}
		}
	}
	if mode != "" {
		if err := s.persistMode(ctx, name, mode); err != nil {
			return LoginSession{}, err
		}
	}
	v, err := s.visor(ctx)
	if err != nil {
		return LoginSession{}, err
	}
	l, err := v.LoginHarness(ctx, name, agentvisor.LoginSpec{Provider: provider})
	if err != nil {
		return LoginSession{}, err
	}
	s.loginChanged(ctx, name, l)
	s.watchLogin(ctx, v, name)
	return loginWire(l), nil
}

// loginMaxWatch stops a login watch that does not end. The visor stops a
// login after 10 minutes.
const loginMaxWatch = 15 * time.Minute

// watchLogin follows the login state in the visor and puts each change on
// the bus. The goroutine stops when the login ends, when the visor has no
// login, or after loginMaxWatch.
func (s *Service) watchLogin(ctx context.Context, v Visor, name string) {
	s.mu.Lock()
	if s.watching[name] {
		s.mu.Unlock()
		return
	}
	s.watching[name] = true
	s.mu.Unlock()
	every := cmp.Or(s.opts.LoginPoll, 500*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loginMaxWatch)
	go func() {
		defer cancel()
		defer func() {
			s.mu.Lock()
			delete(s.watching, name)
			s.mu.Unlock()
		}()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			l, err := v.HarnessLogin(ctx, name)
			if err != nil {
				return
			}
			s.loginChanged(ctx, name, l)
			if loginEnded(l) {
				return
			}
		}
	}()
}

// loginChanged puts a new login state on the bus as harness.login.updated.
// The first reader of the end probes the auth again, which publishes
// harness.updated.
func (s *Service) loginChanged(ctx context.Context, name string, l agentvisor.Login) {
	if l.Phase == "" {
		return
	}
	attrs := map[string]string{
		"harness": name, "phase": string(l.Phase), "url": l.URL, "user_code": l.Code,
		"error": l.Error, "hint": l.Hint, "session": l.Session,
	}
	key := strings.Join([]string{l.StartedAt.String(), attrs["phase"], l.URL, l.Code, l.Error, l.Hint, l.Session}, "\x00")
	s.mu.Lock()
	same := s.lastLogin[name] == key
	s.lastLogin[name] = key
	s.mu.Unlock()
	if same {
		return
	}
	s.log.Info("harness.login.updated", "harness", name, "phase", l.Phase, "error", l.Error, "session", l.Session)
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "harness.login.updated", Entity: "harness", EntityID: name, Action: "updated", Attrs: attrs,
	}); err != nil {
		s.log.Warn("harness.login.updated", "harness", name, "err", err)
	}
	if loginEnded(l) {
		if _, err := s.Probe(ctx, name, SourceLogin); err != nil {
			s.log.Warn("probe after login", "harness", name, "err", err)
		}
	}
}

func loginEnded(l agentvisor.Login) bool {
	return l.Done || l.Phase == agentvisor.LoginDone || l.Phase == agentvisor.LoginFailed
}

// GetLogin gives the login of a harness. The end of a login probes the
// auth again, which publishes harness.updated.
func (s *Service) GetLogin(ctx context.Context, name string) LoginSession {
	v, err := s.visor(ctx)
	if err != nil {
		return LoginSession{Phase: PhaseFailed}
	}
	l, err := v.HarnessLogin(ctx, name)
	if err != nil {
		return LoginSession{}
	}
	s.loginChanged(ctx, name, l)
	return loginWire(l)
}

// SubmitCode types a pasted code into the login.
func (s *Service) SubmitCode(ctx context.Context, name, code string) (LoginSession, error) {
	v, err := s.visor(ctx)
	if err != nil {
		return LoginSession{}, err
	}
	l, err := v.HarnessLoginCode(ctx, name, code)
	if err == nil {
		s.loginChanged(ctx, name, l)
	}
	return loginWire(l), err
}

// CancelLogin stops the login of a harness.
func (s *Service) CancelLogin(ctx context.Context, name string) error {
	v, err := s.visor(ctx)
	if err != nil {
		return err
	}
	if err := v.CancelHarnessLogin(ctx, name); err != nil {
		return err
	}
	if l, err := v.HarnessLogin(ctx, name); err == nil {
		s.loginChanged(ctx, name, l)
	}
	return nil
}

// Files lists the config files of a harness that the user can let Trebi own.
func (s *Service) Files(ctx context.Context, name string) ([]FileRow, error) {
	spec, ok := SpecFor(name)
	if !ok {
		return nil, fmt.Errorf("unknown harness %q", name)
	}
	if len(spec.Files) == 0 {
		return []FileRow{}, nil
	}
	v, err := s.visor(ctx)
	if err != nil {
		return nil, err
	}
	files, err := v.HarnessFiles(ctx, name)
	if err != nil {
		return nil, err
	}
	byKey := map[string]agentvisor.HomeFile{}
	for _, f := range files {
		byKey[f.Key] = f
	}
	rows := make([]FileRow, 0, len(spec.Files))
	for _, cf := range spec.Files {
		f, ok := byKey[cf.Key]
		if !ok {
			continue
		}
		rows = append(rows, fileRow(cf, f))
	}
	return rows, nil
}

func fileRow(cf ConfigFile, f agentvisor.HomeFile) FileRow {
	mode := "yours"
	if !f.Shared {
		mode = "own"
	}
	return FileRow{
		Key: cf.Key, Label: cf.Label, Format: cf.Format, Mode: mode, Inherit: cf.Inherit,
		UserPath: f.UserPath, TrebiPath: f.HomePath, UserExists: f.UserExists, UserModifiedAt: f.UserModAt,
		OwnCreatedAt: f.OwnSince, UserChangedSinceOwn: f.UserChanged, BackupUntil: f.BackupUntil,
	}
}

// OwnCount is the number of config files that Trebi owns for name.
func (s *Service) OwnCount(ctx context.Context, name string) int {
	rows, err := s.Files(ctx, name)
	if err != nil {
		return 0
	}
	n := 0
	for _, r := range rows {
		if r.Mode == "own" {
			n++
		}
	}
	return n
}

func (s *Service) configFile(name, key string) (ConfigFile, error) {
	spec, ok := SpecFor(name)
	if !ok {
		return ConfigFile{}, fmt.Errorf("unknown harness %q", name)
	}
	for _, f := range spec.Files {
		if f.Key == key {
			return f, nil
		}
	}
	return ConfigFile{}, fmt.Errorf("%w %q for %s", ErrUnknownFile, key, name)
}

// ReadFile reads one side of a config file: "user" or "trebi". The empty
// side is the one that a run uses.
func (s *Service) ReadFile(ctx context.Context, name, key, side string) (agentvisor.FileContent, error) {
	if _, err := s.configFile(name, key); err != nil {
		return agentvisor.FileContent{}, err
	}
	v, err := s.visor(ctx)
	if err != nil {
		return agentvisor.FileContent{}, err
	}
	switch side {
	case "":
		side = agentvisor.SideUser
		files, err := v.HarnessFiles(ctx, name)
		if err != nil {
			return agentvisor.FileContent{}, err
		}
		for _, f := range files {
			if f.Key == key && !f.Shared {
				side = agentvisor.SideHome
			}
		}
	case "user":
		side = agentvisor.SideUser
	case "trebi":
		side = agentvisor.SideHome
	default:
		return agentvisor.FileContent{}, fmt.Errorf("%w: side must be user or trebi", agentvisor.ErrInvalid)
	}
	return v.ReadHarnessFile(ctx, name, key, side)
}

// OwnFile makes Trebi's copy of a config file. start is user, empty, or
// backup.
func (s *Service) OwnFile(ctx context.Context, name, key, start string) error {
	if _, err := s.configFile(name, key); err != nil {
		return err
	}
	v, err := s.visor(ctx)
	if err != nil {
		return err
	}
	if _, err := v.OwnHarnessFile(ctx, name, key, agentvisor.OwnStart(cmp.Or(start, "user"))); err != nil {
		return err
	}
	return s.PublishUpdated(ctx, name, map[string]string{"files": "true"})
}

// WriteFile writes Trebi's copy of a config file. A stale ifMatch gives
// agentvisor.ErrStale with the current content.
func (s *Service) WriteFile(ctx context.Context, name, key, content, ifMatch string) (agentvisor.FileContent, error) {
	f, err := s.configFile(name, key)
	if err != nil {
		return agentvisor.FileContent{}, err
	}
	if err := checkSyntax(f.Format, content); err != nil {
		return agentvisor.FileContent{}, err
	}
	v, err := s.visor(ctx)
	if err != nil {
		return agentvisor.FileContent{}, err
	}
	fc, err := v.WriteHarnessFile(ctx, name, key, content, ifMatch)
	if err != nil {
		return fc, err
	}
	return fc, s.PublishUpdated(ctx, name, map[string]string{"files": "true"})
}

// ResetFile goes back to the user file. The visor keeps a backup of
// Trebi's copy.
func (s *Service) ResetFile(ctx context.Context, name, key string) error {
	if _, err := s.configFile(name, key); err != nil {
		return err
	}
	v, err := s.visor(ctx)
	if err != nil {
		return err
	}
	if _, err := v.ShareHarnessFile(ctx, name, key); err != nil {
		return err
	}
	return s.PublishUpdated(ctx, name, map[string]string{"files": "true"})
}

// ParseError is a JSON or TOML error at a line and column (1-based).
type ParseError struct {
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Msg    string `json:"-"`
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("line %d, column %d: %s", e.Line, e.Column, e.Msg)
}

// checkSyntax parses JSON and TOML. A markdown file is free text.
func checkSyntax(format, content string) error {
	switch format {
	case "json":
		if strings.TrimSpace(content) == "" {
			return nil
		}
		var v any
		err := json.Unmarshal([]byte(content), &v)
		var se *json.SyntaxError
		if errors.As(err, &se) {
			line, col := lineCol(content, int(se.Offset))
			return &ParseError{Line: line, Column: col, Msg: se.Error()}
		}
		if err != nil {
			return &ParseError{Line: 1, Column: 1, Msg: err.Error()}
		}
	case "toml":
		var v map[string]any
		_, err := toml.Decode(content, &v)
		var pe toml.ParseError
		if errors.As(err, &pe) {
			return &ParseError{Line: pe.Position.Line, Column: pe.Position.Col, Msg: pe.Message}
		}
		if err != nil {
			return &ParseError{Line: 1, Column: 1, Msg: err.Error()}
		}
	}
	return nil
}

// lineCol maps a byte offset to a 1-based line and column.
func lineCol(s string, off int) (line, col int) {
	if off > len(s) {
		off = len(s)
	}
	before := s[:off]
	line = strings.Count(before, "\n") + 1
	col = off - strings.LastIndex(before, "\n")
	return line, col
}
