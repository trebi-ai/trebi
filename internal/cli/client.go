package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

// APIClient talks to trebid over HTTP.
type APIClient struct {
	Base   string
	Token  string
	Remote bool // true when sticky/linked remote workspace is active
	HTTP   *http.Client
}

// LoadAPIClient resolves the daemon for the active workspace context:
//  1. -w / TREBI_WORKSPACE matching a linked remote workspace
//  2. sticky context with remote URL
//  3. TREBI_URL + TREBI_TOKEN
//  4. local ~/.trebi/api.json
func LoadAPIClient() (*APIClient, error) {
	hint := strings.TrimSpace(os.Getenv("TREBI_WORKSPACE"))
	sticky, err := LoadStickyContext()
	if err != nil {
		return nil, err
	}
	linked, linkedErr := LoadLinkedWorkspaces()
	if linkedErr != nil {
		linked = nil
	}
	return loadAPIClientWithContext(hint, sticky, linked, os.Getenv("TREBI_URL"), os.Getenv("TREBI_TOKEN"))
}

// IsRemote reports whether this client targets a remote daemon.
func (c *APIClient) IsRemote() bool {
	return c != nil && c.Remote
}

// EffectiveWorkspace returns the workspace id for API query params using the
// process environment and persisted workspace context.
func EffectiveWorkspace(c interface{ String(string) string }) string {
	hint := strings.TrimSpace(os.Getenv("TREBI_WORKSPACE"))
	if c != nil {
		if w := strings.TrimSpace(c.String("workspace")); w != "" {
			hint = w
		}
	}
	sticky, stickyErr := LoadStickyContext()
	if stickyErr != nil {
		sticky = nil
	}
	linked, linkedErr := LoadLinkedWorkspaces()
	if linkedErr != nil {
		linked = nil
	}
	return effectiveWorkspaceFor(hint, sticky, linked)
}

// GetJSON performs GET and decodes JSON.
func (c *APIClient) GetJSON(path string, out any) error {
	req, err := http.NewRequest("GET", c.Base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("GET %s: %s: %s", path, resp.Status, body)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// PostJSON performs POST with optional JSON body.
func (c *APIClient) PostJSON(path string, body any, out any) error {
	return c.doJSON("POST", path, body, out)
}

// PutJSON performs PUT with optional JSON body.
func (c *APIClient) PutJSON(path string, body any, out any) error {
	return c.doJSON("PUT", path, body, out)
}

// PatchJSON performs PATCH with optional JSON body.
func (c *APIClient) PatchJSON(path string, body any, out any) error {
	return c.doJSON("PATCH", path, body, out)
}

// Delete performs DELETE.
func (c *APIClient) Delete(path string) error {
	return c.doJSON("DELETE", path, nil, nil)
}

// GetRaw performs GET and returns the response body bytes (any content-type).
func (c *APIClient) GetRaw(path string) ([]byte, error) {
	req, err := http.NewRequest("GET", c.Base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GET %s: %s: %s", path, resp.Status, raw)
	}
	return raw, nil
}

// StreamGET copies a GET response body to dst until EOF or ctx cancel.
// Used for integration logs --follow (SSE). Timeout is disabled on this call.
func (c *APIClient) StreamGET(path string, dst io.Writer) error {
	req, err := http.NewRequest("GET", c.Base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	// Clone without timeout for long-lived streams.
	streamClient := *httpClient
	streamClient.Timeout = 0
	resp, err := streamClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s: %s: %s", path, resp.Status, raw)
	}
	_, err = io.Copy(dst, resp.Body)
	return err
}

const streamScannerMax = 1 << 20 // 1 MiB; attrs are small, but one malformed frame must not truncate

// StreamEvents follows an SSE GET, decoding each `data:` line as a bus frame.
// The HTTP client is cloned with Timeout=0 so the default 30s timeout cannot kill the tail.
func (c *APIClient) StreamEvents(ctx context.Context, path string, fn func(store.StreamEvent) error) error {
	return c.streamEvents(ctx, path, false, fn)
}

// DoHTTP executes a pure HTTPRequest against this client.
func (c *APIClient) DoHTTP(r HTTPRequest, out any) error {
	switch strings.ToUpper(r.Method) {
	case "GET":
		return c.GetJSON(r.Path, out)
	case "POST":
		return c.PostJSON(r.Path, r.Body, out)
	case "PUT":
		return c.PutJSON(r.Path, r.Body, out)
	case "PATCH":
		return c.PatchJSON(r.Path, r.Body, out)
	case "DELETE":
		return c.Delete(r.Path)
	default:
		return fmt.Errorf("unsupported method %s", r.Method)
	}
}

func (c *APIClient) streamEvents(ctx context.Context, path string, skipCatchup bool, fn func(store.StreamEvent) error) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.Base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	streamClient := *httpClient
	streamClient.Timeout = 0
	resp, err := streamClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s: %s: %s", path, resp.Status, raw)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 256*1024), streamScannerMax)
	live := !skipCatchup
	lastData := time.Now()
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var ev store.StreamEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return fmt.Errorf("decode bus frame: %w", err)
		}
		now := time.Now()
		if !live {
			if now.Sub(lastData) < 100*time.Millisecond {
				lastData = now
				continue
			}
			live = true
		}
		lastData = now
		if err := fn(ev); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.EOF
}

func (c *APIClient) doJSON(method, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, c.Base+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, raw)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// apiJSONClient reads local ~/.trebi/api.json.
func apiJSONClient() (*APIClient, error) {
	data, err := os.ReadFile(home.APIJSONPath())
	if err != nil {
		return nil, fmt.Errorf("reading api.json: %w (is trebid running?)", err)
	}
	var doc struct {
		URL   string `json:"url"`
		Token string `json:"token"`
		Port  int    `json:"port"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	base := doc.URL
	if base == "" && doc.Port > 0 {
		base = fmt.Sprintf("http://127.0.0.1:%d", doc.Port)
	}
	return &APIClient{
		Base:  base,
		Token: doc.Token,
		HTTP:  &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func remoteClient(url, token string) *APIClient {
	return &APIClient{
		Base:   strings.TrimRight(url, "/"),
		Token:  token,
		Remote: true,
		HTTP:   &http.Client{Timeout: 30 * time.Second},
	}
}

func equalFoldName(a, b string) bool {
	return b != "" && strings.EqualFold(a, b)
}
