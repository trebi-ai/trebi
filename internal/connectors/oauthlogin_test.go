package connectors_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/connectors/oauth"
	"github.com/trebi-ai/trebi/internal/connectors/oauth/oauthtest"
	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/secrets"
	"github.com/trebi-ai/trebi/internal/tools"
)

type fakeLoginCloud struct{ err error }

func (f fakeLoginCloud) LoginReturn() (string, string, error) {
	return "https://cloud.example/login/done", "dev1", f.err
}

func TestOnlineCheckAndCreate(t *testing.T) {
	_, _, svc := openStack(t)
	srv := oauthtest.New(t, oauthtest.Options{DCR: true})
	svc.WithOAuth(oauth.NewClient(oauth.SecretStore{}, srv.Client(), oauth.Config{}))
	ctx := context.Background()

	chk, err := svc.CheckSource(ctx, srv.MCPURL())
	if err != nil {
		t.Fatal(err)
	}
	if chk.Kind != connectors.SourceOnline || !chk.CanInstall || chk.Login == nil || !chk.Login.Needed || chk.Login.Registration != oauth.RegDCR || chk.Runs.Method != "online" {
		t.Fatalf("check = %+v login %+v", chk, chk.Login)
	}
	if _, err := svc.CreateOnline(ctx, connectors.Connection{}, "ftp://example.com"); err == nil {
		t.Fatal("ftp address added")
	}
	page := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(page.Close)
	var ve *connectors.ValidationError
	if _, err := svc.CheckSource(ctx, page.URL); !errors.As(err, &ve) {
		t.Fatalf("check of a web page: %v", err)
	}
	c, err := svc.CreateOnline(ctx, connectors.Connection{Title: "Granola"}, srv.MCPURL())
	if err != nil {
		t.Fatal(err)
	}
	if c.OAuth == nil || c.From != connectors.FromInline || c.Manifest.Actions.MCP.URL != srv.MCPURL() {
		t.Fatalf("connection = %+v", c)
	}
	methods, err := svc.LoginMethods(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(methods) != 1 || methods[0].ID != connectors.LoginOAuth || methods[0].Owner != connectors.OwnerDaemon || !methods[0].Available || methods[0].Label != "Log in with Granola" {
		t.Fatalf("methods = %+v", methods)
	}
	if got := loginState(t, svc, c.ID); got != protocol.StateAuthRequired {
		t.Fatalf("state before login = %q", got)
	}
}

func TestOAuthLoginReturn(t *testing.T) {
	svc, srv, c := onlineStack(t, oauthtest.Options{DCR: true})
	ctx := context.Background()

	f, ret := startOAuth(ctx, t, svc, srv, c.ID)
	if !strings.HasPrefix(ret.String(), "http://127.0.0.1:7433/login/done?") {
		t.Fatalf("return = %s", ret)
	}
	if got := loginState(t, svc, c.ID); got != protocol.StateAuthPending {
		t.Fatalf("state while waiting = %q", got)
	}
	res, err := svc.FinishLogin(ctx, returnOf(ret))
	if err != nil || res.ID != c.ID || res.Title != "Granola" {
		t.Fatalf("finish = %+v, %v", res, err)
	}
	if d := f.Current().Done; d == nil || !d.OK {
		t.Fatalf("done = %+v", d)
	}
	if _, err := svc.FinishLogin(ctx, returnOf(ret)); !errors.Is(err, connectors.ErrLoginGone) {
		t.Fatalf("second return: %v", err)
	}
	if got := loginState(t, svc, c.ID); got != protocol.StateConnected {
		t.Fatalf("state after login = %q", got)
	}
	v, err := svc.OAuthStatus(c.ID)
	if st := v.Status; err != nil || !st.LoggedIn || st.ClientKind != oauth.RegDCR {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if want := []string{"http://127.0.0.1:7433/login/done", "https://cloud.example/login/done"}; !slices.Equal(v.Returns, want) {
		t.Fatalf("returns = %v", v.Returns)
	}

	if err := svc.Logout(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if len(srv.Revoked()) == 0 {
		t.Fatal("logout revoked nothing")
	}
	if got := loginState(t, svc, c.ID); got != protocol.StateAuthRequired {
		t.Fatalf("state after logout = %q", got)
	}
}

func TestOAuthLoginPaste(t *testing.T) {
	svc, srv, c := onlineStack(t, oauthtest.Options{DCR: true})
	ctx := context.Background()

	f, ret := startOAuth(ctx, t, svc, srv, c.ID)
	other := *ret
	q := other.Query()
	q.Set("state", "other")
	other.RawQuery = q.Encode()
	for _, bad := range []string{"", other.String(), "http://127.0.0.1/login/done?state=x", "not a code?"} {
		if err := svc.SubmitLogin(ctx, f, map[string]protocol.Value{"code": {bad}}); protocol.CodeOf(err) != protocol.CodeInvalid {
			t.Fatalf("paste %q: %v", bad, err)
		}
	}
	if err := svc.SubmitLogin(ctx, f, map[string]protocol.Value{"code": {ret.String()}}); err != nil {
		t.Fatal(err)
	}
	if d := f.Current().Done; d == nil || !d.OK {
		t.Fatalf("done = %+v", d)
	}

	// A bare code works too.
	f, ret = startOAuth(ctx, t, svc, srv, c.ID)
	if err := svc.SubmitLogin(ctx, f, map[string]protocol.Value{"code": {ret.Query().Get("code")}}); err != nil {
		t.Fatal(err)
	}
	if d := f.Current().Done; d == nil || !d.OK {
		t.Fatalf("bare code done = %+v", d)
	}
}

func TestOAuthLoginDenied(t *testing.T) {
	svc, srv, c := onlineStack(t, oauthtest.Options{DCR: true})
	ctx := context.Background()
	f, ret := startOAuth(ctx, t, svc, srv, c.ID)
	r := returnOf(ret)
	r.Code, r.Error = "", "access_denied"
	if _, err := svc.FinishLogin(ctx, r); err == nil {
		t.Fatal("denied login finished")
	}
	if d := f.Current().Done; d == nil || d.OK || d.Error != "Granola said: you did not allow access." {
		t.Fatalf("done = %+v", d)
	}

	f, _ = startOAuth(ctx, t, svc, srv, c.ID)
	if err := svc.CancelLogin(ctx, f); err != nil {
		t.Fatal(err)
	}
	if got := loginState(t, svc, c.ID); got != protocol.StateAuthRequired {
		t.Fatalf("state after cancel = %q", got)
	}
}

func TestOAuthReturnAddress(t *testing.T) {
	svc, _, c := onlineStack(t, oauthtest.Options{DCR: true})
	phone := tools.WithActor(context.Background(), tools.Actor{Kind: tools.ActorMobile})

	f, err := svc.Login(phone, c.ID, connectors.LoginOAuth)
	if err != nil {
		t.Fatal(err)
	}
	step := f.Current().Step
	if !strings.Contains(step.Message, "Remote access is not active") || !strings.Contains(step.URL, url.QueryEscape("http://127.0.0.1:7433/login/done")) {
		t.Fatalf("phone step without cloud = %+v", step)
	}

	svc.WithLoginCloud(fakeLoginCloud{})
	f, err = svc.Login(phone, c.ID, connectors.LoginOAuth)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(f.Current().Step.URL)
	if q := u.Query(); q.Get("redirect_uri") != "https://cloud.example/login/done" || !strings.HasPrefix(q.Get("state"), "dev1.") {
		t.Fatalf("phone step with cloud = %s", u)
	}
}

func TestOAuthOwnAppAndDelete(t *testing.T) {
	app := &oauthtest.App{ID: "my-app", Secret: "s3cret"}
	svc, srv, c := onlineStack(t, oauthtest.Options{OwnApp: app})
	ctx := context.Background()

	if _, err := svc.Login(ctx, c.ID, connectors.LoginOAuth); protocol.CodeOf(err) != protocol.CodeInvalid || !strings.Contains(err.Error(), "lets only known apps log in") {
		t.Fatalf("login with no app: %v", err)
	}
	id, ownApp := "my-app", manifest.RegistrationOwnApp
	if err := svc.SetOAuth(ctx, c.ID, connectors.OAuthPatch{ClientID: &id, Registration: &ownApp, ClientSecret: "s3cret"}); err != nil {
		t.Fatal(err)
	}
	f, ret := startOAuth(ctx, t, svc, srv, c.ID)
	if _, err := svc.FinishLogin(ctx, returnOf(ret)); err != nil {
		t.Fatal(err)
	}
	if d := f.Current().Done; d == nil || !d.OK {
		t.Fatalf("done = %+v", d)
	}
	if names, _ := secrets.List(); len(names) != 0 {
		t.Fatalf("reserved secrets listed: %v", names)
	}
	if err := svc.Delete(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"oauth." + c.ID, "oauth." + c.ID + ".client", "oauth." + c.ID + ".app"} {
		if _, err := secrets.Get(name); err == nil {
			t.Fatalf("%s stays after delete", name)
		}
	}
}

func TestOAuthDoctor(t *testing.T) {
	svc, srv, c := onlineStack(t, oauthtest.Options{DCR: true, NoRefresh: true, AccessTTL: 5 * time.Minute})
	ctx := context.Background()
	codes := func() []string {
		var out []string
		for _, f := range svc.Doctor(ctx) {
			if f.Connector == c.ID && strings.HasPrefix(f.Code, "connector_") {
				out = append(out, f.Code)
			}
		}
		return out
	}
	if got := codes(); !slices.Equal(got, []string{"connector_login_required"}) {
		t.Fatalf("before login = %v", got)
	}
	_, ret := startOAuth(ctx, t, svc, srv, c.ID)
	if _, err := svc.FinishLogin(ctx, returnOf(ret)); err != nil {
		t.Fatal(err)
	}
	if got := codes(); len(got) != 0 {
		t.Fatalf("proxy mode = %v", got)
	}
	if _, err := svc.Patch(ctx, c.ID, connectors.ConnectionPatch{OAuth: json.RawMessage(`{"mode":"header"}`)}); err != nil {
		t.Fatal(err)
	}
	if got := codes(); !slices.Equal(got, []string{"connector_token_expiring"}) {
		t.Fatalf("header mode = %v", got)
	}
}

// onlineStack adds the fake server as an online connection.
func onlineStack(t *testing.T, opts oauthtest.Options) (*connectors.Service, *oauthtest.Server, connectors.Connection) {
	t.Helper()
	_, _, svc := openStack(t)
	srv := oauthtest.New(t, opts)
	svc.WithOAuth(oauth.NewClient(oauth.SecretStore{}, srv.Client(), oauth.Config{CloudReturn: "https://cloud.example/login/done"}))
	svc.SetLocalURL("http://127.0.0.1:7433")
	c, err := svc.CreateOnline(context.Background(), connectors.Connection{Title: "Granola"}, srv.MCPURL())
	if err != nil {
		t.Fatal(err)
	}
	return svc, srv, c
}

func loginState(t *testing.T, svc *connectors.Service, id string) string {
	t.Helper()
	row, err := svc.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return row["login"].(map[string]any)["state"].(string)
}

// startOAuth starts the login and plays the browser up to the return.
func startOAuth(ctx context.Context, t *testing.T, svc *connectors.Service, srv *oauthtest.Server, id string) (*connectors.Flow, *url.URL) {
	t.Helper()
	f, err := svc.Login(ctx, id, connectors.LoginOAuth)
	if err != nil {
		t.Fatal(err)
	}
	step := f.Current().Step
	if step == nil || step.Kind != protocol.StepURL || len(step.Fields) != 1 || step.Fields[0].Name != "code" {
		t.Fatalf("step = %+v", step)
	}
	ret, err := srv.Authorize(step.URL)
	if err != nil {
		t.Fatal(err)
	}
	return f, ret
}

func returnOf(u *url.URL) connectors.LoginReturn {
	q := u.Query()
	return connectors.LoginReturn{State: q.Get("state"), Code: q.Get("code"), Iss: q.Get("iss"), Error: q.Get("error")}
}
