package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/trebi-ai/trebi/internal/assistant"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/store"
)

// APIClient talks to trebid over HTTP.
type APIClient struct {
	base       string
	remote     bool // true when sticky/linked remote workspace is active
	httpClient *http.Client
}

// IsRemote reports whether this client targets a remote daemon.
func (c *APIClient) IsRemote() bool {
	return c != nil && c.remote
}

// GetJSON performs GET and decodes JSON.
func (c *APIClient) GetJSON(path string, out any) error {
	req, err := http.NewRequest("GET", c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
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
	req, err := http.NewRequest("GET", c.base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
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
// Used for connector logs --follow (SSE). Timeout is disabled on this call.
func (c *APIClient) StreamGET(path string, dst io.Writer) error {
	req, err := http.NewRequest("GET", c.base+path, nil)
	if err != nil {
		return err
	}
	httpClient := c.httpClient
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

// StreamConversationEvents follows one conversation event stream (SSE). Each
// frame carries one assistant.Event body; ping comments are skipped. Frames
// with no data line are dropped.
func (c *APIClient) StreamConversationEvents(ctx context.Context, path string, fn func(assistant.Event) error) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+path, nil)
	if err != nil {
		return err
	}
	httpClient := c.httpClient
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
	var data []string
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		var ev assistant.Event
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return fmt.Errorf("decode assistant event: %w", err)
		}
		return fn(ev)
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "" || strings.HasPrefix(line, ":"):
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

// DoHTTP executes a pure request.HTTPRequest against this client.
func (c *APIClient) DoHTTP(r request.HTTPRequest, out any) error {
	if r.Raw != nil || r.IfMatch != "" {
		return c.doRaw(r, out)
	}
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

// WorkspaceIDFor maps a workspace name or id hint to an id via GET /v1/workspaces.
func (c *APIClient) WorkspaceIDFor(hint string) string {
	if c == nil || hint == "" {
		return ""
	}
	var body struct {
		Workspaces []map[string]any `json:"workspaces"`
	}
	if err := c.GetJSON(request.BuildWorkspacesListPath(0), &body); err != nil {
		return ""
	}
	for _, ws := range body.Workspaces {
		id := mapStr(ws, "id")
		name := mapStr(ws, "name")
		if id == hint || equalFoldName(hint, name) {
			return id
		}
	}
	return ""
}

// WaitTmuxSession polls the run until tmux_session is set or the run leaves starting/running.
func (c *APIClient) WaitTmuxSession(runID string, maxWait time.Duration) string {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		var body map[string]any
		if err := c.GetJSON(request.BuildRunGetPath(runID, 0), &body); err == nil {
			if s := mapStr(body, "tmux_session"); s != "" {
				return s
			}
			st := mapStr(body, "status")
			if st != "" && st != "running" && st != "starting" {
				return ""
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return ""
}

// DialAttach opens a WebSocket to GET /v1/runs/{id}/attach.
func (c *APIClient) DialAttach(runID string) (*websocket.Conn, error) {
	return c.DialWS("/v1/runs/" + url.PathEscape(runID) + "/attach")
}

// DialWS opens a WebSocket to path on this client's base URL.
func (c *APIClient) DialWS(path string) (*websocket.Conn, error) {
	wsURL, err := httpToWS(c.base + path)
	if err != nil {
		return nil, err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, resp, err := dialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close() // response body is closed after reporting the handshake failure
			if readErr != nil {
				return nil, fmt.Errorf("attach ws: %w (read handshake response: %v)", err, readErr)
			}
			return nil, fmt.Errorf("attach ws: %w (%s: %s)", err, resp.Status, body)
		}
		return nil, fmt.Errorf("attach ws: %w", err)
	}
	return conn, nil
}

// MessageIDSet returns the set of message ids currently in a conversation.
func (c *APIClient) MessageIDSet(convID string) map[string]struct{} {
	out := map[string]struct{}{}
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := c.GetJSON(request.BuildConversationMessagesPath(convID, "", 200), &body); err != nil {
		return out
	}
	for _, m := range body.Messages {
		out[mapStr(m, "id")] = struct{}{}
	}
	return out
}

// CLIConversation returns the conversation the cli channel points at, or "" when
// none exists yet. The daemon creates one on the first assistant message.
func (c *APIClient) CLIConversation(wsID string) string {
	var ptr map[string]any
	if err := c.DoHTTP(request.BuildPointerGet("cli", wsID, "", wsID), &ptr); err != nil {
		return ""
	}
	return mapStr(ptr, "conversation_id")
}

// DoctorFix applies one doctor finding by id.
func (c *APIClient) DoctorFix(id string) (string, map[string]any) {
	var out map[string]any
	if err := c.DoHTTP(request.BuildDoctorFix(id), &out); err != nil {
		return fmt.Sprintf("%s: error: %v", id, err), map[string]any{"id": id, "error": err.Error()}
	}
	if fixed, ok := out["fixed"]; ok {
		switch v := fixed.(type) {
		case bool:
			if !v {
				note := mapStr(out, "note")
				return fmt.Sprintf("%s: skip (%s)", id, dash(note)), out
			}
		case string:
			if v == "false" {
				return fmt.Sprintf("%s: skip (%s)", id, dash(mapStr(out, "note"))), out
			}
		}
	}
	action := mapStr(out, "action")
	if action == "" {
		action = "fixed"
	}
	return fmt.Sprintf("%s: %s", id, action), out
}

// ScheduleIndex maps workspace/job keys to schedule columns from GET /v1/schedules.
func (c *APIClient) ScheduleIndex() (map[string]JobScheduleCols, error) {
	var body struct {
		Schedules []map[string]any `json:"schedules"`
	}
	if err := c.GetJSON(request.BuildSchedulesListPath(200, "", ""), &body); err != nil {
		return nil, err
	}
	return indexSchedules(body.Schedules), nil
}

// RunLogText decodes GET /v1/runs/{id}/log JSON {"log":"…"} or falls back to transcript raw.
func (c *APIClient) RunLogText(runID string) (string, error) {
	raw, err := c.GetRaw(request.BuildRunLogPath(runID, false))
	if err == nil {
		var wrap map[string]any
		if json.Unmarshal(raw, &wrap) == nil {
			if _, has := wrap["log"]; has {
				return fmt.Sprint(wrap["log"]), nil
			}
		}
		if len(raw) > 0 {
			return string(raw), nil
		}
	}
	raw, err = c.GetRaw(request.BuildRunTranscript(runID, false).Path)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// FindWorkspace resolves name and path for an existing workspace id on this client.
func (c *APIClient) FindWorkspace(id, remoteURL string) (name, path string, err error) {
	var body struct {
		Workspaces []map[string]any `json:"workspaces"`
	}
	if err := c.GetJSON(request.BuildWorkspacesListPath(0), &body); err != nil {
		return "", "", err
	}
	for _, ws := range body.Workspaces {
		if mapStr(ws, "id") == id {
			return mapStr(ws, "name"), mapStr(ws, "path"), nil
		}
	}
	return "", "", fmt.Errorf("workspace id %q not found on %s", id, remoteURL)
}

// doRaw executes a file-plane request: raw body and/or If-Match header.
// doRaw sends a request that carries an If-Match, a raw body, or both. A
// request with If-Match and a JSON Body still needs that body encoded.
func (c *APIClient) doRaw(r request.HTTPRequest, out any) error {
	method := strings.ToUpper(r.Method)
	payload := r.Raw
	contentType := r.ContentType
	if payload == nil && r.Body != nil {
		var err error
		if payload, err = json.Marshal(r.Body); err != nil {
			return err
		}
		contentType = "application/json"
	}
	req, err := http.NewRequest(method, c.base+r.Path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	if payload != nil {
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		req.Header.Set("Content-Type", contentType)
	}
	if r.IfMatch != "" {
		req.Header.Set("If-Match", r.IfMatch)
	}
	if token := os.Getenv("TREBI_RUN_TOKEN"); token != "" {
		req.Header.Set("X-Trebi-Run-Token", token)
	}
	resp, err := c.httpFor(method, r.Path).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s %s: %s: %s", method, r.Path, resp.Status, raw)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func (c *APIClient) streamEvents(ctx context.Context, path string, skipCatchup bool, fn func(store.StreamEvent) error) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+path, nil)
	if err != nil {
		return err
	}
	httpClient := c.httpClient
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
	req, err := http.NewRequest(method, c.base+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpFor(method, path).Do(req)
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
		URL  string `json:"url"`
		Port int    `json:"port"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	base := doc.URL
	if base == "" && doc.Port > 0 {
		base = fmt.Sprintf("http://127.0.0.1:%d", doc.Port)
	}
	return &APIClient{
		base:       base,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func remoteClient(url string) *APIClient {
	return &APIClient{
		base:       strings.TrimRight(url, "/"),
		remote:     true,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

const (
	defaultHTTPTimeout = 30 * time.Second
	launchHTTPTimeout  = 120 * time.Second
	// waitHTTPSlack covers the round trip on top of the server-side wait_ms.
	waitHTTPSlack = 30 * time.Second
)

func (c *APIClient) httpFor(method, path string) *http.Client {
	h := c.httpClient
	if h == nil {
		h = &http.Client{Timeout: defaultHTTPTimeout}
	}
	want := time.Duration(0)
	if strings.EqualFold(method, "POST") && launchHTTPPath(path) {
		want = launchHTTPTimeout
	}
	if d := waitHTTPTimeout(path); d > want {
		want = d
	}
	if want == 0 || h.Timeout == 0 || h.Timeout >= want {
		return h
	}
	cp := *h
	cp.Timeout = want
	return &cp
}

// waitHTTPTimeout gives a long-poll request room for its own wait_ms.
func waitHTTPTimeout(path string) time.Duration {
	i := strings.IndexByte(path, '?')
	if i < 0 {
		return 0
	}
	q, err := url.ParseQuery(path[i+1:])
	if err != nil {
		return 0
	}
	ms, err := strconv.Atoi(q.Get("wait_ms"))
	if err != nil || ms <= 0 {
		return 0
	}
	return time.Duration(ms)*time.Millisecond + waitHTTPSlack
}

func launchHTTPPath(path string) bool {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	switch {
	case path == "/v1/runs":
		return true
	case strings.HasSuffix(path, "/retry"), strings.HasSuffix(path, "/replay"), strings.HasSuffix(path, "/resume"):
		return strings.Contains(path, "/v1/runs/")
	default:
		return false
	}
}

func equalFoldName(a, b string) bool {
	return b != "" && strings.EqualFold(a, b)
}
