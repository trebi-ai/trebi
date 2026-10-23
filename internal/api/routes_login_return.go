package api

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"

	"github.com/trebi-ai/trebi/internal/connectors"
)

// routeLoginReturn serves the page that the browser comes back to after an
// OAuth login. It is outside /v1 and has no auth: the state is the secret.
func (s *Server) routeLoginReturn(mux *http.ServeMux) {
	mux.HandleFunc("GET "+connectors.ReturnPath, s.handleLoginReturn)
}

// loginPage is the return page. It never shows the code.
//
//nolint:gochecknoglobals // parsed once, never mutated
var loginPage = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Heading}}</title>
<style>
:root{color-scheme:light dark;--bg:#f6f5f2;--fg:#1d1c1a;--muted:#6b6862;--card:#fff;--line:#e3e0da;--accent:#2f5d50}
@media (prefers-color-scheme:dark){:root{--bg:#161615;--fg:#ecebe8;--muted:#a19e97;--card:#1f1f1d;--line:#33322f;--accent:#7fb3a3}}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,sans-serif;display:grid;place-items:center;min-height:100vh;padding:16px;box-sizing:border-box}
main{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:28px;max-width:420px;width:100%}
h1{font-size:19px;margin:0 0 8px}p{margin:0 0 16px;color:var(--muted)}
a{display:inline-block;background:var(--accent);color:var(--card);text-decoration:none;padding:8px 14px;border-radius:8px;font-weight:600}
</style></head><body><main>
<h1>{{.Heading}}</h1>
<p>{{.Text}}</p>
{{if .Back}}<a href="{{.Back}}">Back to Trebi</a>{{end}}
</main></body></html>`))

type loginPageData struct {
	Heading string
	Text    string
	Back    string
}

func (s *Server) handleLoginReturn(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ret := connectors.LoginReturn{
		State: q.Get("state"), Code: q.Get("code"), Iss: q.Get("iss"),
		Error: q.Get("error"), ErrorDescription: q.Get("error_description"),
	}
	res, err := s.connectors.FinishLogin(r.Context(), ret)
	page := loginPageData{Heading: "You are logged in to " + res.Title, Text: "Go back to Trebi. You can close this tab."}
	status := http.StatusOK
	switch {
	case errors.Is(err, connectors.ErrLoginGone):
		page = loginPageData{Heading: "This login is not open", Text: "The login expired or finished. Go back to Trebi and start it again."}
		status = http.StatusNotFound
	case err != nil:
		page = loginPageData{Heading: "The login did not finish", Text: err.Error() + " Go back to Trebi and try again."}
		status = http.StatusBadRequest
	}
	if res.ID != "" {
		page.Back = "/connectors/" + url.PathEscape(res.ID) + "?tab=login"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	if err := loginPage.Execute(w, page); err != nil {
		s.log.Warn("connector.login_return", "err", err)
	}
}
