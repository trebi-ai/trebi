// Package oauth is the OAuth client of Trebi for online MCP servers. Other
// code asks it for a login address, gives it the code, and asks it for a
// valid token. Nothing else in the daemon reads a token.
//
// Library choice: go-sdk auth.AuthorizationCodeHandler owns one session
// and gets the code through a synchronous fetcher, but here the code comes
// later through a route, and the tokens live in the Trebi secrets. So the
// package uses go-sdk oauthex and auth only for metadata discovery and
// dynamic client registration. It sends the token, refresh, and revoke
// requests itself, because golang.org/x/oauth2 cannot send the resource
// parameter (RFC 8707) on a refresh.
package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

var (
	// ErrLoginRequired is a connection with no usable token: never logged
	// in, logged out, or a refresh that the server refused.
	ErrLoginRequired = errors.New("log in again")
	// ErrLoginEnded is a token that Trebi dropped now, because the server
	// refused to renew it. Only the call that drops the token gets it.
	ErrLoginEnded = fmt.Errorf("%w: the server ended the login", ErrLoginRequired)
	// ErrNoRegistration is a server that offers no way for Trebi to get a
	// client. The user adds their own app.
	ErrNoRegistration = errors.New("the login server needs your own app")
	// ErrFlowExpired is a code that came after the flow ended.
	ErrFlowExpired = errors.New("the login took too long; start it again")
	// ErrNoLogin is a server that answers without a login.
	ErrNoLogin = errors.New("the server does not need a login")
)

// Steps that errors name.
const (
	stepDiscover  = "discover"
	stepRegister  = "register"
	stepAuthorize = "authorize"
	stepExchange  = "exchange"
	stepRefresh   = "refresh"
	stepRevoke    = "revoke"
)

const (
	flowTTL     = 10 * time.Minute
	refreshSkew = 60 * time.Second
)

// Error is a failed step. Err is a *ResponseError when the server answered
// with an OAuth error.
type Error struct {
	Step string
	Err  error
}

// ResponseError is an OAuth error answer (RFC 6749 §5.2).
type ResponseError struct {
	Status      int
	Code        string
	Description string
}

// Config is the identity of Trebi at an auth server.
type Config struct {
	ClientName string // shown on the consent page
	CIMDURL    string // client ID metadata document URL; empty turns CIMD off
	// CloudReturn is also registered with DCR, so a switch between the
	// return pages needs no new registration.
	CloudReturn string
	Now         func() time.Time
}

// OwnApp is the app that the user registered with the service.
type OwnApp struct {
	ClientID     string
	ClientSecret string
}

// Conn is one connection to an online MCP server.
type Conn struct {
	ID           string
	URL          string
	Scopes       []string // asked for on top of the server scopes
	Registration string   // RegAuto when empty
	OwnApp       *OwnApp
}

// ReturnAddr is where the browser comes back. StatePrefix starts the state
// value; the cloud return page reads the device id from it.
type ReturnAddr struct {
	URL         string
	StatePrefix string
}

// Flow is one started login. It holds the PKCE verifier, so it stays in
// the memory of the daemon only.
type Flow struct {
	Conn     string
	State    string
	AuthURL  string
	Return   string
	Expires  time.Time
	Scopes   []string
	verifier string
	server   Server
	client   Registration
}

// Status is what a user may see of a connection login. It has no secret.
type Status struct {
	LoggedIn   bool      `json:"logged_in"`
	Server     string    `json:"server,omitempty"`
	Issuer     string    `json:"issuer,omitempty"`
	ClientKind string    `json:"client_kind,omitempty"`
	ClientID   string    `json:"client_id,omitempty"`
	Scopes     []string  `json:"scopes,omitempty"`
	Expiry     time.Time `json:"expires_at,omitzero"`
	Renewable  bool      `json:"renewable"`
	// Registration is how the next login gets a client: cimd, dcr, or own_app.
	Registration string `json:"registration,omitempty"`
}

// Client is the OAuth client. It is safe for concurrent use.
type Client struct {
	store Store
	http  *http.Client
	cfg   Config

	mu    sync.Mutex
	locks map[string]*sync.Mutex // connection id → refresh lock
}

// NewClient returns a client. A nil http client uses http.DefaultClient.
func NewClient(store Store, hc *http.Client, cfg Config) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ClientName == "" {
		cfg.ClientName = "Trebi"
	}
	return &Client{store: store, http: hc, cfg: cfg, locks: map[string]*sync.Mutex{}}
}

func (e *Error) Error() string { return e.Step + ": " + e.Err.Error() }

func (e *Error) Unwrap() error { return e.Err }

func (e *ResponseError) Error() string {
	if e.Description != "" {
		return e.Code + ": " + e.Description
	}
	if e.Code != "" {
		return e.Code
	}
	return fmt.Sprintf("status %d", e.Status)
}

// Expired reports whether the flow ended at t.
func (f *Flow) Expired(t time.Time) bool { return !t.Before(f.Expires) }

// Begin discovers the server, gets a client, and makes the authorize
// address with PKCE S256, state, and resource.
func (c *Client) Begin(ctx context.Context, conn Conn, ret ReturnAddr) (*Flow, error) {
	return c.begin(ctx, conn, ret, nil)
}

// StepUp starts a new login that adds the scopes of a 403
// insufficient_scope challenge to the current ones.
func (c *Client) StepUp(ctx context.Context, conn Conn, ret ReturnAddr, challenge []string) (*Flow, error) {
	more, ok := insufficientScope(challenge)
	if !ok {
		return nil, stepErr(stepAuthorize, errors.New("the answer is not an insufficient_scope challenge"))
	}
	return c.begin(ctx, conn, ret, more)
}

// Finish swaps the code for tokens and keeps them. iss is the issuer of
// the answer (RFC 9207); an empty one is not checked, because a pasted
// bare code has none.
func (c *Client) Finish(ctx context.Context, f *Flow, code, iss string) error {
	if f.Expired(c.cfg.Now()) {
		return stepErr(stepExchange, ErrFlowExpired)
	}
	if iss != "" && strings.TrimSuffix(iss, "/") != strings.TrimSuffix(f.server.Issuer, "/") {
		return stepErr(stepExchange, fmt.Errorf("the answer comes from %s, not from %s", iss, f.server.Issuer))
	}
	if strings.TrimSpace(code) == "" {
		return stepErr(stepExchange, errors.New("no code"))
	}
	tok, err := c.tokenRequest(ctx, f.server, f.client, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {f.Return},
		"code_verifier": {f.verifier},
		"resource":      {f.server.Resource},
	}, f.Scopes)
	if err != nil {
		return stepErr(stepExchange, err)
	}
	lock := c.lock(f.Conn)
	lock.Lock()
	defer lock.Unlock()
	rec, err := c.load(f.Conn)
	if err != nil {
		return err
	}
	srv, client := f.server, f.client
	rec.Server, rec.Client, rec.Token = &srv, &client, tok
	return c.store.Save(f.Conn, rec)
}

// Token returns a valid access token. It renews one that ends within a
// minute. A refused refresh drops the token and returns ErrLoginRequired;
// a network error keeps it.
func (c *Client) Token(ctx context.Context, conn string) (string, error) {
	return c.token(ctx, conn, false)
}

// Refresh renews the token now, for a server that refused it before its
// expiry. It returns the new access token.
func (c *Client) Refresh(ctx context.Context, conn string) (string, error) {
	return c.token(ctx, conn, true)
}

// Logout revokes the tokens at the server when it can (RFC 7009) and drops
// them. The server and client stay for the next login. A failed revoke
// still logs out; its error comes back for the log.
func (c *Client) Logout(ctx context.Context, conn string) error {
	return c.drop(ctx, conn, false)
}

// Forget revokes the tokens and drops the whole record, for a removed
// connection.
func (c *Client) Forget(ctx context.Context, conn string) error {
	return c.drop(ctx, conn, true)
}

// CloudReturn is the return page on Trebi Cloud, or empty.
func (c *Client) CloudReturn() string { return c.cfg.CloudReturn }

// Status returns the login of a connection without a secret.
func (c *Client) Status(conn string) (Status, error) {
	rec, err := c.load(conn)
	if err != nil {
		return Status{}, err
	}
	var st Status
	if rec.Server != nil {
		st.Server, st.Issuer = rec.Server.Resource, rec.Server.Issuer
		st.Registration = c.Registration(*rec.Server)
	}
	if rec.Client != nil {
		st.ClientKind, st.ClientID = rec.Client.Kind, rec.Client.ID
	}
	if t := rec.Token; t != nil {
		st.LoggedIn = t.RefreshToken != "" || t.Expiry.IsZero() || c.cfg.Now().Before(t.Expiry)
		st.Scopes, st.Expiry, st.Renewable = t.Scopes, t.Expiry, t.RefreshToken != ""
	}
	return st, nil
}

func (c *Client) begin(ctx context.Context, conn Conn, ret ReturnAddr, more []string) (*Flow, error) {
	if ret.URL == "" {
		return nil, stepErr(stepAuthorize, errors.New("no return address"))
	}
	rec, err := c.load(conn.ID)
	if err != nil {
		return nil, err
	}
	srv, err := c.Detect(ctx, conn.URL)
	if err != nil {
		return nil, err
	}
	if srv == nil {
		return nil, stepErr(stepDiscover, ErrNoLogin)
	}
	client, err := c.client(ctx, conn, *srv, rec.Client, ret.URL)
	if err != nil {
		return nil, c.keepServer(conn.ID, rec, srv, err)
	}
	scopes := union(srv.Scopes, conn.Scopes, more)
	if rec.Token != nil && len(more) > 0 {
		scopes = union(scopes, rec.Token.Scopes)
	}
	verifier, err := randomString(32)
	if err != nil {
		return nil, stepErr(stepAuthorize, err)
	}
	state, err := randomString(32)
	if err != nil {
		return nil, stepErr(stepAuthorize, err)
	}
	if ret.StatePrefix != "" {
		state = ret.StatePrefix + "." + state
	}
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {client.ID},
		"redirect_uri":          {ret.URL},
		"code_challenge":        {challengeS256(verifier)},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"resource":              {srv.Resource},
	}
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	authURL, err := withQuery(srv.AuthURL, q)
	if err != nil {
		return nil, stepErr(stepAuthorize, err)
	}
	rec.Server, rec.Client = srv, client
	if err := c.store.Save(conn.ID, rec); err != nil {
		return nil, stepErr(stepRegister, err)
	}
	return &Flow{
		Conn: conn.ID, State: state, AuthURL: authURL, Return: ret.URL,
		Expires: c.cfg.Now().Add(flowTTL), Scopes: scopes,
		verifier: verifier, server: *srv, client: *client,
	}, nil
}

// keepServer stores the server of a login that needs the own app, so the
// status tells the user to bring one. It returns err.
func (c *Client) keepServer(conn string, rec Record, srv *Server, err error) error {
	if !errors.Is(err, ErrNoRegistration) {
		return err
	}
	rec.Server = srv
	if serr := c.store.Save(conn, rec); serr != nil {
		return stepErr(stepRegister, serr)
	}
	return err
}

func (c *Client) token(ctx context.Context, conn string, force bool) (string, error) {
	lock := c.lock(conn)
	lock.Lock()
	defer lock.Unlock()
	rec, err := c.load(conn)
	if err != nil {
		return "", err
	}
	t := rec.Token
	if t == nil || rec.Server == nil || rec.Client == nil {
		return "", ErrLoginRequired
	}
	if !force && (t.Expiry.IsZero() || c.cfg.Now().Add(refreshSkew).Before(t.Expiry)) {
		return t.AccessToken, nil
	}
	return c.renew(ctx, conn, rec)
}

// renew sends the refresh grant of a record. The caller holds the refresh
// lock of the connection.
func (c *Client) renew(ctx context.Context, conn string, rec Record) (string, error) {
	t := rec.Token
	if t.RefreshToken == "" {
		rec.Token = nil
		if err := c.store.Save(conn, rec); err != nil {
			return "", err
		}
		return "", ErrLoginEnded
	}
	next, err := c.tokenRequest(ctx, *rec.Server, *rec.Client, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {t.RefreshToken},
		"resource":      {rec.Server.Resource},
	}, t.Scopes)
	var re *ResponseError
	switch {
	case errors.As(err, &re) && (re.Code == "invalid_grant" || re.Code == "invalid_client" || re.Code == "unauthorized_client"):
		rec.Token = nil
		if serr := c.store.Save(conn, rec); serr != nil {
			return "", serr
		}
		return "", fmt.Errorf("%w: %w", ErrLoginEnded, stepErr(stepRefresh, err))
	case err != nil:
		return "", stepErr(stepRefresh, err)
	}
	if next.RefreshToken == "" {
		next.RefreshToken = t.RefreshToken // the server did not rotate it
	}
	rec.Token = next
	if err := c.store.Save(conn, rec); err != nil {
		return "", err
	}
	return next.AccessToken, nil
}

func (c *Client) drop(ctx context.Context, conn string, all bool) error {
	lock := c.lock(conn)
	lock.Lock()
	defer lock.Unlock()
	rec, err := c.load(conn)
	if err != nil {
		return err
	}
	var revokeErr error
	if t := rec.Token; t != nil && rec.Server != nil && rec.Client != nil && rec.Server.RevokeURL != "" {
		if t.RefreshToken != "" {
			revokeErr = c.revoke(ctx, *rec.Server, *rec.Client, t.RefreshToken, "refresh_token")
		}
		revokeErr = errors.Join(revokeErr, c.revoke(ctx, *rec.Server, *rec.Client, t.AccessToken, "access_token"))
	}
	if all {
		err = c.store.Delete(conn)
	} else if rec.Token != nil {
		rec.Token = nil
		err = c.store.Save(conn, rec)
	}
	return errors.Join(stepErr(stepRevoke, revokeErr), err)
}

// load reads the record. No record is an empty one.
func (c *Client) load(conn string) (Record, error) {
	rec, err := c.store.Load(conn)
	if errors.Is(err, ErrNoRecord) {
		return Record{}, nil
	}
	return rec, err
}

// lock returns the refresh lock of a connection, so one refresh runs at a
// time and a rotated refresh token is never used twice.
func (c *Client) lock(conn string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.locks[conn]
	if !ok {
		l = &sync.Mutex{}
		c.locks[conn] = l
	}
	return l
}

func stepErr(step string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Step: step, Err: err}
}

// union returns the distinct values of the lists in order.
func union(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		for _, v := range l {
			if v != "" && !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	return out
}

// withQuery adds q to the query of raw.
func withQuery(raw string, q url.Values) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	old := u.Query()
	for k, v := range q {
		old[k] = v
	}
	u.RawQuery = old.Encode()
	return u.String(), nil
}
