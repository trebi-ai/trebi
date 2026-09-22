package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/cloudlink"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/secrets"
)

// fakeCloud is the hosted worker under test: register plus the hosted
// object mint/revoke calls, scripted. It never checks signatures.
type fakeCloud struct {
	mu         sync.Mutex
	srv        *httptest.Server
	registered string // last enrollment_token seen at register
	nextAddr   string
	nextHook   string
	hookCalls  int
}

// TestSystemVersionFields pins the release contract: version is always
// present, ui_version only when a UI is embedded (Part C of the release
// versioning plan).
func TestSystemVersionFields(t *testing.T) {
	oldVersion, oldUI := api.Version, api.UIVersion
	t.Cleanup(func() { api.Version, api.UIVersion = oldVersion, oldUI })
	api.Version, api.UIVersion = "0.2.1", ""

	_, base := startTestServer(t)

	code, body, _ := doJSON(t, "GET", base+"/v1/system/health", nil)
	if code != 200 {
		t.Fatalf("health %d %v", code, body)
	}
	if body["version"] != "0.2.1" {
		t.Fatalf("health version=%v", body["version"])
	}
	if body["edition"] != "free" {
		t.Fatalf("health edition=%v", body["edition"])
	}
	if _, ok := body["edition_version"]; ok {
		t.Fatalf("free build must omit edition_version: %v", body)
	}
	if _, ok := body["ui_version"]; ok {
		t.Fatalf("plain build must omit ui_version: %v", body)
	}

	api.UIVersion = "0.5.3"
	code, body, _ = doJSON(t, "GET", base+"/v1/system/health", nil)
	if code != 200 || body["ui_version"] != "0.5.3" {
		t.Fatalf("health with ui %d %v", code, body)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/system/overview", nil)
	if code != 200 || body["ui_version"] != "0.5.3" {
		t.Fatalf("overview with ui %d %v", code, body)
	}
}

func TestSystemUpdateStubHasSource(t *testing.T) {
	_, base := startTestServer(t)
	code, body, _ := doJSON(t, "GET", base+"/v1/system/update", nil)
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	for _, k := range []string{"current", "latest", "update_available", "source"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("missing %s in %v", k, body)
		}
	}
	if body["update_available"] != false {
		t.Fatalf("stub must not claim an update: %v", body)
	}
	if body["current"] != body["latest"] {
		t.Fatalf("stub latest must equal current: %v", body)
	}
}

func TestSystemLogsFilters(t *testing.T) {
	_, base := startTestServer(t)
	logDir := filepath.Join(home.Base(), "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 0; i < 10; i++ {
		b.WriteString(`{"level":"INFO","msg":"keep"}` + "\n")
	}
	b.WriteString(`{"level":"WARN","msg":"careful"}` + "\n")
	b.WriteString(`{"level":"ERROR","msg":"boom hello"}` + "\n")
	b.WriteString("plain hello line\n")
	if err := os.WriteFile(filepath.Join(logDir, "trebid.log"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	code, body, _ := doJSON(t, "GET", base+"/v1/system/logs?lines=3", nil)
	if code != 200 {
		t.Fatalf("lines %d %v", code, body)
	}
	got := logLines(t, body)
	if len(got) != 3 {
		t.Fatalf("lines=3 got %d %v", len(got), got)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/system/logs?grep=hello", nil)
	if code != 200 {
		t.Fatalf("grep %d %v", code, body)
	}
	got = logLines(t, body)
	if len(got) != 2 {
		t.Fatalf("grep hello got %d %v", len(got), got)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/system/logs?level=warn", nil)
	if code != 200 {
		t.Fatalf("level %d %v", code, body)
	}
	got = logLines(t, body)
	if len(got) != 1 || !strings.Contains(got[0], "WARN") {
		t.Fatalf("level warn got %v", got)
	}

	var many strings.Builder
	for i := 0; i < 250; i++ {
		many.WriteString("line\n")
	}
	if err := os.WriteFile(filepath.Join(logDir, "trebid.log"), []byte(many.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/system/logs?lines=500", nil)
	if code != 200 {
		t.Fatalf("cap %d %v", code, body)
	}
	got = logLines(t, body)
	if len(got) != 200 {
		t.Fatalf("cap 200 got %d", len(got))
	}
}

func TestSystemLogsRunFilter(t *testing.T) {
	_, base := startTestServer(t)
	logDir := filepath.Join(home.Base(), "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Join([]string{
		`time=2026-09-15T15:05:15-03:00 level=INFO msg=api.request source=http_server route=/v1/runs/01RUN status=200`,
		`time=2026-09-15T15:05:16-03:00 level=INFO msg="run launched" source=runs id=01RUN tmux=trebi-1 harness=claude`,
		`time=2026-09-15T15:05:17-03:00 level=INFO msg="run launched" source=runs id=01OTHER tmux=trebi-2 harness=codex`,
		`time=2026-09-15T15:14:51-03:00 level=INFO msg="run finished" source=runs id=01RUN status=complete end_reason=sentinel`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(logDir, "trebid.log"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	code, resp, _ := doJSON(t, "GET", base+"/v1/system/logs?run=01RUN", nil)
	if code != 200 {
		t.Fatalf("run filter %d %v", code, resp)
	}
	got := logLines(t, resp)
	if len(got) != 2 {
		t.Fatalf("run filter got %d %v", len(got), got)
	}
	for i, line := range got {
		if i == 0 && !strings.Contains(line, "run launched") {
			t.Fatalf("launch line missing: %v", got)
		}
		if i == 1 && !strings.Contains(line, "run finished") {
			t.Fatalf("finish line missing: %v", got)
		}
		if !strings.Contains(line, "01RUN") || strings.Contains(line, "01OTHER") ||
			strings.Contains(line, "msg=api.request") {
			t.Fatalf("line should be filtered: %q", line)
		}
	}

	code, resp, _ = doJSON(t, "GET", base+"/v1/system/logs?run=01RUN&grep=finished", nil)
	if code != 200 {
		t.Fatalf("run+grep %d %v", code, resp)
	}
	if got = logLines(t, resp); len(got) != 1 || !strings.Contains(got[0], "run finished") {
		t.Fatalf("run+grep got %v", got)
	}
}

// TestCloudStatusUnlinkedAnswers pins Part G: unlinked is 200 with
// linked:false and the whole shape present, never a 404.
func TestCloudStatusUnlinkedAnswers(t *testing.T) {
	_, base := startTestServer(t)
	code, body, _ := doJSON(t, "GET", base+"/v1/cloud/status", nil)
	if code != http.StatusOK {
		t.Fatalf("status %d %v", code, body)
	}
	if body["linked"] != false || body["status"] != "not_linked" {
		t.Fatalf("unlinked shape: %v", body)
	}
	for _, k := range []string{"account_email", "device_name", "plan", "features", "connected", "refreshed_at"} {
		if _, ok := body[k]; !ok {
			t.Errorf("status is missing %q: %v", k, body)
		}
	}
}

// TestCloudLinkAddressesUnlink walks the link flow end to end against the
// fake worker: the account page's grouped code registers, the cached
// state serves the UI, addresses mask, and unlink clears the key.
func TestCloudLinkAddressesUnlink(t *testing.T) {
	s, base, _ := startAPI(t)
	f := newFakeCloud(t)
	s.Cloud().Configure(cloudlink.Settings{APIURL: f.srv.URL, Enabled: false})

	code, body, _ := doJSON(t, "POST", base+"/v1/cloud/link",
		map[string]string{"enrollment_token": "ABCD-EFGH-0123", "name": "mbp"})
	if code != http.StatusOK {
		t.Fatalf("link %d %v", code, body)
	}
	if f.registered != "abcdefgh0123" {
		t.Fatalf("register token = %q; display grouping must normalize", f.registered)
	}
	if body["linked"] != true || body["status"] != "linked" || body["device_name"] != "mbp" {
		t.Fatalf("link shape: %v", body)
	}

	// A second link is a conflict; the caller unlinks first.
	code, body, _ = doJSON(t, "POST", base+"/v1/cloud/link",
		map[string]string{"enrollment_token": "ABCD-EFGH-0123"})
	if code != http.StatusConflict || errCode(body) != "conflict" {
		t.Fatalf("second link: %d %v", code, body)
	}

	// The minted hosted address serves masked, and reveal returns it full.
	if _, err := s.Cloud().MintAddress(t.Context(), "ws_1"); err != nil {
		t.Fatal(err)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/cloud/addresses", nil)
	if code != http.StatusOK {
		t.Fatalf("addresses %d %v", code, body)
	}
	rows, _ := body["addresses"].([]any)
	if len(rows) != 1 {
		t.Fatalf("addresses: %v", body)
	}
	row, _ := rows[0].(map[string]any)
	if row["masked"] != true || row["address"] == "a-abc123@mail.example.com" {
		t.Fatalf("masked row: %v", row)
	}
	if row["workspace_id"] != "ws_1" || row["local_part"] == "" {
		t.Fatalf("row shape: %v", row)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/cloud/addresses?reveal=1", nil)
	if code != http.StatusOK {
		t.Fatalf("reveal %d %v", code, body)
	}
	rows, _ = body["addresses"].([]any)
	row, _ = rows[0].(map[string]any)
	if row["masked"] != false || row["address"] != "a-abc123@mail.example.com" {
		t.Fatalf("revealed row: %v", row)
	}

	// Unlink is confirm-gated (confirm_test covers the gate), drops the
	// row, and deletes the device key from the secrets directory.
	code, body, _ = doJSON(t, "DELETE", base+"/v1/cloud/link?confirm=1", nil)
	if code != http.StatusOK || body["linked"] != false {
		t.Fatalf("unlink %d %v", code, body)
	}
	if _, err := secrets.Get("cloud_device_key"); err == nil {
		t.Fatal("device key survived unlink")
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/cloud/status", nil)
	if code != http.StatusOK || body["linked"] != false || body["status"] != "not_linked" {
		t.Fatalf("status after unlink: %d %v", code, body)
	}
}

// TestCloudLinkNeedsToken pins the validation contract: the code is
// required and empty groups do not pass.
func TestCloudLinkNeedsToken(t *testing.T) {
	s, base, _ := startAPI(t)
	f := newFakeCloud(t)
	s.Cloud().Configure(cloudlink.Settings{APIURL: f.srv.URL, Enabled: false})
	for _, code := range []string{"", "----"} {
		status, body, _ := doJSON(t, "POST", base+"/v1/cloud/link",
			map[string]string{"enrollment_token": code})
		if status != http.StatusBadRequest || errCode(body) != "validation" {
			t.Fatalf("enrollment code %q: %d %v", code, status, body)
		}
	}
}

func logLines(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, ok := body["lines"].([]any)
	if !ok {
		t.Fatalf("lines missing: %v", body)
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

func newFakeCloud(t *testing.T) *fakeCloud {
	t.Helper()
	f := &fakeCloud{nextAddr: "a-abc123", nextHook: "tok-1"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/devices":
			var body struct {
				EnrollmentToken string `json:"enrollment_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.registered = body.EnrollmentToken
			_ = json.NewEncoder(w).Encode(map[string]string{"device_id": "01DEVICE0000000000000000AA"})
		case r.Method == "POST" && r.URL.Path == "/v1/addresses":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"id": "01ADDR000000000000000000AA", "local_part": f.nextAddr,
				"address": f.nextAddr + "@mail.example.com",
			})
		case r.Method == "POST" && r.URL.Path == "/v1/hooks":
			f.hookCalls++
			_ = json.NewEncoder(w).Encode(map[string]string{
				"id": "01HOOK00000000000000000AA", "token": f.nextHook,
				"url": "https://in.example.com/hook/" + f.nextHook,
			})
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/v1/hooks/"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/v1/addresses/"):
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}
