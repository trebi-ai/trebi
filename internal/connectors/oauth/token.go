package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// tokenBodyCap bounds a token endpoint answer.
const tokenBodyCap = 1 << 20

// tokenRequest posts a grant to the token endpoint. asked are the scopes
// of the request; a server that names none granted them all.
func (c *Client) tokenRequest(ctx context.Context, srv Server, client Registration, form url.Values, asked []string) (*Token, error) {
	var tr struct {
		AccessToken  string          `json:"access_token"`
		TokenType    string          `json:"token_type"`
		RefreshToken string          `json:"refresh_token"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
		Scope        string          `json:"scope"`
	}
	if err := c.post(ctx, srv, client, srv.TokenURL, form, &tr); err != nil {
		return nil, err
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("the answer has no access_token")
	}
	if tr.TokenType != "" && !strings.EqualFold(tr.TokenType, "bearer") {
		return nil, fmt.Errorf("token type %q is not bearer", tr.TokenType)
	}
	now := c.cfg.Now()
	t := &Token{AccessToken: tr.AccessToken, RefreshToken: tr.RefreshToken, TokenType: "Bearer", ObtainedAt: now, Scopes: slices.Clone(asked)}
	// Some servers send expires_in as a string.
	if n, err := strconv.ParseInt(strings.Trim(string(tr.ExpiresIn), `"`), 10, 64); err == nil && n > 0 {
		t.Expiry = now.Add(time.Duration(n) * time.Second)
	}
	if tr.Scope != "" {
		t.Scopes = strings.Fields(tr.Scope)
	}
	return t, nil
}

// revoke asks the server to end one token (RFC 7009).
func (c *Client) revoke(ctx context.Context, srv Server, client Registration, token, hint string) error {
	return c.post(ctx, srv, client, srv.RevokeURL, url.Values{"token": {token}, "token_type_hint": {hint}}, nil)
}

// post sends a form with the client auth and decodes a JSON answer into
// out. A public client sends its id in the form. A client with a secret
// uses client_secret_post when the server names only that, else HTTP
// basic (RFC 6749 §2.3.1).
func (c *Client) post(ctx context.Context, srv Server, client Registration, endpoint string, form url.Values, out any) error {
	postOnly := slices.Contains(srv.AuthMethods, "client_secret_post") && !slices.Contains(srv.AuthMethods, "client_secret_basic")
	basic := client.Secret != "" && !postOnly
	if basic {
		form.Del("client_id")
	} else {
		form.Set("client_id", client.ID)
		if client.Secret != "" {
			form.Set("client_secret", client.Secret)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic {
		req.SetBasicAuth(url.QueryEscape(client.ID), url.QueryEscape(client.Secret))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // a read body; the close error carries nothing
	body, err := io.ReadAll(io.LimitReader(resp.Body, tokenBodyCap))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		re := &ResponseError{Status: resp.StatusCode}
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		if json.Unmarshal(body, &e) == nil {
			re.Code, re.Description = e.Error, e.Description
		}
		return re
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("read the answer: %w", err)
	}
	return nil
}

// randomString returns n random bytes in base64url with no padding.
func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// challengeS256 is the PKCE S256 challenge of a verifier (RFC 7636).
func challengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
