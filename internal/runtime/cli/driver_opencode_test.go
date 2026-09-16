package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

func testOpenCodeServer(t *testing.T, handler http.Handler) *openCodeServer {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	u, _ := url.Parse(ts.URL)
	port, _ := strconv.Atoi(u.Port())
	return &openCodeServer{port: port, username: "opencode", password: "pw", key: "test"}
}

// TestOpenCodeSSEParsing frames server-sent events.
func TestOpenCodeSSEParsing(t *testing.T) {
	t.Parallel()
	srv := testOpenCodeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, line := range []string{
			"event: message\ndata: {\"type\":\"server.connected\",\"properties\":{}}\n\n",
			"data: {\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"ses_1\"}}\n\n",
		} {
			_, _ = w.Write([]byte(line))
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch, err := srv.stream(ctx, "/tmp/ws")
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for frame := range ch {
		var m map[string]any
		if err := json.Unmarshal(frame.Data, &m); err != nil {
			t.Fatalf("frame: %v", err)
		}
		types = append(types, str(m["type"]))
		if len(types) == 2 {
			break
		}
	}
	if len(types) != 2 || types[0] != "server.connected" || types[1] != "session.idle" {
		t.Fatalf("types: %v", types)
	}
}

// TestOpenCodeSessionEventMapping pins the bus-event → normalized-event shape.
func TestOpenCodeSessionEventMapping(t *testing.T) {
	t.Parallel()
	s := &openCodeSession{id: "ses_1", parts: map[string]int{}}
	cases := []struct {
		name string
		in   map[string]any
		want EventType
	}{
		{"idle", map[string]any{"type": "session.idle", "properties": map[string]any{"sessionID": "ses_1"}}, EventResult},
		{"text part", map[string]any{"type": "message.part.updated", "properties": map[string]any{"part": map[string]any{
			"id": "p1", "type": "text", "text": "hello", "sessionID": "ses_1",
		}}}, EventAssistant},
		{"tool part", map[string]any{"type": "message.part.updated", "properties": map[string]any{"part": map[string]any{
			"id": "p2", "type": "tool", "tool": "bash", "sessionID": "ses_1",
			"state": map[string]any{"status": "completed", "output": "ok"},
		}}}, EventTool},
		{"permission", map[string]any{"type": "permission.updated", "properties": map[string]any{
			"id": "perm_1", "title": "Allow edit", "sessionID": "ses_1",
		}}, EventPermission},
		{"other session", map[string]any{"type": "session.idle", "properties": map[string]any{"sessionID": "ses_2"}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evs := s.parse(tc.in)
			if tc.want == "" {
				if len(evs) != 0 {
					t.Fatalf("expected filtered, got %+v", evs)
				}
				return
			}
			if len(evs) == 0 || evs[0].Type != tc.want {
				t.Fatalf("got %+v want %s", evs, tc.want)
			}
		})
	}
	// Repeated text part updates emit only the suffix.
	first := s.parse(map[string]any{"type": "message.part.updated", "properties": map[string]any{"part": map[string]any{
		"id": "p3", "type": "text", "text": "abc", "sessionID": "ses_1",
	}}})
	second := s.parse(map[string]any{"type": "message.part.updated", "properties": map[string]any{"part": map[string]any{
		"id": "p3", "type": "text", "text": "abcdef", "sessionID": "ses_1",
	}}})
	if len(first) != 1 || first[0].Text != "abc" {
		t.Fatalf("first: %+v", first)
	}
	if len(second) != 1 || second[0].Text != "def" || !second[0].Delta {
		t.Fatalf("second: %+v", second)
	}
}

// TestOpenCodeFingerprintSharesRunScope pins E.3: per-run values must not
// split the shared server; real config differences still do.
func TestOpenCodeFingerprintSharesRunScope(t *testing.T) {
	t.Parallel()
	runA := map[string]string{
		"OPENCODE_CONFIG": "/tmp/cfg.json", "PATH": "/bin",
		"TREBI_RUN_ID": "run-1", "TREBI_HOOK_TOOL": "opencode", "TREBI_MEMORY_PATH": "/tmp/m1",
	}
	runB := map[string]string{
		"OPENCODE_CONFIG": "/tmp/cfg.json", "PATH": "/bin",
		"TREBI_RUN_ID": "run-2", "TREBI_HOOK_TOOL": "opencode",
	}
	if a, b := openCodeFingerprint("/bin/opencode", runA, nil), openCodeFingerprint("/bin/opencode", runB, nil); a != b {
		t.Fatalf("run-scoped env split the server:\n%s\n%s", a, b)
	}
	otherCfg := map[string]string{"OPENCODE_CONFIG": "/tmp/other.json", "PATH": "/bin"}
	if a, c := openCodeFingerprint("/bin/opencode", runA, nil), openCodeFingerprint("/bin/opencode", otherCfg, nil); a == c {
		t.Fatal("different config must not share a server")
	}
	jobEnv := map[string]string{"OPENCODE_CONFIG": "/tmp/cfg.json", "PATH": "/bin", "FOO": "bar"}
	if a, d := openCodeFingerprint("/bin/opencode", runA, nil), openCodeFingerprint("/bin/opencode", jobEnv, nil); a == d {
		t.Fatal("job env must not share a server")
	}
}

// TestOpenCodeHTTPCallAuthAndDirectory pins basic auth + directory header.
func TestOpenCodeHTTPCallAuthAndDirectory(t *testing.T) {
	t.Parallel()
	var gotAuth string
	var gotDir string
	srv := testOpenCodeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotDir = r.Header.Get("x-opencode-directory")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ses_9"})
	}))
	var out struct {
		ID string `json:"id"`
	}
	if err := srv.call(context.Background(), "POST", "/session", "/tmp/ws", map[string]any{}, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "ses_9" {
		t.Fatalf("id: %q", out.ID)
	}
	if gotDir != "/tmp/ws" {
		t.Fatalf("directory header: %q", gotDir)
	}
	if gotAuth == "" || gotAuth[:6] != "Basic " {
		t.Fatalf("basic auth missing: %q", gotAuth)
	}
}
