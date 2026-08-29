package harness

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	config "github.com/trebi-ai/trebi/internal/config"
)

// Login phases for the interactive session state machine.
const (
	PhaseStarting        = "starting"
	PhaseAwaitingBrowser = "awaiting_browser"
	PhaseAwaitingCode    = "awaiting_code"
	PhaseVerifying       = "verifying"
	PhaseDone            = "done"
	PhaseFailed          = "failed"
)

// LoginSession is the wire shape of GET /login.
type LoginSession struct {
	Active     bool      `json:"active"`
	Phase      string    `json:"phase"`
	URL        string    `json:"url,omitempty"`
	UserCode   string    `json:"user_code,omitempty"`
	ScreenTail string    `json:"screen_tail,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	Error      string    `json:"error,omitempty"`
	LocalCmd   string    `json:"local_cmd,omitempty"`
	Hint       string    `json:"hint,omitempty"`
	LoginModes []string  `json:"login_modes,omitempty"`
}

type loginSession struct {
	name      string
	tmux      string
	phase     string
	url       string
	userCode  string
	tail      string
	err       string
	localCmd  string
	hint      string
	startedAt time.Time
	cancel    context.CancelFunc
}

// LoginSessionName is the tmux session for an interactive login.
func LoginSessionName(name string) string { return "trebi-login-" + name }

// StartLogin begins (or returns) the interactive login session.
func (s *Service) StartLogin(ctx context.Context, name, mode string) (LoginSession, error) {
	if !Known(name) {
		return LoginSession{}, fmt.Errorf("unknown harness %q", name)
	}
	if s.driver == nil {
		return LoginSession{}, fmt.Errorf("no auth driver")
	}
	caps := s.driver.AuthCaps(name)
	if !caps.HasLoginMode(LoginRelay) {
		return LoginSession{}, errLocalOnly{name: name, caps: caps}
	}
	if mode != "" && mode != AuthModeShared && mode != AuthModeSeparate {
		return LoginSession{}, fmt.Errorf("auth mode must be shared or separate")
	}
	if mode != "" {
		if err := s.persistMode(name, mode); err != nil {
			return LoginSession{}, err
		}
	} else {
		mode = currentMode(name)
	}

	s.mu.Lock()
	if cur, ok := s.logins[name]; ok && cur.phase != PhaseDone && cur.phase != PhaseFailed {
		out := cur.wire()
		s.mu.Unlock()
		return out, nil
	}
	s.mu.Unlock()

	cmd, env, err := s.driver.LoginCommand(name, mode)
	if err != nil {
		return LoginSession{}, err
	}
	sess := LoginSessionName(name)
	if s.driver.HasSession(sess) {
		_ = s.driver.KillSession(sess)
	}
	cwd := config.HarnessDir(name)
	_ = os.MkdirAll(cwd, 0755)
	if err := s.driver.NewNamedSession(sess, cwd); err != nil {
		return LoginSession{}, fmt.Errorf("tmux login session: %w", err)
	}
	if export := envExport(env); export != "" {
		_ = s.driver.Send(sess, export+"\n")
	}
	if cmd != "" {
		if err := s.driver.Send(sess, cmd+"\n"); err != nil {
			_ = s.driver.KillSession(sess)
			return LoginSession{}, err
		}
	}

	watchCtx, cancel := context.WithTimeout(context.Background(), s.opts.LoginMaxAge)
	ls := &loginSession{
		name: name, tmux: sess, phase: PhaseStarting,
		startedAt: time.Now().UTC(), cancel: cancel,
	}
	s.mu.Lock()
	s.logins[name] = ls
	s.mu.Unlock()
	s.log.Info("auth.login_started", "harness", name, "session", sess)

	s.loginWG.Add(1)
	go func() {
		defer s.loginWG.Done()
		s.watchLogin(watchCtx, name)
	}()
	return ls.wire(), nil
}

type errLocalOnly struct {
	name string
	caps AuthCaps
}

func (e errLocalOnly) Error() string {
	cmd := e.caps.LocalCmd
	if cmd == "" {
		cmd = e.name + " login"
	}
	return e.name + " login needs a browser on the daemon machine"
}

// IsLocalOnly reports a typed local_only refusal.
func IsLocalOnly(err error) (AuthCaps, bool) {
	e, ok := err.(errLocalOnly)
	if !ok {
		return AuthCaps{}, false
	}
	return e.caps, true
}

// GetLogin returns the in-memory session (active=false when none).
func (s *Service) GetLogin(name string) LoginSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ls, ok := s.logins[name]; ok {
		return ls.wire()
	}
	return LoginSession{Active: false}
}

// SubmitCode types the paste-back code into the login session.
func (s *Service) SubmitCode(ctx context.Context, name, code string) (LoginSession, error) {
	code = trimSpace(code)
	if code == "" {
		return LoginSession{}, fmt.Errorf("code required")
	}
	s.mu.Lock()
	ls, ok := s.logins[name]
	s.mu.Unlock()
	if !ok || ls.phase == PhaseDone || ls.phase == PhaseFailed {
		return LoginSession{}, errNoLogin
	}
	if err := s.driver.Send(ls.tmux, code); err != nil {
		return LoginSession{}, err
	}
	_ = s.driver.SendKey(ls.tmux, "enter")
	s.mu.Lock()
	ls.phase = PhaseVerifying
	out := ls.wire()
	s.mu.Unlock()
	return out, nil
}

// ErrNoLogin is a 409: POST {code} with no active session.
var errNoLogin = fmt.Errorf("no active login session")

// ErrNoLogin returns the sentinel.
func ErrNoLogin() error { return errNoLogin }

// CancelLogin kills the login session.
func (s *Service) CancelLogin(name string) {
	s.finishLogin(name, PhaseFailed, "cancelled")
}

// StopLoginWatchers waits for in-flight login goroutines (daemon shutdown).
func (s *Service) StopLoginWatchers() {
	s.mu.Lock()
	for _, ls := range s.logins {
		if ls.cancel != nil {
			ls.cancel()
		}
	}
	s.mu.Unlock()
	s.loginWG.Wait()
}

func (s *Service) watchLogin(ctx context.Context, name string) {
	tick := time.NewTicker(400 * time.Millisecond)
	defer tick.Stop()
	extractDeadline := time.Now().Add(extractWindow(s.opts.LoginMaxAge))
	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			ls, ok := s.logins[name]
			done := ok && (ls.phase == PhaseDone || ls.phase == PhaseFailed)
			s.mu.Unlock()
			if !done {
				s.finishLogin(name, PhaseFailed, "timeout")
			}
			return
		case <-tick.C:
			if s.tickLogin(name, extractDeadline) {
				return
			}
		}
	}
}

func extractWindow(maxAge time.Duration) time.Duration {
	w := 45 * time.Second
	if maxAge > 0 && maxAge/2 < w {
		w = maxAge / 2
	}
	if w < 200*time.Millisecond {
		w = 200 * time.Millisecond
	}
	return w
}

// tickLogin returns true when the session has reached a terminal state.
func (s *Service) tickLogin(name string, extractDeadline time.Time) bool {
	s.mu.Lock()
	ls, ok := s.logins[name]
	s.mu.Unlock()
	if !ok || ls.phase == PhaseDone || ls.phase == PhaseFailed {
		return true
	}
	screen, err := s.driver.ReadScreen(ls.tmux)
	if err != nil {
		if !s.driver.HasSession(ls.tmux) {
			s.verifyAndFinish(name)
			return true
		}
		return false
	}
	ex := s.driver.ExtractLogin(name, screen)
	s.mu.Lock()
	ls.tail = tail(screen, 80)
	if ex.URL != "" && ls.url == "" {
		ls.url = ex.URL
		if ls.phase == PhaseStarting {
			ls.phase = PhaseAwaitingBrowser
		}
	}
	if ex.UserCode != "" {
		ls.userCode = ex.UserCode
	}
	if ex.AwaitCode && ls.phase != PhaseVerifying && ls.phase != PhaseDone {
		ls.phase = PhaseAwaitingCode
	}
	if ex.Token != "" {
		// Relay capture stores the setup-token and leaves auth mode alone.
		// LoginToken (explicit API key) is the path that forces separate.
		_ = StoreOAuthToken(name, "CLAUDE_CODE_OAUTH_TOKEN", ex.Token)
	}
	phase := ls.phase
	caps := s.driver.AuthCaps(name)
	s.mu.Unlock()

	if !s.driver.HasSession(ls.tmux) {
		s.verifyAndFinish(name)
		return true
	}
	if caps.RelayFragile && !ex.NoExtract && ls.url == "" && time.Now().After(extractDeadline) &&
		phase == PhaseStarting {
		s.failLocal(name, caps)
		return true
	}
	return false
}

func (s *Service) verifyAndFinish(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := s.Probe(ctx, name, SourceLogin)
	if err != nil || st.Status != AuthOK {
		msg := "probe failed"
		if err != nil {
			msg = err.Error()
		} else {
			msg = "auth status is " + st.Status
		}
		s.finishLogin(name, PhaseFailed, msg)
		return
	}
	s.log.Info("auth.login_completed", "harness", name)
	s.finishLogin(name, PhaseDone, "")
}

func (s *Service) failLocal(name string, caps AuthCaps) {
	d := LocalOnlyDetails(name, caps)
	s.mu.Lock()
	if ls, ok := s.logins[name]; ok {
		ls.phase = PhaseFailed
		ls.err = name + " login needs a browser on the daemon machine"
		ls.localCmd, _ = d["local_cmd"].(string)
		ls.hint, _ = d["hint"].(string)
		if ls.cancel != nil {
			ls.cancel()
		}
	}
	s.mu.Unlock()
	if s.driver != nil {
		_ = s.driver.KillSession(LoginSessionName(name))
	}
}

func (s *Service) finishLogin(name, phase, errMsg string) {
	s.mu.Lock()
	ls, ok := s.logins[name]
	if ok {
		ls.phase = phase
		ls.err = errMsg
		if ls.cancel != nil {
			ls.cancel()
		}
	}
	s.mu.Unlock()
	if s.driver != nil {
		_ = s.driver.KillSession(LoginSessionName(name))
	}
	if phase == PhaseDone {
		// drop the session after a short moment so GET can still observe done
		time.AfterFunc(2*time.Second, func() {
			s.mu.Lock()
			if cur, ok := s.logins[name]; ok && cur.phase == PhaseDone {
				delete(s.logins, name)
			}
			s.mu.Unlock()
		})
	}
}

func (ls *loginSession) wire() LoginSession {
	active := ls.phase != PhaseDone && ls.phase != PhaseFailed
	return LoginSession{
		Active:     active,
		Phase:      ls.phase,
		URL:        ls.url,
		UserCode:   ls.userCode,
		ScreenTail: ls.tail,
		StartedAt:  ls.startedAt,
		Error:      ls.err,
		LocalCmd:   ls.localCmd,
		Hint:       ls.hint,
	}
}

func envExport(env []string) string {
	if len(env) == 0 {
		return ""
	}
	return "export " + strings.Join(env, " ")
}

func tail(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
