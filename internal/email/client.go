package email

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
)

// APIError is a Worker error envelope: {error: {code, message}}.
type APIError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("mail worker %d %s: %s", e.Status, e.Code, e.Message)
}

// RetryAfterError marks a 429 from the platform. The Worker does not retry;
// the caller owns the backoff.
type RetryAfterError struct {
	After time.Duration
}

func (e *RetryAfterError) Error() string {
	return fmt.Sprintf("mail worker rate limited; retry after %s", e.After)
}

// Client speaks the Worker contract (testdata/contract/CONTRACT.md). Every
// route carries the shared bearer token.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// NewClient builds a Client against one Worker URL.
func NewClient(worker, token string) *Client {
	return &Client{
		base:  worker,
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

var _ Transport = (*Client)(nil)

// Pull reads spooled messages after seq, oldest first. It returns the
// messages and the seq of the last one, which is the next after value.
func (c *Client) Pull(ctx context.Context, after int64, limit int) ([]IngressMessage, int64, error) {
	q := url.Values{}
	q.Set("after", strconv.FormatInt(after, 10))
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out struct {
		Messages []IngressMessage `json:"messages"`
		Next     int64            `json:"next"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/ingress?"+q.Encode(), nil, &out); err != nil {
		return nil, after, err
	}
	return out.Messages, out.Next, nil
}

// Ack deletes messages through seq and replaces the known set in one
// Worker transaction. Send the full live local-part set every time.
func (c *Client) Ack(ctx context.Context, through int64, known []string) error {
	body, err := json.Marshal(map[string]any{"through": through, "known": known})
	if err != nil {
		return err
	}
	return c.call(ctx, http.MethodPost, "/v1/ingress/ack", body, nil)
}

// Send posts one message and returns the accepted Message-ID.
func (c *Client) Send(ctx context.Context, req SendRequest) (SendResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return SendResult{}, err
	}
	var out SendResult
	if err := c.call(ctx, http.MethodPost, "/v1/send", body, &out); err != nil {
		return SendResult{}, err
	}
	return out, nil
}

// Attachment streams one attachment's bytes from the Worker store. The
// key is "<messageID>/<attachmentID>"; the second result is the content
// type the store recorded.
func (c *Client) Attachment(ctx context.Context, key string) ([]byte, string, error) {
	messageID, attachmentID, ok := strings.Cut(key, "/")
	if !ok || messageID == "" || attachmentID == "" {
		return nil, "", fmt.Errorf("mail worker attachment key %q: want messageID/attachmentID", key)
	}
	path := fmt.Sprintf("/v1/attachments/%s/%s", url.PathEscape(messageID), url.PathEscape(attachmentID))
	req, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("mail worker attachment: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", apiError(resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, "", fmt.Errorf("mail worker attachment: %w", err)
	}
	return raw, resp.Header.Get("Content-Type"), nil
}

// Health reads the Worker fact sheet.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var out Health
	if err := c.call(ctx, http.MethodGet, "/v1/health", nil, &out); err != nil {
		return Health{}, err
	}
	return out, nil
}

func (c *Client) request(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// call performs one JSON round trip and maps the error envelope.
func (c *Client) call(ctx context.Context, method, path string, body []byte, out any) error {
	req, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("mail worker %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		after := parseRetryAfterMS(resp.Header.Get("Retry-After-Ms"))
		if raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<16)); rerr == nil {
			var env struct {
				RetryAfterMS int64 `json:"retry_after_ms"`
			}
			if json.Unmarshal(raw, &env) == nil && env.RetryAfterMS > 0 {
				after = time.Duration(env.RetryAfterMS) * time.Millisecond
			}
		}
		return &RetryAfterError{After: after}
	}
	if resp.StatusCode >= 300 {
		return apiError(resp)
	}
	if out == nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("mail worker %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("mail worker %s: %w", path, err)
	}
	return nil
}

func apiError(resp *http.Response) error {
	apiErr := &APIError{Status: resp.StatusCode}
	if raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16)); err == nil && len(raw) > 0 {
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
