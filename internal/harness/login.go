package harness

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/store"
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

// doneRetain is how long GET /login still answers for an ended session.
const doneRetain = 60 * time.Second

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
	endedAt   time.Time
	finishing bool // verifyAndFinish runs once per session
	cancel    context.CancelFunc
}

// LoginSessionName is the tmux session for an interactive login.
func LoginSessionName(name string) string { return "trebi-login-" + name }

// StartLogin begins (or returns) the interactive login session.
func (s *Service) StartLogin(ctx context.Context, name, mode, provider string) (LoginSession, error) {
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
	if cur, ok := s.logins[name]; ok && !cur.ended() {
		out := cur.wire()
		s.mu.Unlock()
		return out, nil
	}
	s.mu.Unlock()

	cmd, env, err := s.driver.LoginCommand(name, mode, provider)
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
	s.log.Info("harness.login.updated", "harness", name, "phase", PhaseStarting, "session", sess)
	s.publishLogin(ctx, ls)

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
	if !ok || ls.ended() {
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
	s.publishLogin(ctx, ls)
	return out, nil
}

// ErrNoLogin is a 409: POST {code} with no active session.
var errNoLogin = fmt.Errorf("no active login session")

// ErrNoLogin returns the sentinel.
func ErrNoLogin() error { return errNoLogin }

// CancelLogin kills the login session and forgets it.
func (s *Service) CancelLogin(name string) {
	s.finishLogin(name, PhaseFailed, "cancelled", "")
	s.mu.Lock()
	delete(s.logins, name)
	s.mu.Unlock()
}

// CredChanged is called by the credential watcher. When a login for name is
// active and the probe passes, it finishes the login. A failed probe keeps the
// session: a logout also changes the files.
func (s *Service) CredChanged(name string) {
	s.mu.Lock()
	ls, ok := s.logins[name]
	if !ok || ls.ended() || ls.finishing {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.loginWG.Add(1)
	go func() {
		defer s.loginWG.Done()
		s.verifyAndFinish(name, false)
	}()
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
			done := ok && (ls.ended() || ls.finishing)
			s.mu.Unlock()
			if !done {
				s.finishLogin(name, PhaseFailed, "The login took too long. Start it again.", "")
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
	if !ok || ls.ended() {
		s.mu.Unlock()
		return true
	}
	// A probe runs; it may keep the session.
	if ls.finishing {
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()
	screen, err := s.driver.ReadScreen(ls.tmux)
	if err != nil {
		if !s.driver.HasSession(ls.tmux) {
			s.verifyAndFinish(name, true)
			return true
		}
		return false
	}
	ex := s.driver.ExtractLogin(name, screen)
	s.mu.Lock()
	before := ls.progress()
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
	changed := ls.progress() != before
	phase := ls.phase
	caps := s.driver.AuthCaps(name)
	s.mu.Unlock()
	if changed {
		s.publishLogin(context.Background(), ls)
	}

	if ex.Error != "" {
		s.finishLogin(name, PhaseFailed, ex.Error, ex.Hint)
		return true
	}
	if !s.driver.HasSession(ls.tmux) {
		s.verifyAndFinish(name, true)
		return true
	}
	if caps.RelayFragile && !ex.NoExtract && ls.url == "" && time.Now().After(extractDeadline) &&
		phase == PhaseStarting {
		s.failLocal(name, caps)
		return true
	}
	return false
}

// verifyAndFinish probes and ends the session as done. On a failed probe it
// ends the session as failed only when final is true.
func (s *Service) verifyAndFinish(name string, final bool) {
	s.mu.Lock()
	ls, ok := s.logins[name]
	if !ok || ls.ended() || ls.finishing {
		s.mu.Unlock()
		return
	}
	ls.finishing = true
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := s.Probe(ctx, name, SourceLogin)
	if err != nil || st.Status != AuthOK {
		if !final {
			s.mu.Lock()
			ls.finishing = false
			s.mu.Unlock()
			return
		}
		msg := "The login did not finish."
		if err != nil {
			msg = err.Error()
		} else if st.Detail != "" {
			msg = st.Detail
		}
		s.finishLogin(name, PhaseFailed, msg, "")
		return
	}
	if err := ClearLoginEnded(name); err != nil {
		s.log.Warn("clear login-ended marker", "harness", name, "error", err)
	}
	s.finishLogin(name, PhaseDone, "", "")
}

func (s *Service) failLocal(name string, caps AuthCaps) {
	d := LocalOnlyDetails(name, caps)
	s.mu.Lock()
	ls, ok := s.logins[name]
	if ok {
		ls.phase = PhaseFailed
		ls.err = name + " login needs a browser on the daemon machine"
		ls.localCmd, _ = d["local_cmd"].(string)
		ls.hint, _ = d["hint"].(string)
		ls.endedAt = time.Now().UTC()
		if ls.cancel != nil {
			ls.cancel()
		}
	}
	s.mu.Unlock()
	if s.driver != nil {
		_ = s.driver.KillSession(LoginSessionName(name))
	}
	if ok {
		s.publishLogin(context.Background(), ls)
		s.expireLogin(name, ls)
	}
}

func (s *Service) finishLogin(name, phase, errMsg, hint string) {
	s.mu.Lock()
	ls, ok := s.logins[name]
	if ok {
		ls.phase = phase
		ls.err = errMsg
		if hint != "" {
			ls.hint = hint
		}
		ls.endedAt = time.Now().UTC()
		if ls.cancel != nil {
			ls.cancel()
		}
	}
	s.mu.Unlock()
	if s.driver != nil {
		_ = s.driver.KillSession(LoginSessionName(name))
	}
	if !ok {
		return
	}
	s.log.Info("harness.login.updated", "harness", name, "phase", phase, "error", errMsg)
	s.publishLogin(context.Background(), ls)
	s.expireLogin(name, ls)
}

// expireLogin drops an ended session after doneRetain, unless a new login
// replaced it.
func (s *Service) expireLogin(name string, ls *loginSession) {
	time.AfterFunc(doneRetain, func() {
		s.mu.Lock()
		if cur, ok := s.logins[name]; ok && cur == ls {
			delete(s.logins, name)
		}
		s.mu.Unlock()
	})
}

// publishLogin puts the login progress on the bus. screen_tail stays in
// GET /login, because it is too large for the bus.
func (s *Service) publishLogin(ctx context.Context, ls *loginSession) {
	s.mu.Lock()
	attrs := map[string]string{
		"harness":   ls.name,
		"phase":     ls.phase,
		"url":       ls.url,
		"user_code": ls.userCode,
		"error":     ls.err,
		"hint":      ls.hint,
	}
	s.mu.Unlock()
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: "harness.login.updated", Entity: "harness", EntityID: ls.name, Action: "updated",
		Attrs: attrs,
	}); err != nil {
		s.log.Warn("harness.login.updated", "harness", ls.name, "error", err)
	}
}

func (ls *loginSession) ended() bool { return ls.phase == PhaseDone || ls.phase == PhaseFailed }

// progress is the part of a session that the bus carries.
func (ls *loginSession) progress() string {
	return ls.phase + "\x00" + ls.url + "\x00" + ls.userCode
}

func (ls *loginSession) wire() LoginSession {
	active := !ls.ended()
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
