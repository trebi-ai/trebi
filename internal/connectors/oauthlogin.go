package connectors

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/oauth"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/tools"
)

// LoginOAuth is the id of the OAuth login that Trebi adds to an online MCP
// server with OAuth settings.
const LoginOAuth = "oauth"

// ReturnPath is the path of the login return page, on this computer and on
// Trebi Cloud.
const ReturnPath = "/login/done"

const (
	// oauthWait bounds the code exchange after the browser comes back.
	oauthWait = 30 * time.Second
	// pasteHelp tells the user how to finish when the browser does not
	// come back.
	pasteHelp = "After you log in, copy the address from the bar at the top of the browser. The page can show an error. That is not a problem."
)

var (
	// ErrLoginGone is a return whose state no open login has.
	ErrLoginGone = errors.New("this login is not open any more")
	// ErrNoOAuth is an OAuth action on a connection with no OAuth login.
	ErrNoOAuth = errors.New("the connection has no OAuth login")
)

// LoginCloud gives the return page on Trebi Cloud. The daemon wires it to
// the cloud link.
type LoginCloud interface {
	// LoginReturn returns the page and the device id. The error says why
	// the cloud cannot bring a login back.
	LoginReturn() (url, deviceID string, err error)
}

// LoginReturn is what the browser brings back to a return page.
type LoginReturn struct {
	State            string
	Code             string
	Iss              string
	Error            string
	ErrorDescription string
}

// LoginResult names the connection of a finished login.
type LoginResult struct {
	ID    string
	Title string
}

// LoginCheck is what a check of an online server found about its login.
type LoginCheck struct {
	Needed       bool   `json:"needed"`
	Server       string `json:"server,omitempty"`       // the login server
	Registration string `json:"registration,omitempty"` // cimd | dcr | own_app
}

// WithOAuth sets the OAuth client of online MCP servers.
func (s *Service) WithOAuth(c *oauth.Client) *Service {
	s.oauth = c
	return s
}

// WithLoginCloud sets the return page on Trebi Cloud.
func (s *Service) WithLoginCloud(c LoginCloud) *Service {
	s.loginCloud = c
	return s
}

// SetLocalURL sets the address of the API on this computer, for example
// http://127.0.0.1:7433. The return page on this computer uses it.
func (s *Service) SetLocalURL(u string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.localURL = strings.TrimSuffix(u, "/")
}

// OAuthClient returns the OAuth client of online MCP servers.
func (s *Service) OAuthClient() *oauth.Client { return s.oauth }

// OAuthStatus returns the OAuth login of a connection, without a secret.
func (s *Service) OAuthStatus(ref string) (oauth.Status, error) {
	r, err := s.Resolve(ref, "")
	if err != nil {
		return oauth.Status{}, err
	}
	if _, ok := s.oauthMethod(r); !ok {
		return oauth.Status{}, ErrNoOAuth
	}
	return s.oauth.Status(r.ID)
}

// SetOAuthApp stores the own app of a connection: the client id in its
// settings and the secret in a reserved secret. An empty client id
// removes the app.
func (s *Service) SetOAuthApp(ctx context.Context, ref, clientID, secret string) error {
	c, err := s.Lookup(ref)
	if err != nil {
		return err
	}
	set := OAuthSettings{}
	if c.OAuth != nil {
		set = *c.OAuth
	}
	set.ClientID = strings.TrimSpace(clientID)
	if set.ClientID == "" && set.Registration == manifest.RegistrationOwnApp {
		set.Registration = ""
	}
	raw, err := json.Marshal(set)
	if err != nil {
		return err
	}
	if _, err := s.Patch(ctx, c.ID, ConnectionPatch{OAuth: raw}); err != nil {
		return err
	}
	if set.ClientID == "" || secret == "" {
		return deleteSecret(oauthAppSecret(c.ID))
	}
	return secrets.Set(oauthAppSecret(c.ID), secret)
}

// CheckLogin probes an online MCP server and tells whether it needs an
// OAuth login, and how Trebi gets a client for it.
func (s *Service) CheckLogin(ctx context.Context, mcpURL string) (LoginCheck, error) {
	srv, err := s.oauth.Detect(ctx, mcpURL)
	if err != nil || srv == nil {
		return LoginCheck{}, err
	}
	return LoginCheck{Needed: true, Server: srv.Issuer, Registration: s.oauth.Registration(*srv)}, nil
}

// FinishLogin ends the open login of a return. The return page on this
// computer and the cloud frame call it.
func (s *Service) FinishLogin(ctx context.Context, ret LoginReturn) (LoginResult, error) {
	s.mu.Lock()
	f := s.oauthStates[ret.State]
	s.mu.Unlock()
	if f == nil || ret.State == "" {
		return LoginResult{}, ErrLoginGone
	}
	of := f.oauthFlow()
	res := LoginResult{ID: f.Instance, Title: s.titleOf(f.Instance)}
	return res, s.completeOAuth(ctx, f, of, res.Title, ret)
}

// CheckOnline probes an online MCP server. It installs nothing. The check
// has no staging folder, so the add sends the URL again.
func (s *Service) CheckOnline(ctx context.Context, link string) (SourceCheck, error) {
	m, err := onlineManifest(link, "")
	if err != nil {
		return SourceCheck{}, err
	}
	chk := SourceCheck{
		ID: "chk_" + strings.ToLower(ulid.Make().String()), Kind: SourceOnline, URL: mcpURLOf(m),
		Entry: &SourceEntry{Name: m.Name, Title: m.Title, Description: m.Description, Version: m.Version, Capabilities: []string{"actions"}, Inputs: []manifest.Input{}},
		Files: []string{}, Found: []string{}, ExpiresAt: s.now().Add(checkTTL).UTC(),
		Runs: SourceRuns{Commands: []string{}, Operations: []string{}, Method: SourceOnline},
	}
	login, err := s.CheckLogin(ctx, chk.URL)
	if errors.Is(err, oauth.ErrNotMCP) {
		return SourceCheck{}, notOnline()
	}
	if err != nil {
		chk.Results = []SourceResult{{Group: "server", Check: "reach", Status: "fail", Message: "The server does not answer: " + err.Error()}}
		return chk, nil
	}
	chk.Results = []SourceResult{{Group: "server", Check: "reach", Status: "pass", Message: "The server answers"}}
	if login.Needed {
		chk.Login = &login
		chk.Results = append(chk.Results, SourceResult{Group: "server", Check: "login", Status: "pass", Message: "It needs a login"})
	}
	chk.CanInstall = true
	return chk, nil
}

// CreateOnline adds an online MCP server by its URL. Trebi writes the
// manifest. A server that needs a login gets OAuth settings.
func (s *Service) CreateOnline(ctx context.Context, c Connection, mcpURL string) (Connection, error) {
	m, err := onlineManifest(mcpURL, c.Title)
	if err != nil {
		return Connection{}, err
	}
	login, err := s.CheckLogin(ctx, mcpURL)
	switch {
	case errors.Is(err, oauth.ErrNotMCP):
		return Connection{}, notOnline()
	case err != nil:
		return Connection{}, &ValidationError{Name: "url", Fields: []FieldError{{Field: "url", Message: "the server does not answer: " + err.Error()}}}
	}
	if login.Needed && c.OAuth == nil {
		c.OAuth = &OAuthSettings{}
	}
	c.Type, c.From, c.Manifest = m.Name, FromInline, &m
	return s.Create(ctx, c)
}

// oauthMethod returns the OAuth login of a connection: the manifest method
// with oauth, else the method that Trebi adds to an online server with
// OAuth settings.
func (s *Service) oauthMethod(r Resolved) (manifest.LoginMethod, bool) {
	if s.oauth == nil {
		return manifest.LoginMethod{}, false
	}
	if m, ok := r.Manifest.Setup.OAuthMethod(); ok {
		return m, true
	}
	c, err := s.Lookup(r.ID)
	if err != nil || c.OAuth == nil || mcpURLOf(r.Manifest) == "" {
		return manifest.LoginMethod{}, false
	}
	return manifest.LoginMethod{ID: LoginOAuth, Type: manifest.LoginBrowser, Label: "Log in with " + r.Title, OAuth: &manifest.OAuthLogin{}}, true
}

// oauthConn builds the OAuth view of a connection from its settings and
// its manifest method.
func (s *Service) oauthConn(r Resolved) (oauth.Conn, OAuthSettings, error) {
	m, ok := s.oauthMethod(r)
	if !ok {
		return oauth.Conn{}, OAuthSettings{}, ErrNoOAuth
	}
	c, err := s.Lookup(r.ID)
	if err != nil {
		return oauth.Conn{}, OAuthSettings{}, err
	}
	set := OAuthSettings{}
	if c.OAuth != nil {
		set = *c.OAuth
	}
	srv, _, err := r.MCPServer()
	if err != nil || srv == nil || srv.URL == "" {
		return oauth.Conn{}, set, cmp.Or(err, ErrNoOAuth)
	}
	conn := oauth.Conn{ID: r.ID, URL: srv.URL, Scopes: set.Scopes, Registration: cmp.Or(set.Registration, m.OAuth.Registration)}
	if len(conn.Scopes) == 0 {
		conn.Scopes = m.OAuth.Scopes
	}
	if id := cmp.Or(set.ClientID, m.OAuth.ClientID); id != "" {
		secret, err := secrets.Get(oauthAppSecret(r.ID))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return oauth.Conn{}, set, err
		}
		conn.OwnApp = &oauth.OwnApp{ClientID: id, ClientSecret: string(secret)}
	}
	return conn, set, nil
}

// beginOAuthLogin starts an OAuth flow: a url step with the login page and
// a field for the address of the last page.
func (s *Service) beginOAuthLogin(ctx context.Context, instance string, m LoginMethod) (*Flow, error) {
	return s.startOAuth(ctx, instance, m, s.oauth.Begin)
}

// stepUpLogin starts a login that asks for more access, after a 403
// insufficient_scope. The user finishes it on the Login tab. It starts
// nothing while a login of the connection is open.
func (s *Service) stepUpLogin(ctx context.Context, id string, challenge []string) error {
	if s.openOAuthFlow(id) != nil {
		return nil
	}
	methods, err := s.LoginMethods(ctx, id)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(methods, func(m LoginMethod) bool { return m.Owner == OwnerDaemon && m.Type == protocol.LoginBrowser })
	if i < 0 {
		return ErrNoOAuth
	}
	_, err = s.startOAuth(ctx, id, methods[i], func(ctx context.Context, conn oauth.Conn, ret oauth.ReturnAddr) (*oauth.Flow, error) {
		return s.oauth.StepUp(ctx, conn, ret, challenge)
	})
	if err != nil {
		return err
	}
	s.emitConnector("connector.auth.required", id, map[string]string{"reason": "scope"})
	return nil
}

// startOAuth runs begin and opens the flow of its login page.
func (s *Service) startOAuth(ctx context.Context, instance string, m LoginMethod,
	begin func(context.Context, oauth.Conn, oauth.ReturnAddr) (*oauth.Flow, error),
) (*Flow, error) {
	r, err := s.Resolve(instance, "")
	if err != nil {
		return nil, err
	}
	conn, set, err := s.oauthConn(r)
	if err != nil {
		return nil, err
	}
	ret, note := s.returnAddr(ctx, set.Return)
	of, err := begin(ctx, conn, ret)
	if err != nil {
		return nil, oauthError(r.Title, err)
	}
	f := s.flow(instance, "oauth_"+strings.ToLower(ulid.Make().String()), true)
	f.setMethod(m.LoginMethod, nil)
	f.setOAuth(of)
	s.mu.Lock()
	s.oauthStates[of.State] = f
	s.mu.Unlock()
	f.setStep(protocol.Step{
		Kind:      protocol.StepURL,
		URL:       of.AuthURL,
		Message:   cmp.Or(note, "Log in to "+r.Title+" in the browser. Trebi finishes the login when the browser comes back."),
		ExpiresAt: of.Expires.UTC().Format(time.RFC3339),
		Fields:    []protocol.StepField{{Name: "code", Label: "Paste the address from the browser", Help: pasteHelp}},
	}, true)
	return f, nil
}

// returnAddr picks the return page. Auto uses this computer for a local
// caller and Trebi Cloud for a phone or a remote client. Without remote
// access, the note tells the user to paste the address.
func (s *Service) returnAddr(ctx context.Context, mode string) (oauth.ReturnAddr, string) {
	s.mu.Lock()
	local := oauth.ReturnAddr{URL: cmp.Or(s.localURL, "http://127.0.0.1") + ReturnPath}
	s.mu.Unlock()
	if mode == ReturnLocal || (mode != ReturnCloud && !remoteCaller(ctx)) {
		return local, ""
	}
	if s.loginCloud != nil {
		u, dev, err := s.loginCloud.LoginReturn()
		if err == nil {
			return oauth.ReturnAddr{URL: u, StatePrefix: dev}, ""
		}
		s.log.Info("connector.login_return", "return", "local", "reason", err)
	}
	return local, "Remote access is not active for this Trebi account. Log in on the Trebi computer, or paste the address of the last page here."
}

// submitOAuthLogin finishes a flow with the pasted address or code.
func (s *Service) submitOAuthLogin(ctx context.Context, f *Flow, of *oauth.Flow, fields map[string]protocol.Value) error {
	raw := strings.TrimSpace(fields["code"].String())
	if raw == "" {
		return protocol.NewError(protocol.CodeInvalid, "Paste the address from the browser first.")
	}
	ret, err := parseReturn(raw, of.State)
	if err != nil {
		return protocol.NewError(protocol.CodeInvalid, err.Error())
	}
	return s.completeOAuth(ctx, f, of, s.titleOf(f.Instance), ret)
}

// completeOAuth swaps the code for tokens and ends the flow. The flow
// ends with the error of the server, or of the exchange.
func (s *Service) completeOAuth(ctx context.Context, f *Flow, of *oauth.Flow, title string, ret LoginReturn) error {
	s.dropState(of.State)
	if ret.Error != "" {
		msg := deniedMessage(title, ret.Error, ret.ErrorDescription)
		f.finish(protocol.AuthDoneParams{FlowID: f.ID, Error: msg})
		return loginError(msg)
	}
	f.setStep(protocol.Step{Kind: protocol.StepWait, Message: "Trebi finishes the login."}, false)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), oauthWait)
	defer cancel()
	if err := s.oauth.Finish(ctx, of, ret.Code, ret.Iss); err != nil {
		msg := oauthError(title, err).Error()
		f.finish(protocol.AuthDoneParams{FlowID: f.ID, Error: msg})
		return loginError(msg)
	}
	f.finish(protocol.AuthDoneParams{FlowID: f.ID, OK: true})
	s.emitConnector("connector.auth.completed", f.Instance, nil)
	return nil
}

// logoutOAuth revokes and drops the tokens of a connection. The server and
// the client stay, so the next login is fast.
func (s *Service) logoutOAuth(ctx context.Context, id string) error {
	if err := s.oauth.Logout(ctx, id); err != nil {
		return err
	}
	s.emitConnector("connector.updated", id, nil)
	return nil
}

// forgetOAuth drops every OAuth record of a removed connection.
func (s *Service) forgetOAuth(ctx context.Context, id string) {
	if s.oauth != nil {
		if err := s.oauth.Forget(ctx, id); err != nil {
			s.log.Warn("connector.oauth_forget", "connector", id, "err", err)
		}
	}
	if err := deleteSecret(oauthAppSecret(id)); err != nil {
		s.log.Warn("connector.oauth_forget", "connector", id, "err", err)
	}
}

// oauthRow is the login state of a connection with an OAuth login.
func (s *Service) oauthRow(id string) map[string]any {
	if f := s.openOAuthFlow(id); f != nil {
		return map[string]any{"state": protocol.StateAuthPending, "message": "Finish the login in the browser.", "flow_id": f.ID}
	}
	st, err := s.oauth.Status(id)
	switch {
	case err != nil:
		return map[string]any{"state": protocol.StateError, "message": err.Error()}
	case st.LoggedIn:
		return map[string]any{"state": protocol.StateConnected, "message": ""}
	}
	return map[string]any{"state": protocol.StateAuthRequired, "message": "Log in first."}
}

// openOAuthFlow returns the OAuth flow of a connection that waits for the
// browser, or nil.
func (s *Service) openOAuthFlow(id string) *Flow {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.oauthStates {
		if f.Instance == id && !f.oauthFlow().Expired(now) {
			return f
		}
	}
	return nil
}

// dropState forgets the state of a flow, so a second return fails.
func (s *Service) dropState(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.oauthStates, state)
}

// pruneOAuth ends the OAuth flows whose login page expired.
func (s *Service) pruneOAuth() {
	now := s.now()
	s.mu.Lock()
	var gone []*Flow
	for state, f := range s.oauthStates {
		if f.oauthFlow().Expired(now) {
			delete(s.oauthStates, state)
			gone = append(gone, f)
		}
	}
	s.mu.Unlock()
	for _, f := range gone {
		f.finish(protocol.AuthDoneParams{FlowID: f.ID, Error: "The login expired. Start it again."})
	}
}

// titleOf returns the display title of a connection, else its id.
func (s *Service) titleOf(id string) string {
	if r, err := s.Resolve(id, ""); err == nil {
		return r.Title
	}
	return id
}

// OnlineLink reports whether a link is the address of an online server, not
// a GitHub folder.
func OnlineLink(link string) bool {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return false
	}
	h := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	return h != "github.com"
}

// mcpURLOf returns the URL of an online MCP server, or "".
func mcpURLOf(m manifest.Manifest) string {
	if m.Actions == nil || m.Actions.MCP == nil {
		return ""
	}
	return m.Actions.MCP.URL
}

// oauthAppSecret is the reserved secret that holds the client secret of
// the own app of a connection.
func oauthAppSecret(id string) string { return secrets.ReservedPrefix + id + ".app" }

// deleteSecret removes a secret. A missing one is not an error.
func deleteSecret(name string) error {
	if err := secrets.Delete(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// remoteCaller reports whether a request comes from a phone or from a
// remote client, whose browser cannot reach this computer.
func remoteCaller(ctx context.Context) bool {
	a, ok := tools.ActorFrom(ctx)
	return ok && (a.Kind == tools.ActorMobile || a.Kind == tools.ActorRemote)
}

// parseReturn reads a pasted return: the full address of the last page, or
// the bare code. A full address must carry the state of the flow.
func parseReturn(raw, state string) (LoginReturn, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		if strings.ContainsAny(raw, " /?&") {
			return LoginReturn{}, errors.New("this is not the address of the last page; copy the full address from the browser")
		}
		return LoginReturn{State: state, Code: raw}, nil
	}
	q := u.Query()
	ret := LoginReturn{State: q.Get("state"), Code: q.Get("code"), Iss: q.Get("iss"), Error: q.Get("error"), ErrorDescription: q.Get("error_description")}
	switch {
	case ret.Code == "" && ret.Error == "":
		return LoginReturn{}, errors.New("this address has no login code; copy the address after the login")
	case ret.State != state:
		return LoginReturn{}, errors.New("this address is from another login; start the login again")
	}
	return ret, nil
}

// deniedMessage is the text of an error that the server sent back to the
// return page.
func deniedMessage(title, code, desc string) string {
	if code == "access_denied" {
		return title + " said: you did not allow access."
	}
	return fmt.Sprintf("%s did not finish the login: %s", title, cmp.Or(desc, code))
}

// oauthError is the text of a failed OAuth step for the Login tab.
func oauthError(title string, err error) error {
	switch {
	case errors.Is(err, oauth.ErrNoRegistration):
		return protocol.NewError(protocol.CodeInvalid, title+" lets only known apps log in. Add the app details of your "+title+" account first.")
	case errors.Is(err, oauth.ErrFlowExpired):
		return protocol.NewError(protocol.CodeInvalid, "The login expired. Start it again.")
	case errors.Is(err, oauth.ErrNoMetadata):
		return protocol.NewError(protocol.CodeInvalid, title+" does not offer a login that Trebi can use.")
	}
	var re *oauth.ResponseError
	if errors.As(err, &re) {
		return protocol.NewError(protocol.CodeInvalid, fmt.Sprintf("%s refused the login: %s", title, re.Error()))
	}
	return protocol.NewError(protocol.CodeTransient, fmt.Sprintf("Trebi could not reach the login server of %s: %v", title, err))
}

// onlineManifest writes the manifest of an online MCP server. The name
// comes from the host: mcp.granola.ai gives granola.
func onlineManifest(link, title string) (manifest.Manifest, error) {
	bad := &ValidationError{Name: "url", Fields: []FieldError{{Field: "url", Message: "use the https address of the server"}}}
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || u.Host == "" || (u.Scheme != "https" && (u.Scheme != "http" || !loopback(u.Hostname()))) {
		return manifest.Manifest{}, bad
	}
	host := strings.ToLower(u.Hostname())
	for _, p := range []string{"mcp.", "api.", "www."} {
		host = strings.TrimPrefix(host, p)
	}
	name, _, _ := strings.Cut(host, ".")
	if loopback(u.Hostname()) {
		name = "local"
	}
	if name == "" {
		return manifest.Manifest{}, bad
	}
	if title = strings.TrimSpace(title); title == "" {
		title = strings.ToUpper(name[:1]) + name[1:]
	}
	return manifest.Manifest{
		Schema: manifest.SchemaID, Name: name, Title: title, Version: "0.1.0",
		Description: "Online server at " + u.Host + ".",
		Actions:     &manifest.Actions{MCP: &manifest.MCPAction{URL: u.String()}},
	}, nil
}

// notOnline is the error of an address that is neither a GitHub folder nor
// an online server.
func notOnline() error {
	return &ValidationError{Name: "url", Fields: []FieldError{{Field: "url", Message: "this address is not a GitHub folder or an online MCP server"}}}
}

// loopback reports whether a host is this computer. Only such a server may
// use plain http.
func loopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}
