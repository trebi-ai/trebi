package registry

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"github.com/trebi-ai/trebi/internal/config"
)

// smithery reads the Smithery registry API (registry.smithery.ai). The API
// has no versions. Only an HTTP connection is installable; a stdio
// connection is an .mcpb bundle, which this client does not run.
type smithery struct {
	s   *Service
	reg config.Registry
}

// Search pages by page number. The cursor is the next page number.
func (m smithery) Search(ctx context.Context, q Query) (Page, error) {
	v := url.Values{}
	if strings.TrimSpace(q.Text) != "" {
		v.Set("q", q.Text)
	}
	if q.Cursor != "" {
		v.Set("page", q.Cursor)
	}
	if q.Limit > 0 {
		v.Set("pageSize", strconv.Itoa(min(q.Limit, 100)))
	}
	var out smitheryList
	if err := m.s.fetchJSON(ctx, m.reg, "/servers", v, &out); err != nil {
		return Page{}, err
	}
	page := Page{}
	for _, it := range out.Servers {
		page.Entries = append(page.Entries, m.entry(it))
	}
	if p := out.Pagination; p.CurrentPage < p.TotalPages {
		page.Next = strconv.Itoa(p.CurrentPage + 1)
	}
	return page, nil
}

// Get reads one server by its qualified name. version is ignored.
func (m smithery) Get(ctx context.Context, name, _ string) (Entry, error) {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	var out smitheryServer
	if err := m.s.fetchJSON(ctx, m.reg, "/servers/"+strings.Join(parts, "/"), nil, &out); err != nil {
		return Entry{}, err
	}
	e := m.entry(out)
	for _, c := range out.Connections {
		if c.Type == "http" && c.DeploymentURL != "" {
			e.Server.Remotes = append(e.Server.Remotes, Remote{Type: "streamable-http", URL: c.DeploymentURL})
		}
	}
	return e, nil
}

func (m smithery) entry(it smitheryServer) Entry {
	trust := TrustCommunity
	if it.Verified {
		trust = TrustVerified
	}
	srv := ServerJSON{Name: it.QualifiedName, Title: it.DisplayName, Description: it.Description}
	if it.Homepage != "" {
		srv.Repository = &Repository{URL: it.Homepage}
	}
	return Entry{Registry: m.reg.Name, Server: srv, Trust: trust}
}

// smitheryList is GET /servers.
type smitheryList struct {
	Servers    []smitheryServer `json:"servers"`
	Pagination struct {
		CurrentPage int `json:"currentPage"`
		TotalPages  int `json:"totalPages"`
	} `json:"pagination"`
}

// smitheryServer is one list row or GET /servers/{qualifiedName}.
type smitheryServer struct {
	QualifiedName string `json:"qualifiedName"`
	DisplayName   string `json:"displayName"`
	Description   string `json:"description"`
	Homepage      string `json:"homepage"`
	Verified      bool   `json:"verified"`
	Connections   []struct {
		Type          string `json:"type"`
		DeploymentURL string `json:"deploymentUrl"`
	} `json:"connections"`
}
