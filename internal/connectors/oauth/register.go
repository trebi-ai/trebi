package oauth

import (
	"context"
	"errors"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// Kind returns the registration that a login with this server uses, in
// the order own app, CIMD, DCR. It is "" when Trebi has no way, so the
// user needs their own app. cimd tells whether Trebi has a CIMD document.
func (s Server) Kind(cimd bool) string {
	switch {
	case s.CIMD && cimd:
		return RegCIMD
	case s.RegisterURL != "":
		return RegDCR
	}
	return ""
}

// client picks the client for a login: the own app of the connection,
// then a stored client that still fits, then CIMD, then DCR.
func (c *Client) client(ctx context.Context, conn Conn, srv Server, stored *Registration, ret string) (*Registration, error) {
	mode := conn.Registration
	if mode == "" {
		mode = RegAuto
	}
	allow := func(kind string) bool { return mode == RegAuto || mode == kind }
	if conn.OwnApp != nil && conn.OwnApp.ClientID != "" && allow(RegOwnApp) {
		return &Registration{Kind: RegOwnApp, ID: conn.OwnApp.ClientID, Secret: conn.OwnApp.ClientSecret, Issuer: srv.Issuer}, nil
	}
	if mode == RegOwnApp {
		return nil, stepErr(stepRegister, ErrNoRegistration)
	}
	if r := stored; r != nil && r.Issuer == srv.Issuer && allow(r.Kind) && c.fits(*r, ret) {
		return r, nil
	}
	return c.newClient(ctx, srv, ret, allow)
}

// newClient gets a client with CIMD, else with DCR.
func (c *Client) newClient(ctx context.Context, srv Server, ret string, allow func(string) bool) (*Registration, error) {
	if allow(RegCIMD) && srv.CIMD && c.cfg.CIMDURL != "" {
		return &Registration{Kind: RegCIMD, ID: c.cfg.CIMDURL, Issuer: srv.Issuer}, nil
	}
	if allow(RegDCR) && srv.RegisterURL != "" {
		return c.register(ctx, srv, ret)
	}
	return nil, stepErr(stepRegister, ErrNoRegistration)
}

// fits reports whether a stored client works for a return address.
func (c *Client) fits(r Registration, ret string) bool {
	switch r.Kind {
	case RegCIMD:
		return r.ID == c.cfg.CIMDURL && c.cfg.CIMDURL != ""
	case RegDCR:
		expired := !r.SecretExpiry.IsZero() && !c.cfg.Now().Before(r.SecretExpiry)
		return slices.Contains(r.RedirectURIs, ret) && !expired
	}
	return false
}

// register gets a public client with dynamic client registration (RFC
// 7591). It registers the cloud return page too.
func (c *Client) register(ctx context.Context, srv Server, ret string) (*Registration, error) {
	uris := union([]string{ret, c.cfg.CloudReturn})
	meta := &oauthex.ClientRegistrationMetadata{
		RedirectURIs:            uris,
		TokenEndpointAuthMethod: "none",
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		ClientName:              c.cfg.ClientName,
		ClientURI:               "https://trebi.ai",
	}
	resp, err := oauthex.RegisterClient(ctx, srv.RegisterURL, meta, c.http)
	if err != nil {
		var re *oauthex.ClientRegistrationError
		if errors.As(err, &re) {
			return nil, stepErr(stepRegister, &ResponseError{Code: re.ErrorCode, Description: re.ErrorDescription})
		}
		return nil, stepErr(stepRegister, err)
	}
	if len(resp.RedirectURIs) > 0 {
		uris = resp.RedirectURIs
	}
	return &Registration{
		Kind: RegDCR, ID: resp.ClientID, Secret: resp.ClientSecret, Issuer: srv.Issuer,
		RedirectURIs: uris, SecretExpiry: resp.ClientSecretExpiresAt,
	}, nil
}
