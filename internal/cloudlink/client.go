package cloudlink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/email"
)

const (
	apiBodyCap   = 64 << 20 // 64 MiB response cap, matching the mail client
	apiErrCap    = 1 << 16
	apiUserAgent = "trebid"
)

// APIError is a cloud error envelope: {"error":{"code","message"}}.
type APIError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("cloud api %d %s: %s", e.Status, e.Code, e.Message)
}

// RetryAfterError marks a 429. The client does not retry; the caller owns
// the backoff.
type RetryAfterError struct {
	After time.Duration
}

func (e *RetryAfterError) Error() string {
	return fmt.Sprintf("cloud api rate limited; retry after %s", e.After)
}

// Address is one hosted mail address the worker minted.
type Address struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	LocalPart   string `json:"local_part"`
	Address     string `json:"address"`
	CreatedAt   string `json:"created_at,omitempty"`
}

// Hook is one minted hosted webhook. URL holds the public receiver
// including the token: treat it like a webhook secret.
type Hook struct {
	ID    string `json:"id"`
	Token string `json:"token,omitempty"`
	URL   string `json:"url"`
}

// CodeMachineLimit is the error code of a register over the plan machine limit.
const CodeMachineLimit = "machine_limit"

// Roster is the account roster (machines-response.v1.json).
type Roster struct {
	Limit    int             `json:"limit"`
	Machines []RosterMachine `json:"machines"`
}

// RosterMachine is one machine of the account. LastSeen is nil when the
// machine never sent a heartbeat.
type RosterMachine struct {
	DeviceID     string               `json:"device_id"`
	Name         string               `json:"name"`
	Online       bool                 `json:"online"`
	LastSeen     *string              `json:"last_seen"`
	Role         string               `json:"role"`
	Capabilities *Capabilities        `json:"capabilities"`
	Workspaces   []WorkspaceIndexItem `json:"workspaces"`
}

// WorkspaceIndexItem is one workspace in the cloud index. It has no content.
type WorkspaceIndexItem struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Handle string `json:"handle"`
}

// WorkspaceIndex is the body of PUT /v1/workspaces.
type WorkspaceIndex struct {
	Workspaces []WorkspaceIndexItem `json:"workspaces"`
}

// Client speaks the signed cloud API (testdata/contract/CONTRACT.md).
// Every route except Register carries the four device signature headers.
type Client struct {
	ep   *endpoints
	id   Identity
	http *http.Client
}

// NewClient builds a Client against the cloud API addresses in ep.
func NewClient(ep *endpoints, id Identity) *Client {
	return &Client{ep: ep, id: id, http: newHTTPClient()}
}

// Register exchanges an enroll code for a device id. It is the only
// unsigned call: the device has no id yet. The body never carries a
// private key field; the worker refuses one.
func (c *Client) Register(ctx context.Context, enrollToken, name string) (string, error) {
	body, err := json.Marshal(map[string]string{
		"enrollment_token": enrollToken,
		"name":             name,
		"public_key":       c.id.PublicKeyB64(),
	})
	if err != nil {
		return "", err
	}
	var out struct {
		DeviceID string `json:"device_id"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/devices", body, &out, false); err != nil {
		return "", err
	}
	return out.DeviceID, nil
}

// MintAddress mints one hosted mail address, <handle>-<token>. The worker
// replaces the earlier address of the same device and workspace.
func (c *Client) MintAddress(ctx context.Context, workspaceID, handle string) (Address, error) {
	body, err := json.Marshal(map[string]string{"workspace_id": workspaceID, "handle": handle})
	if err != nil {
		return Address{}, err
	}
	var out Address
	if err := c.call(ctx, http.MethodPost, "/v1/addresses", body, &out, true); err != nil {
		return Address{}, err
	}
	return out, nil
}

// ListAddresses lists the account's hosted addresses. The deployed worker
// serves this list from a browser session only; it is still implemented as
// a signed device call so a device-routed list works when the worker grows
// one. Tests pin the wire shape against a fake server.
func (c *Client) ListAddresses(ctx context.Context) ([]Address, error) {
	var out struct {
		Addresses []Address `json:"addresses"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/addresses", nil, &out, true); err != nil {
		return nil, err
	}
	return out.Addresses, nil
}

// DeleteAddress removes one hosted address.
func (c *Client) DeleteAddress(ctx context.Context, id string) error {
	path := "/v1/addresses/" + url.PathEscape(id)
	return c.call(ctx, http.MethodDelete, path, nil, nil, true)
}

// MintHookRequest names a hook. Headers are the extra request headers the
// cloud forwards; Handshake holds the rules the cloud answers itself.
type MintHookRequest struct {
	Label     string   `json:"label"`
	Headers   []string `json:"headers,omitempty"`
	Handshake []string `json:"handshake,omitempty"`
}

// MintHook mints one hosted webhook and returns its receiver URL once.
func (c *Client) MintHook(ctx context.Context, req MintHookRequest) (Hook, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Hook{}, err
	}
	var out Hook
	if err := c.call(ctx, http.MethodPost, "/v1/hooks", body, &out, true); err != nil {
		return Hook{}, err
	}
	return out, nil
}

// ListHooks lists the account's hosted webhooks. As with ListAddresses the
// deployed worker is session-only today; the call stays signed and device
// shaped.
func (c *Client) ListHooks(ctx context.Context) ([]Hook, error) {
	var out struct {
		Hooks []Hook `json:"hooks"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/hooks", nil, &out, true); err != nil {
		return nil, err
	}
	return out.Hooks, nil
}

// DeleteHook revokes one hosted webhook.
func (c *Client) DeleteHook(ctx context.Context, id string) error {
	path := "/v1/hooks/" + url.PathEscape(id)
	return c.call(ctx, http.MethodDelete, path, nil, nil, true)
}

// MintClaim mints a one-time pairing claim (POST /v1/clients/claims).
func (c *Client) MintClaim(ctx context.Context, uses int, ttl time.Duration) (Claim, error) {
	body, err := json.Marshal(ClaimMintRequest{Uses: uses, TTLSeconds: int(ttl / time.Second)})
	if err != nil {
		return Claim{}, err
	}
	var out Claim
	if err := c.call(ctx, http.MethodPost, "/v1/clients/claims", body, &out, true); err != nil {
		return Claim{}, err
	}
	return out, nil
}

// RevokeClient removes a phone from the account (DELETE /v1/clients/{id}).
func (c *Client) RevokeClient(ctx context.Context, cloudClientID string) error {
	return c.call(ctx, http.MethodDelete, "/v1/clients/"+url.PathEscape(cloudClientID), nil, nil, true)
}

// Roster reads the machines of the account (GET /v1/machines).
func (c *Client) Roster(ctx context.Context) (Roster, error) {
	var out Roster
	if err := c.call(ctx, http.MethodGet, "/v1/machines", nil, &out, true); err != nil {
		return Roster{}, err
	}
	return out, nil
}

// PublishWorkspaces replaces the workspace index of this machine
// (PUT /v1/workspaces).
func (c *Client) PublishWorkspaces(ctx context.Context, items []WorkspaceIndexItem) error {
	if items == nil {
		items = []WorkspaceIndexItem{}
	}
	body, err := json.Marshal(WorkspaceIndex{Workspaces: items})
	if err != nil {
		return err
	}
	return c.call(ctx, http.MethodPut, "/v1/workspaces", body, nil, true)
}

// Send posts one message and returns the accepted Message-ID.
func (c *Client) Send(ctx context.Context, req email.SendRequest) (email.SendResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return email.SendResult{}, err
	}
	var out email.SendResult
	if err := c.call(ctx, http.MethodPost, "/v1/send", body, &out, true); err != nil {
		return email.SendResult{}, err
	}
	return out, nil
}

// Attachment reads one attachment's bytes and content type. The key is
// "<messageID>/<attachmentID>".
func (c *Client) Attachment(ctx context.Context, key string) ([]byte, string, error) {
	messageID, attachmentID, ok := strings.Cut(key, "/")
	if !ok || messageID == "" || attachmentID == "" || strings.Contains(attachmentID, "/") {
		return nil, "", fmt.Errorf("cloud api attachment: malformed key %q", key)
	}
	path := "/v1/attachments/" + url.PathEscape(messageID) + "/" + url.PathEscape(attachmentID)
	resp, err := c.do(ctx, http.MethodGet, path, nil, true)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", apiError(resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, apiBodyCap))
	if err != nil {
		return nil, "", fmt.Errorf("cloud api %s: %w", path, err)
	}
	return raw, resp.Header.Get("Content-Type"), nil
}

// do sends one request. When the connection to the active address does
// not open, it sends the request again to the next address, with a new
// signature and nonce. Any other error is returned as is.
func (c *Client) do(ctx context.Context, method, path string, body []byte, signed bool) (*http.Response, error) {
	base := c.ep.Current()
	for tries := 1; ; tries++ {
		req, err := c.request(ctx, base, method, path, body, signed)
		if err != nil {
			return nil, err
		}
		resp, err := c.http.Do(req)
		if err == nil {
			return resp, nil
		}
		if tries < c.ep.Len() && ctx.Err() == nil && connectFailed(err) {
			if next, ok := c.ep.Failed(base); ok && next != base {
				base = next
				continue
			}
		}
		return nil, fmt.Errorf("cloud api %s: %w", path, err)
	}
}

func (c *Client) request(ctx context.Context, base, method, path string, body []byte, signed bool) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", apiUserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if signed {
		SignRequest(req, c.id, time.Now().UTC())
	}
	return req, nil
}

// call performs one JSON round trip and maps the error envelope.
func (c *Client) call(ctx context.Context, method, path string, body []byte, out any, signed bool) error {
	resp, err := c.do(ctx, method, path, body, signed)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return retryAfterError(resp)
	}
	if resp.StatusCode >= 300 {
		return apiError(resp)
	}
	if out == nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, apiBodyCap))
	if err != nil {
		return fmt.Errorf("cloud api %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("cloud api %s: %w", path, err)
	}
	return nil
}

// retryAfterError reads the 429 backoff from Retry-After-Ms or a
// retry_after_ms body, defaulting to one second.
func retryAfterError(resp *http.Response) error {
	after := parseRetryAfterMS(resp.Header.Get("Retry-After-Ms"))
	if raw, rerr := io.ReadAll(io.LimitReader(resp.Body, apiErrCap)); rerr == nil {
		var env struct {
			RetryAfterMS int64 `json:"retry_after_ms"`
		}
		if json.Unmarshal(raw, &env) == nil && env.RetryAfterMS > 0 {
			after = time.Duration(env.RetryAfterMS) * time.Millisecond
		}
	}
	return &RetryAfterError{After: after}
}

func apiError(resp *http.Response) error {
	apiErr := &APIError{Status: resp.StatusCode}
	if raw, err := io.ReadAll(io.LimitReader(resp.Body, apiErrCap)); err == nil && len(raw) > 0 {
		var env struct {
			Error *APIError `json:"error"`
		}
		if json.Unmarshal(raw, &env) == nil && env.Error != nil {
			env.Error.Status = resp.StatusCode
			return env.Error
		}
	}
	if apiErr.Code == "" {
		apiErr.Code = "unreachable"
		apiErr.Message = resp.Status
	}
	return apiErr
}

func parseRetryAfterMS(v string) time.Duration {
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil || ms <= 0 {
		return time.Second
	}
	return time.Duration(ms) * time.Millisecond
}
