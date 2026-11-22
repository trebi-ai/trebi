package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/cloudlink"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/routing"
	"github.com/trebi-ai/trebi/internal/routing/routingtest"
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
	full       []byte // register answers 403 with this body when set
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
	s.Cloud().Configure(cloudlink.Settings{APIURLs: []string{f.srv.URL}, Enabled: false})

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
	if _, err := s.Cloud().MintAddress(t.Context(), "ws_1", "acme"); err != nil {
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

// TestCloudLinkMachineLimit shows the cloud message when the plan has no
// free machine slot.
func TestCloudLinkMachineLimit(t *testing.T) {
	s, base, _ := startAPI(t)
	f := newFakeCloud(t)
	full, err := os.ReadFile("../cloudlink/testdata/contract/machine-limit-error.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	f.full = full
	s.Cloud().Configure(cloudlink.Settings{APIURLs: []string{f.srv.URL}, Enabled: false})
	code, body, _ := doJSON(t, "POST", base+"/v1/cloud/link", map[string]string{"enrollment_token": "ABCD-EFGH-0123"})
	if code != http.StatusPreconditionFailed || errCode(body) != "precondition_failed" {
		t.Fatalf("link %d %v", code, body)
	}
	msg, _ := body["error"].(map[string]any)["message"].(string)
	if !strings.HasPrefix(msg, "This plan allows 1 machine.") {
		t.Fatalf("message = %q", msg)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/cloud/status", nil)
	if code != http.StatusOK || body["linked"] != false {
		t.Fatalf("status after refused link: %d %v", code, body)
	}
}

// TestCloudLinkNeedsToken pins the validation contract: the code is
// required and empty groups do not pass.
func TestCloudLinkNeedsToken(t *testing.T) {
	s, base, _ := startAPI(t)
	f := newFakeCloud(t)
	s.Cloud().Configure(cloudlink.Settings{APIURLs: []string{f.srv.URL}, Enabled: false})
	for _, code := range []string{"", "----"} {
		status, body, _ := doJSON(t, "POST", base+"/v1/cloud/link",
			map[string]string{"enrollment_token": code})
		if status != http.StatusBadRequest || errCode(body) != "validation" {
			t.Fatalf("enrollment code %q: %d %v", code, status, body)
		}
	}
}

func TestDoctorRouting(t *testing.T) {
	base, _, wsPath := routingFixture(t)
	routingtest.Write(t, wsPath, "gmail", routing.File{Routes: []routing.Route{
		{ID: "rt_aaaaaaaaaa", Agent: "gone", Gets: routing.GetsOff},
	}})
	_, body, _ := doJSON(t, "GET", base+"/v1/system/doctor", nil)
	rows, _ := body["findings"].([]any)
	var found []map[string]any
	for _, r := range rows {
		if m, _ := r.(map[string]any); m["code"] == routing.CodeRouteTargetMissing {
			found = append(found, m)
		}
	}
	if len(found) != 1 || found[0]["severity"] != "error" {
		t.Fatalf("findings = %v", found)
	}
	msg, _ := found[0]["message"].(string)
	if want := "Run: trebi connector routing remove gmail rt_aaaaaaaaaa --yes --workspace rt"; !strings.Contains(msg, want) {
		t.Fatalf("message = %q, want %q", msg, want)
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
			if f.full != nil {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write(f.full)
				return
			}
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

// TestCloudFallbackDoctor pins the fallback finding: it appears only after
// a connection error moved the link to the backup address.
func TestCloudFallbackDoctor(t *testing.T) {
	s, base, _ := startAPI(t)
	f := newFakeCloud(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // a closed port: the dial is refused
	s.Cloud().Configure(cloudlink.Settings{APIURLs: []string{deadURL, f.srv.URL}, Enabled: false})

	hasFinding := func() bool {
		_, body, _ := doJSON(t, "GET", base+"/v1/system/doctor", nil)
		rows, _ := body["findings"].([]any)
		for _, r := range rows {
			if m, _ := r.(map[string]any); m["code"] == "cloud_fallback_active" {
				return true
			}
		}
		return false
	}
	if hasFinding() {
		t.Fatal("fallback finding before any failover")
	}
	code, body, _ := doJSON(t, "POST", base+"/v1/cloud/link",
		map[string]string{"enrollment_token": "ABCD-EFGH-0123", "name": "mbp"})
	if code != http.StatusOK {
		t.Fatalf("link through the fallback: %d %v", code, body)
	}
	if body["fallback"] != true || body["endpoint"] != strings.TrimPrefix(f.srv.URL, "http://") {
		t.Fatalf("status after failover: %v", body)
	}
	if !hasFinding() {
		t.Fatal("no fallback finding while on the backup address")
	}
	if got := s.Cloud().APIHost(); got != strings.TrimPrefix(deadURL, "http://") {
		t.Fatalf("APIHost = %q; the pairing link must name the primary host", got)
	}
}

// TestCloudLinkRequestRoutes covers the link by email routes against a
// fake worker: 202, 400, 409, 412, 429, resend, and cancel.
func TestCloudLinkRequestRoutes(t *testing.T) {
	s, base, _ := startAPI(t)
	var (
		mu     sync.Mutex
		limit  bool
		resend int
		name   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/devices/requests":
			if limit {
				w.Header().Set("Retry-After-Ms", "90000")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			var body struct{ Name string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			name = body.Name
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"request_id":"req1","poll_secret":"sec","poll_after_ms":60000,"expires_at":"` +
				time.Now().Add(30*time.Minute).UTC().Format(time.RFC3339) + `"}`))
		case r.Method == "GET" && r.URL.Path == "/v1/devices/requests/req1":
			_, _ = w.Write([]byte(`{"status":"pending"}`))
		case r.Method == "POST" && r.URL.Path == "/v1/devices/requests/req1/resend":
			resend++
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	s.Cloud().Configure(cloudlink.Settings{APIURLs: []string{srv.URL}, Enabled: false})
	t.Cleanup(s.Cloud().Stop)

	// Resend and cancel with nothing open.
	code, body, _ := doJSON(t, "POST", base+"/v1/cloud/link-request/resend", nil)
	if code != http.StatusPreconditionFailed || errCode(body) != "precondition_failed" {
		t.Fatalf("resend with nothing open: %d %v", code, body)
	}
	if code, _, _ := doJSON(t, "DELETE", base+"/v1/cloud/link-request", nil); code != http.StatusNoContent {
		t.Fatalf("cancel with nothing open: %d", code)
	}

	// 400 for a bad email.
	for _, email := range []string{"", "not-an-email", "Ellis <ellis@example.com>"} {
		code, body, _ = doJSON(t, "POST", base+"/v1/cloud/link-request", map[string]string{"email": email})
		if code != http.StatusBadRequest || errCode(body) != "validation" {
			t.Fatalf("email %q: %d %v", email, code, body)
		}
	}

	// 429 carries the backoff.
	mu.Lock()
	limit = true
	mu.Unlock()
	code, body, hdr := doJSON(t, "POST", base+"/v1/cloud/link-request", map[string]string{"email": "ellis@example.com"})
	if code != http.StatusTooManyRequests || errCode(body) != "rate_limited" || hdr.Get("Retry-After") != "90" {
		t.Fatalf("rate limit: %d %v %v", code, body, hdr)
	}
	mu.Lock()
	limit = false
	mu.Unlock()

	// 202 with the pending status; the name defaults.
	code, body, _ = doJSON(t, "POST", base+"/v1/cloud/link-request", map[string]string{"email": "ellis@example.com"})
	if code != http.StatusAccepted {
		t.Fatalf("request: %d %v", code, body)
	}
	pending, _ := body["pending"].(map[string]any)
	if body["linked"] != false || pending["email"] != "ellis@example.com" || pending["status"] != "pending" || pending["request_id"] != "req1" {
		t.Fatalf("request shape: %v", body)
	}
	mu.Lock()
	if name == "" {
		t.Error("the machine name did not default")
	}
	mu.Unlock()
	code, body, _ = doJSON(t, "GET", base+"/v1/cloud/status", nil)
	if p, _ := body["pending"].(map[string]any); code != http.StatusOK || p["status"] != "pending" {
		t.Fatalf("status: %d %v", code, body)
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/cloud/link-request/resend", nil)
	if code != http.StatusOK || body["pending"] == nil {
		t.Fatalf("resend: %d %v", code, body)
	}
	mu.Lock()
	if resend != 1 {
		t.Errorf("resend calls = %d", resend)
	}
	mu.Unlock()

	if code, _, _ := doJSON(t, "DELETE", base+"/v1/cloud/link-request", nil); code != http.StatusNoContent {
		t.Fatalf("cancel: %d", code)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/cloud/status", nil)
	if code != http.StatusOK || body["pending"] != nil {
		t.Fatalf("status after cancel: %d %v", code, body)
	}

	// 409 when the machine is linked.
	f := newFakeCloud(t)
	s.Cloud().Configure(cloudlink.Settings{APIURLs: []string{f.srv.URL}, Enabled: false})
	if code, body, _ := doJSON(t, "POST", base+"/v1/cloud/link", map[string]string{"enrollment_token": "ABCD-EFGH"}); code != http.StatusOK {
		t.Fatalf("link: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "POST", base+"/v1/cloud/link-request", map[string]string{"email": "ellis@example.com"})
	if code != http.StatusConflict || errCode(body) != "conflict" {
		t.Fatalf("request while linked: %d %v", code, body)
	}
}

func TestSystemNotifications(t *testing.T) {
	_, base := startTestServer(t)

	code, body, _ := doJSON(t, "GET", base+"/v1/system/notifications", nil)
	if code != 200 || body["status"] != "unknown" || body["tray"] != false {
		t.Fatalf("before a report: %d %v", code, body)
	}
	if code, body, _ = doJSON(t, "PUT", base+"/v1/system/notifications", map[string]any{"status": "maybe"}); code != 400 {
		t.Fatalf("bad status: %d %v", code, body)
	}
	resp, err := http.DefaultClient.Do(mustReq(t, "PUT", base+"/v1/system/notifications", `{"status":"denied"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("put: %d", resp.StatusCode)
	}
	// The same status again publishes nothing more.
	resp, err = http.DefaultClient.Do(mustReq(t, "PUT", base+"/v1/system/notifications", `{"status":"denied"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp, err = http.Get(base + "/v1/stream?follow=0&cursor=0&topics=system.notifications_changed")
	if err != nil {
		t.Fatal(err)
	}
	changed, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if n := strings.Count(string(changed), "system.notifications_changed"); n != 1 {
		t.Fatalf("changed frames = %d, want 1: %q", n, changed)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/system/notifications", nil)
	if code != 200 || body["status"] != "denied" || body["tray"] != true {
		t.Fatalf("after a report: %d %v", code, body)
	}

	resp, err = http.Post(base+"/v1/system/notifications/request", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("request: %d", resp.StatusCode)
	}
	resp, err = http.Get(base + "/v1/stream?follow=0&cursor=0&topics=system.notifications_requested")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "system.notifications_requested") {
		t.Fatalf("topic not published: %q", raw)
	}
}

func mustReq(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestDoctorFindingsCarryKey checks that a config finding names its config
// key and that a harness finding names its harness.
func TestDoctorFindingsCarryKey(t *testing.T) {
	_, base := startTestServer(t)
	code, body, _ := doJSON(t, "PATCH", base+"/v1/config", map[string]any{"set": map[string]any{
		"api.host": "0.0.0.0", "api.tls.insecure": "true",
	}})
	if code/100 != 2 {
		t.Fatalf("config patch %d %v", code, body)
	}
	_, body, _ = doJSON(t, "GET", base+"/v1/system/doctor", nil)
	byCode := map[string]map[string]any{}
	for _, r := range body["findings"].([]any) {
		m := r.(map[string]any)
		byCode[m["code"].(string)] = m
	}
	if f := byCode["api_tls_insecure"]; f == nil || f["key"] != "api.tls.insecure" {
		t.Fatalf("api_tls_insecure finding = %v", f)
	}
	if f := byCode["harness_home_missing"]; f == nil || f["harness"] == nil || f["harness"] == "" {
		t.Fatalf("harness_home_missing finding = %v", f)
	}
}

type fakeRestarter struct{ calls chan struct{} }

func (f *fakeRestarter) RestartDaemon() error {
	f.calls <- struct{}{}
	return nil
}

// TestSystemRestartRoute uses a fake restarter, so no real service restarts.
func TestSystemRestartRoute(t *testing.T) {
	fake := &fakeRestarter{calls: make(chan struct{}, 1)}
	_, base := startTestServerOpts(t, api.Options{Restarter: fake})
	if code, body, _ := doJSON(t, "POST", base+"/v1/system/restart", nil); code != 400 || errCode(body) != "validation" {
		t.Fatalf("restart without confirm %d %v", code, body)
	}
	code, body, _ := doJSON(t, "POST", base+"/v1/system/restart?confirm=1", nil)
	if code != 202 || body["ok"] != true {
		t.Fatalf("restart %d %v", code, body)
	}
	select {
	case <-fake.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("RestartDaemon not called")
	}

	_, plain := startTestServer(t)
	if code, body, _ := doJSON(t, "POST", plain+"/v1/system/restart?confirm=1", nil); code != 501 || errCode(body) != "unavailable" {
		t.Fatalf("restart with no service %d %v", code, body)
	}
}
