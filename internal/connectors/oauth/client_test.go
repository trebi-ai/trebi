package oauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/oauth/oauthtest"
	"github.com/trebi-ai/trebi/internal/secrets"
)

const testReturn = "http://127.0.0.1:7433/login/done"

type memStore struct {
	mu   sync.Mutex
	recs map[string]Record
}

func (m *memStore) Load(conn string) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[conn]
	if !ok {
		return Record{}, ErrNoRecord
	}
	return r, nil
}

func (m *memStore) Save(conn string, r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs[conn] = r
	return nil
}

func (m *memStore) Delete(conn string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.recs, conn)
	return nil
}

func TestRegistrationPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		opts     oauthtest.Options
		conn     Conn
		cimdURL  string
		wantKind string
		wantErr  error
		wantDCR  int
	}{
		{name: "cimd", opts: oauthtest.Options{CIMD: true, DCR: true}, cimdURL: "https://api.trebi.ai/oauth/client.json", wantKind: RegCIMD},
		{name: "dcr when no cimd document", opts: oauthtest.Options{CIMD: true, DCR: true}, wantKind: RegDCR, wantDCR: 1},
		{name: "dcr", opts: oauthtest.Options{DCR: true}, cimdURL: "https://api.trebi.ai/oauth/client.json", wantKind: RegDCR, wantDCR: 1},
		{name: "own app first", opts: oauthtest.Options{DCR: true, OwnApp: &oauthtest.App{ID: "mine", Secret: "s3"}}, conn: Conn{OwnApp: &OwnApp{ClientID: "mine", ClientSecret: "s3"}}, wantKind: RegOwnApp},
		{name: "forced dcr", opts: oauthtest.Options{CIMD: true, DCR: true}, cimdURL: "https://api.trebi.ai/oauth/client.json", conn: Conn{Registration: RegDCR}, wantKind: RegDCR, wantDCR: 1},
		{name: "none", opts: oauthtest.Options{}, wantErr: ErrNoRegistration},
		{name: "own app asked but missing", opts: oauthtest.Options{DCR: true}, conn: Conn{Registration: RegOwnApp}, wantErr: ErrNoRegistration},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := oauthtest.New(t, tc.opts)
			c := NewClient(newMemStore(), nil, Config{CIMDURL: tc.cimdURL})
			conn := tc.conn
			conn.ID, conn.URL = "con_1", fake.MCPURL()
			if tc.wantErr != nil {
				_, err := c.Begin(context.Background(), conn, ReturnAddr{URL: testReturn})
				var se *Error
				if !errors.Is(err, tc.wantErr) || !errors.As(err, &se) || se.Step != stepRegister {
					t.Fatalf("Begin error = %v, want %v at step register", err, tc.wantErr)
				}
				return
			}
			login(t, c, fake, conn)
			st, err := c.Status("con_1")
			if err != nil || !st.LoggedIn || st.ClientKind != tc.wantKind {
				t.Fatalf("Status = %+v, %v; want logged in with %s", st, err, tc.wantKind)
			}
			if fake.Registered() != tc.wantDCR {
				t.Fatalf("registrations = %d, want %d", fake.Registered(), tc.wantDCR)
			}
			tok, err := c.Token(context.Background(), "con_1")
			if err != nil {
				t.Fatal(err)
			}
			if resp := call(t, fake, tok); resp.StatusCode != http.StatusOK {
				t.Fatalf("MCP call status = %d", resp.StatusCode)
			}
		})
	}
}

func TestAuthorizeRequest(t *testing.T) {
	t.Parallel()
	fake := oauthtest.New(t, oauthtest.Options{DCR: true, Scopes: []string{"read"}})
	c := NewClient(newMemStore(), nil, Config{})
	f := login(t, c, fake, Conn{ID: "con_1", URL: fake.MCPURL(), Scopes: []string{"extra"}})
	q := fake.Authorizes()[0]
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Fatalf("no PKCE S256: %v", q)
	}
	if q.Get("resource") != fake.MCPURL() || q.Get("redirect_uri") != testReturn {
		t.Fatalf("resource or return: %v", q)
	}
	if q.Get("scope") != "read extra" {
		t.Fatalf("scope = %q", q.Get("scope"))
	}
	tr := fake.TokenRequests()[0]
	if tr.Get("code_verifier") != f.verifier || tr.Get("resource") != fake.MCPURL() {
		t.Fatalf("token request: %v", tr)
	}
	if len(f.State) < 40 {
		t.Fatalf("state %q is short", f.State)
	}
}

func TestStatePrefix(t *testing.T) {
	t.Parallel()
	fake := oauthtest.New(t, oauthtest.Options{DCR: true})
	c := NewClient(newMemStore(), nil, Config{})
	f, err := c.Begin(context.Background(), Conn{ID: "con_1", URL: fake.MCPURL()}, ReturnAddr{URL: "https://api.trebi.ai/login/done", StatePrefix: "dev_1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.State, "dev_1.") {
		t.Fatalf("state = %q", f.State)
	}
}

func TestFinishChecks(t *testing.T) {
	t.Parallel()
	fake := oauthtest.New(t, oauthtest.Options{DCR: true})
	now := time.Now()
	c := NewClient(newMemStore(), nil, Config{Now: func() time.Time { return now }})
	ctx := context.Background()
	f, err := c.Begin(ctx, Conn{ID: "con_1", URL: fake.MCPURL()}, ReturnAddr{URL: testReturn})
	if err != nil {
		t.Fatal(err)
	}
	back, err := fake.Authorize(f.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Finish(ctx, f, back.Query().Get("code"), "https://evil.example"); err == nil {
		t.Fatal("Finish took an answer of another issuer")
	}
	now = now.Add(11 * time.Minute)
	if err := c.Finish(ctx, f, back.Query().Get("code"), ""); !errors.Is(err, ErrFlowExpired) {
		t.Fatalf("expired flow: %v", err)
	}
}

func TestRefresh(t *testing.T) {
	t.Parallel()
	for _, rotate := range []bool{false, true} {
		t.Run(map[bool]string{false: "kept", true: "rotated"}[rotate], func(t *testing.T) {
			t.Parallel()
			fake := oauthtest.New(t, oauthtest.Options{DCR: true, Rotate: rotate, AccessTTL: 30 * time.Second})
			st := newMemStore()
			c := NewClient(st, nil, Config{})
			login(t, c, fake, Conn{ID: "con_1", URL: fake.MCPURL()})
			first := st.recs["con_1"].Token
			// The token ends within the skew, so each Token call renews it.
			tok, err := c.Token(context.Background(), "con_1")
			if err != nil {
				t.Fatal(err)
			}
			if tok == first.AccessToken || fake.Refreshes() != 1 {
				t.Fatalf("no refresh: refreshes = %d", fake.Refreshes())
			}
			second := st.recs["con_1"].Token
			if rotate == (second.RefreshToken == first.RefreshToken) {
				t.Fatalf("rotate=%v: refresh token %q → %q", rotate, first.RefreshToken, second.RefreshToken)
			}
			if _, err := c.Token(context.Background(), "con_1"); err != nil || fake.Refreshes() != 2 {
				t.Fatalf("second refresh: %v, refreshes = %d", err, fake.Refreshes())
			}
		})
	}
}

func TestRefreshFailure(t *testing.T) {
	t.Parallel()
	fake := oauthtest.New(t, oauthtest.Options{DCR: true, AccessTTL: 30 * time.Second})
	st := newMemStore()
	c := NewClient(st, nil, Config{})
	login(t, c, fake, Conn{ID: "con_1", URL: fake.MCPURL()})
	fake.FailRefresh(true)
	_, err := c.Token(context.Background(), "con_1")
	if !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("Token error = %v, want ErrLoginRequired", err)
	}
	if s, _ := c.Status("con_1"); s.LoggedIn || s.ClientKind != RegDCR {
		t.Fatalf("Status after refused refresh = %+v", s)
	}
	if _, err := c.Token(context.Background(), "con_1"); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("second Token error = %v", err)
	}
}

func TestRefreshNetworkErrorKeepsToken(t *testing.T) {
	t.Parallel()
	fake := oauthtest.New(t, oauthtest.Options{DCR: true, AccessTTL: 30 * time.Second})
	st := newMemStore()
	c := NewClient(st, nil, Config{})
	login(t, c, fake, Conn{ID: "con_1", URL: fake.MCPURL()})
	rec := st.recs["con_1"]
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	rec.Server.TokenURL = down.URL + "/token"
	st.recs["con_1"] = rec
	_, err := c.Token(context.Background(), "con_1")
	if err == nil || errors.Is(err, ErrLoginRequired) {
		t.Fatalf("Token error = %v, want a plain network error", err)
	}
	if st.recs["con_1"].Token == nil {
		t.Fatal("a network error dropped the token")
	}
}

func TestLogoutRevokes(t *testing.T) {
	t.Parallel()
	fake := oauthtest.New(t, oauthtest.Options{DCR: true})
	st := newMemStore()
	c := NewClient(st, nil, Config{})
	login(t, c, fake, Conn{ID: "con_1", URL: fake.MCPURL()})
	tok := *st.recs["con_1"].Token
	if err := c.Logout(context.Background(), "con_1"); err != nil {
		t.Fatal(err)
	}
	rev := fake.Revoked()
	if !slices.Contains(rev, tok.RefreshToken) || !slices.Contains(rev, tok.AccessToken) {
		t.Fatalf("revoked = %v", rev)
	}
	rec := st.recs["con_1"]
	if rec.Token != nil || rec.Client == nil {
		t.Fatalf("after logout: %+v", rec)
	}
	if resp := call(t, fake, tok.AccessToken); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token still works: %d", resp.StatusCode)
	}
	// The next login reuses the client.
	login(t, c, fake, Conn{ID: "con_1", URL: fake.MCPURL()})
	if fake.Registered() != 1 {
		t.Fatalf("registrations = %d, want 1", fake.Registered())
	}
	if err := c.Forget(context.Background(), "con_1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.recs["con_1"]; ok {
		t.Fatal("Forget kept the record")
	}
}

func TestStepUp(t *testing.T) {
	t.Parallel()
	fake := oauthtest.New(t, oauthtest.Options{DCR: true, Scopes: []string{"read"}})
	c := NewClient(newMemStore(), nil, Config{})
	conn := Conn{ID: "con_1", URL: fake.MCPURL()}
	login(t, c, fake, conn)
	fake.NeedScope("write")
	tok, _ := c.Token(context.Background(), "con_1")
	resp := call(t, fake, tok)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	f, err := c.StepUp(context.Background(), conn, ReturnAddr{URL: testReturn}, resp.Header.Values("WWW-Authenticate"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.Scopes, "write") || !slices.Contains(f.Scopes, "read") {
		t.Fatalf("step-up scopes = %v", f.Scopes)
	}
	back, err := fake.Authorize(f.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Finish(context.Background(), f, back.Query().Get("code"), ""); err != nil {
		t.Fatal(err)
	}
	tok, _ = c.Token(context.Background(), "con_1")
	if resp := call(t, fake, tok); resp.StatusCode != http.StatusOK {
		t.Fatalf("after step-up: %d", resp.StatusCode)
	}
	if _, err := c.StepUp(context.Background(), conn, ReturnAddr{URL: testReturn}, []string{`Bearer realm="x"`}); err == nil {
		t.Fatal("StepUp took a challenge without insufficient_scope")
	}
}

func TestDetect(t *testing.T) {
	t.Parallel()
	open := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":0,"result":{}}`))
	}))
	t.Cleanup(open.Close)
	c := NewClient(newMemStore(), nil, Config{CIMDURL: "https://api.trebi.ai/oauth/client.json"})
	ctx := context.Background()
	if srv, err := c.Detect(ctx, open.URL); srv != nil || err != nil {
		t.Fatalf("open server: %+v, %v", srv, err)
	}
	fake := oauthtest.New(t, oauthtest.Options{CIMD: true, Scopes: []string{"read"}})
	srv, err := c.Detect(ctx, fake.MCPURL())
	if err != nil || srv == nil {
		t.Fatalf("Detect: %+v, %v", srv, err)
	}
	if srv.Issuer != fake.URL || srv.Resource != fake.MCPURL() || srv.Kind(true) != RegCIMD || srv.Kind(false) != "" {
		t.Fatalf("server = %+v", srv)
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html></html>"))
	}))
	t.Cleanup(page.Close)
	if _, err := c.Detect(ctx, page.URL); !errors.Is(err, ErrNotMCP) {
		t.Fatalf("web page: %v", err)
	}
	bare := oauthtest.New(t, oauthtest.Options{NoMetadata: true})
	if _, err := c.Detect(ctx, bare.MCPURL()); !errors.Is(err, ErrNoMetadata) {
		t.Fatalf("no metadata: %v", err)
	}
}

func TestSecretStore(t *testing.T) {
	t.Setenv("TREBI_HOME", t.TempDir())
	var st SecretStore
	if _, err := st.Load("con_1"); !errors.Is(err, ErrNoRecord) {
		t.Fatalf("empty Load: %v", err)
	}
	rec := Record{
		Server: &Server{Resource: "https://x/mcp", Issuer: "https://x"},
		Client: &Registration{Kind: RegDCR, ID: "c1", Issuer: "https://x"},
		Token:  &Token{AccessToken: "a", RefreshToken: "r"},
	}
	if err := st.Save("con_1", rec); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load("con_1")
	if err != nil || got.Client == nil || got.Client.ID != "c1" || got.Token.RefreshToken != "r" || got.Server.Issuer != "https://x" {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	rec.Token = nil
	if err := st.Save("con_1", rec); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Load("con_1"); got.Token != nil || got.Client == nil {
		t.Fatalf("after logout save: %+v", got)
	}
	if err := st.Save("con_1", rec); err != nil {
		t.Fatal(err)
	}
	if names, _ := secrets.List(); len(names) != 0 {
		t.Fatalf("List shows reserved names: %v", names)
	}
	if err := st.Delete("con_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Load("con_1"); !errors.Is(err, ErrNoRecord) {
		t.Fatalf("after Delete: %v", err)
	}
	if names, _ := secrets.List(); len(names) != 0 {
		t.Fatalf("List shows reserved names: %v", names)
	}
}

func newMemStore() *memStore { return &memStore{recs: map[string]Record{}} }

// login runs Begin, the fake browser, and Finish.
func login(t *testing.T, c *Client, fake *oauthtest.Server, conn Conn) *Flow {
	t.Helper()
	ctx := context.Background()
	f, err := c.Begin(ctx, conn, ReturnAddr{URL: testReturn})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	back, err := fake.Authorize(f.AuthURL)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if got := back.Query().Get("state"); got != f.State {
		t.Fatalf("state = %q, want %q", got, f.State)
	}
	if err := c.Finish(ctx, f, back.Query().Get("code"), back.Query().Get("iss")); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return f
}

// call sends one MCP request with the token and returns the status.
func call(t *testing.T, fake *oauthtest.Server, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, fake.MCPURL(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}
