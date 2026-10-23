// Package oauthtest is a fake online MCP server with its own OAuth auth
// server, for tests. Switches turn CIMD, DCR, refresh rotation, refresh
// failure, and the scope check on and off.
package oauthtest

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Options are the switches of the fake.
type Options struct {
	CIMD       bool // the auth server takes a client ID metadata document
	DCR        bool // the auth server has a registration endpoint
	Rotate     bool // each refresh gives a new refresh token
	NoMetadata bool // 401 with no protected resource metadata
	NoRevoke   bool // no revocation endpoint
	Scopes     []string
	AccessTTL  time.Duration // 0 is one hour
	// OwnApp is a client that the user registered by hand. A secret makes
	// it a confidential client.
	OwnApp *App
}

// App is a registered client.
type App struct {
	ID     string
	Secret string
	URIs   []string // empty takes any return address
}

type grant struct {
	client    string
	scopes    []string
	resource  string
	challenge string
	redirect  string
	expires   time.Time
}

// Server is the fake. Its exported counters are safe to read after the
// requests end.
type Server struct {
	*httptest.Server
	opts Options

	mu          sync.Mutex
	clients     map[string]App
	codes       map[string]grant
	access      map[string]grant
	refresh     map[string]grant
	failRefresh bool
	need        string // scope that the MCP endpoint needs
	revoked     []string
	refreshes   int
	authorizes  []url.Values
	tokens      []url.Values
	registered  int

	mcp http.Handler
}

// New starts a fake. The test closes it.
func New(t testing.TB, opts Options) *Server {
	t.Helper()
	s := &Server{
		opts: opts, clients: map[string]App{}, codes: map[string]grant{},
		access: map[string]grant{}, refresh: map[string]grant{},
	}
	if opts.OwnApp != nil {
		s.clients[opts.OwnApp.ID] = *opts.OwnApp
	}
	s.mcp = http.HandlerFunc(s.serveMCP)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", s.handlePRM)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.handleASM)
	mux.HandleFunc("GET /authorize", s.handleAuthorize)
	mux.HandleFunc("POST /token", s.handleToken)
	mux.HandleFunc("POST /register", s.handleRegister)
	mux.HandleFunc("POST /revoke", s.handleRevoke)
	mux.Handle("/mcp", s.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handler().ServeHTTP(w, r) })))
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// MCPURL is the address of the MCP endpoint.
func (s *Server) MCPURL() string { return s.URL + "/mcp" }

// SetMCP replaces the handler behind the bearer check.
func (s *Server) SetMCP(h http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mcp = h
}

// FailRefresh makes each refresh answer invalid_grant.
func (s *Server) FailRefresh(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failRefresh = on
}

// NeedScope makes the MCP endpoint answer 403 insufficient_scope to a
// token without scope.
func (s *Server) NeedScope(scope string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.need = scope
}

// ExpireAccess makes each access token invalid, so the next call gets 401.
func (s *Server) ExpireAccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.access)
}

// Revoked returns the revoked tokens.
func (s *Server) Revoked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.revoked)
}

// Refreshes counts the refresh grants that succeeded.
func (s *Server) Refreshes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshes
}

// Registered counts the dynamic client registrations.
func (s *Server) Registered() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.registered
}

// Authorizes returns the query of each authorize request.
func (s *Server) Authorizes() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.authorizes)
}

// TokenRequests returns the form of each token request.
func (s *Server) TokenRequests() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.tokens)
}

// Issue mints an access token with scopes, as if a login happened.
func (s *Server) Issue(scopes ...string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok := random()
	s.access[tok] = grant{scopes: scopes, resource: s.MCPURL(), expires: time.Now().Add(time.Hour)}
	return tok
}

// Authorize plays the browser: it opens the authorize address, the fake
// logs the user in at once, and it returns the return address with the
// code, without opening it.
func (s *Server) Authorize(authURL string) (*url.URL, error) {
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Get(authURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // test fake
	if resp.StatusCode != http.StatusFound {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("authorize: %d %s", resp.StatusCode, b)
	}
	return url.Parse(resp.Header.Get("Location"))
}

// Protect answers 401 to a request without a valid bearer token and 403
// insufficient_scope to a token without the needed scope.
func (s *Server) Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		g, ok := s.access[tok]
		need := s.need
		s.mu.Unlock()
		if !ok || time.Now().After(g.expires) {
			if s.opts.NoMetadata {
				w.Header().Set("WWW-Authenticate", `Bearer realm="fake"`)
			} else {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q`, s.URL+"/.well-known/oauth-protected-resource/mcp"))
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if need != "" && !slices.Contains(g.scopes, need) {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer error="insufficient_scope", scope=%q`, need))
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handler() http.Handler {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mcp
}

func (s *Server) handlePRM(w http.ResponseWriter, _ *http.Request) {
	if s.opts.NoMetadata {
		http.NotFound(w, nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":              s.MCPURL(),
		"authorization_servers": []string{s.URL},
		"scopes_supported":      s.opts.Scopes,
	})
}

func (s *Server) handleASM(w http.ResponseWriter, _ *http.Request) {
	m := map[string]any{
		"issuer":                                         s.URL,
		"authorization_endpoint":                         s.URL + "/authorize",
		"token_endpoint":                                 s.URL + "/token",
		"response_types_supported":                       []string{"code"},
		"code_challenge_methods_supported":               []string{"S256"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported":          []string{"none", "client_secret_basic"},
		"authorization_response_iss_parameter_supported": true,
	}
	if s.opts.DCR {
		m["registration_endpoint"] = s.URL + "/register"
	}
	if s.opts.CIMD {
		m["client_id_metadata_document_supported"] = true
	}
	if !s.opts.NoRevoke {
		m["revocation_endpoint"] = s.URL + "/revoke"
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	s.authorizes = append(s.authorizes, q)
	app, known := s.clients[q.Get("client_id")]
	s.mu.Unlock()
	cimd := s.opts.CIMD && strings.HasPrefix(q.Get("client_id"), "https://")
	redirect := q.Get("redirect_uri")
	switch {
	case !known && !cimd:
		http.Error(w, "unknown client", http.StatusBadRequest)
		return
	case known && len(app.URIs) > 0 && !slices.Contains(app.URIs, redirect):
		http.Error(w, "redirect_uri is not registered", http.StatusBadRequest)
		return
	case q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "":
		http.Error(w, "PKCE S256 is required", http.StatusBadRequest)
		return
	case q.Get("resource") != s.MCPURL():
		http.Error(w, "wrong resource", http.StatusBadRequest)
		return
	case q.Get("response_type") != "code":
		http.Error(w, "response_type must be code", http.StatusBadRequest)
		return
	}
	code := random()
	s.mu.Lock()
	s.codes[code] = grant{
		client: q.Get("client_id"), scopes: strings.Fields(q.Get("scope")), resource: q.Get("resource"),
		challenge: q.Get("code_challenge"), redirect: redirect, expires: time.Now().Add(time.Minute),
	}
	s.mu.Unlock()
	back, err := url.Parse(redirect)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	bq := back.Query()
	bq.Set("code", code)
	bq.Set("state", q.Get("state"))
	bq.Set("iss", s.URL)
	back.RawQuery = bq.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthErr(w, "invalid_request", err.Error())
		return
	}
	f := r.PostForm
	clientID, secret, basic := r.BasicAuth()
	if basic {
		clientID, _ = url.QueryUnescape(clientID)
		secret, _ = url.QueryUnescape(secret)
	} else {
		clientID, secret = f.Get("client_id"), f.Get("client_secret")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = append(s.tokens, f)
	if app, ok := s.clients[clientID]; ok && app.Secret != "" && app.Secret != secret {
		oauthErr(w, "invalid_client", "bad client secret")
		return
	}
	switch f.Get("grant_type") {
	case "authorization_code":
		s.codeGrantLocked(w, f, clientID)
	case "refresh_token":
		s.refreshGrantLocked(w, f, clientID)
	default:
		oauthErr(w, "unsupported_grant_type", f.Get("grant_type"))
	}
}

// codeGrantLocked answers an authorization_code grant. The caller holds
// s.mu.
func (s *Server) codeGrantLocked(w http.ResponseWriter, f url.Values, clientID string) {
	g, ok := s.codes[f.Get("code")]
	delete(s.codes, f.Get("code"))
	switch {
	case !ok || time.Now().After(g.expires):
		oauthErr(w, "invalid_grant", "unknown code")
	case g.client != clientID:
		oauthErr(w, "invalid_grant", "code of another client")
	case g.redirect != f.Get("redirect_uri"):
		oauthErr(w, "invalid_grant", "redirect_uri does not match")
	case s256(f.Get("code_verifier")) != g.challenge:
		oauthErr(w, "invalid_grant", "PKCE verifier does not match")
	case f.Get("resource") != g.resource:
		oauthErr(w, "invalid_target", "wrong resource")
	default:
		s.grantLocked(w, g, "")
	}
}

// refreshGrantLocked answers a refresh_token grant. The caller holds s.mu.
func (s *Server) refreshGrantLocked(w http.ResponseWriter, f url.Values, clientID string) {
	old := f.Get("refresh_token")
	g, ok := s.refresh[old]
	switch {
	case s.failRefresh || !ok:
		oauthErr(w, "invalid_grant", "refresh token is not valid")
	case g.client != clientID:
		oauthErr(w, "invalid_grant", "refresh token of another client")
	case f.Get("resource") != g.resource:
		oauthErr(w, "invalid_target", "wrong resource")
	default:
		s.refreshes++
		keep := old
		if s.opts.Rotate {
			delete(s.refresh, old)
			keep = ""
		}
		s.grantLocked(w, g, keep)
	}
}

// grantLocked issues an access token, and a refresh token unless keep
// names the one that stays. The caller holds s.mu.
func (s *Server) grantLocked(w http.ResponseWriter, g grant, keep string) {
	ttl := s.opts.AccessTTL
	if ttl == 0 {
		ttl = time.Hour
	}
	g.expires = time.Now().Add(ttl)
	access := random()
	s.access[access] = g
	out := map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": int(ttl.Seconds()), "scope": strings.Join(g.scopes, " ")}
	if keep == "" {
		rt := random()
		s.refresh[rt] = g
		out["refresh_token"] = rt
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.opts.DCR {
		http.NotFound(w, r)
		return
	}
	var meta struct {
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(r.Body).Decode(&meta); err != nil || len(meta.RedirectURIs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_redirect_uri"})
		return
	}
	id := "dcr-" + random()[:8]
	s.mu.Lock()
	s.clients[id] = App{ID: id, URIs: meta.RedirectURIs}
	s.registered++
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"client_id": id, "redirect_uris": meta.RedirectURIs, "token_endpoint_auth_method": "none"})
}

func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthErr(w, "invalid_request", err.Error())
		return
	}
	tok := r.PostForm.Get("token")
	s.mu.Lock()
	s.revoked = append(s.revoked, tok)
	delete(s.access, tok)
	delete(s.refresh, tok)
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// serveMCP is a tiny MCP server: initialize, tools/list with one echo
// tool, and tools/call.
func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch req.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "fake-session")
		result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake", "version": "1"}}
	case "tools/list":
		result = map[string]any{"tools": []map[string]any{{"name": "echo", "description": "Echo the text.", "inputSchema": map[string]any{"type": "object"}}}}
	case "tools/call":
		result = map[string]any{"content": []map[string]any{{"type": "text", "text": string(req.Params)}}}
	default:
		writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) //nolint:errcheck // test fake
}

func oauthErr(w http.ResponseWriter, code, desc string) {
	status := http.StatusBadRequest
	if code == "invalid_client" {
		status = http.StatusUnauthorized
	}
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

func random() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(errors.Join(errors.New("oauthtest: random"), err))
	}
	return hex.EncodeToString(b)
}

func s256(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
