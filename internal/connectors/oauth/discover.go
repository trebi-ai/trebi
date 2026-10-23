package oauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// ErrNoMetadata is a server that answers 401 with no protected resource
// metadata: Trebi cannot find where to log in.
var ErrNoMetadata = errors.New("the server needs a login but does not say where")

// probeBody is the first MCP request: an initialize that any server
// answers, or refuses with 401.
const probeBody = `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"trebi","version":"check"}}}`

type metaURL struct{ url, resource string }

// Detect asks the server for a login. It returns nil, nil when the server
// answers without one. A 401 gives the server metadata (RFC 9728), then
// the auth server metadata (RFC 8414, else OpenID discovery).
func (c *Client) Detect(ctx context.Context, serverURL string) (*Server, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL, strings.NewReader(probeBody))
	if err != nil {
		return nil, stepErr(stepDiscover, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, stepErr(stepDiscover, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // drain for reuse; the body is not needed
	_ = resp.Body.Close()                                         // a read body; the close error carries nothing
	if resp.StatusCode != http.StatusUnauthorized {
		return nil, nil
	}
	return c.discover(ctx, serverURL, resp.Header.Values("WWW-Authenticate"))
}

// discover reads the metadata for a server from a 401 challenge.
func (c *Client) discover(ctx context.Context, serverURL string, challenge []string) (*Server, error) {
	cs, _ := oauthex.ParseWWWAuthenticate(challenge) // a bad header only loses its hints
	var metaURL string
	var scopes []string
	for _, ch := range cs {
		if ch.Scheme != "bearer" {
			continue
		}
		metaURL = ch.Params["resource_metadata"]
		if s := ch.Params["scope"]; s != "" {
			scopes = strings.Fields(s)
		}
	}
	var prm *oauthex.ProtectedResourceMetadata
	for _, u := range resourceMetadataURLs(metaURL, serverURL) {
		if m, err := oauthex.GetProtectedResourceMetadata(ctx, u.url, u.resource, c.http); err == nil && len(m.AuthorizationServers) > 0 {
			prm = m
			break
		}
	}
	if prm == nil {
		return nil, stepErr(stepDiscover, ErrNoMetadata)
	}
	issuer := prm.AuthorizationServers[0]
	asm, err := auth.GetAuthServerMetadata(ctx, issuer, c.http)
	if err != nil {
		return nil, stepErr(stepDiscover, err)
	}
	if asm == nil {
		return nil, stepErr(stepDiscover, fmt.Errorf("the login server %s publishes no metadata", issuer))
	}
	if !slices.Contains(asm.CodeChallengeMethodsSupported, "S256") {
		return nil, stepErr(stepDiscover, fmt.Errorf("the login server %s does not support PKCE S256", issuer))
	}
	if len(scopes) == 0 {
		scopes = prm.ScopesSupported
	}
	return &Server{
		Resource:    prm.Resource,
		Issuer:      asm.Issuer,
		AuthURL:     asm.AuthorizationEndpoint,
		TokenURL:    asm.TokenEndpoint,
		RegisterURL: asm.RegistrationEndpoint,
		RevokeURL:   asm.RevocationEndpoint,
		Scopes:      scopes,
		CIMD:        asm.ClientIDMetadataDocumentSupported,
		IssParam:    asm.AuthorizationResponseIssParameterSupported,
		AuthMethods: asm.TokenEndpointAuthMethodsSupported,
	}, nil
}

// resourceMetadataURLs lists where the metadata can be: the header, the
// well-known path of the server path, then the well-known path at the root.
func resourceMetadataURLs(fromHeader, serverURL string) []metaURL {
	var out []metaURL
	if fromHeader != "" {
		out = append(out, metaURL{fromHeader, serverURL})
	}
	u, err := url.Parse(serverURL)
	if err != nil {
		return out
	}
	m := *u
	m.RawQuery, m.Fragment = "", ""
	if p := strings.Trim(u.Path, "/"); p != "" {
		m.Path = "/.well-known/oauth-protected-resource/" + p
		out = append(out, metaURL{m.String(), serverURL})
	}
	m.Path = "/.well-known/oauth-protected-resource"
	root := *u
	root.Path, root.RawQuery, root.Fragment = "", "", ""
	return append(out, metaURL{m.String(), root.String()})
}

// insufficientScope returns the scopes of a 403 insufficient_scope
// challenge (RFC 6750 §3.1). ok is false for any other challenge.
func insufficientScope(challenge []string) (scopes []string, ok bool) {
	cs, err := oauthex.ParseWWWAuthenticate(challenge)
	if err != nil {
		return nil, false
	}
	for _, ch := range cs {
		if ch.Scheme == "bearer" && ch.Params["error"] == "insufficient_scope" {
			return strings.Fields(ch.Params["scope"]), true
		}
	}
	return nil, false
}
