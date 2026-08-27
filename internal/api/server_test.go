package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestHealthAndAuth(t *testing.T) {
	s, base := startTestServer(t)

	// no auth
	resp, err := http.Get(base + "/v1/system/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", base+"/v1/system/health", nil)
	req.Header.Set("Authorization", "Bearer "+s.Token())
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["api"] != "v1" || body["version"] == "" {
		t.Fatalf("%v", body)
	}
}

func TestRunsLifecycle(t *testing.T) {
	s, base := startTestServer(t)
	client := &http.Client{Timeout: 5 * time.Second}
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "demo")
	wsID := addWorkspace(t, base, s.Token(), wsDir, "life-ws")

	// create run
	payload := `{"job":"demo","workspace":` + mustQuote(wsID) + `,"harness":"claude","prompt_override":"hello TREBI_RUN_COMPLETE"}`
	req, _ := http.NewRequest("POST", base+"/v1/runs", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer "+s.Token())
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	var run map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&run)
	id, _ := run["id"].(string)
	if id == "" {
		t.Fatalf("%v", run)
	}
	// With TREBI_FAKE_HARNESS, CreateAndLaunch completes synchronously.
	if st, _ := run["status"].(string); st != "running" && st != "complete" {
		t.Fatalf("status=%v want running|complete: %v", st, run)
	}

	// get
	req, _ = http.NewRequest("GET", base+"/v1/runs/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+s.Token())
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("get %d", resp.StatusCode)
	}

	// timeline
	req, _ = http.NewRequest("GET", base+"/v1/runs/"+id+"/timeline", nil)
	req.Header.Set("Authorization", "Bearer "+s.Token())
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var tl map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&tl)
	resp.Body.Close()
	segs, _ := tl["segments"].([]any)
	if len(segs) < 1 {
		t.Fatalf("timeline: %v", tl)
	}

	// kill
	req, _ = http.NewRequest("POST", base+"/v1/runs/"+id+"/kill", nil)
	req.Header.Set("Authorization", "Bearer "+s.Token())
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("kill %d", resp.StatusCode)
	}

	req, _ = http.NewRequest("GET", base+"/v1/runs/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+s.Token())
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&run)
	resp.Body.Close()
	// Kill is idempotent: fake harness may already be complete/sentinel.
	st, _ := run["status"].(string)
	if st != "exited" && st != "complete" {
		t.Fatalf("%v", run)
	}
}

func TestRulesTestAndQueueFire(t *testing.T) {
	s, base := startTestServer(t)
	client := &http.Client{Timeout: 5 * time.Second}

	body := `{"match":"event.n===1","event":{"n":1}}`
	req, _ := http.NewRequest("POST", base+"/v1/rules/test", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+s.Token())
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if res["matched"] != true {
		t.Fatalf("%v", res)
	}

	fire := `{"integration":"test","workspace":"ws","job":"j","match":"true","event":{"x":1}}`
	req, _ = http.NewRequest("POST", base+"/v1/rules/fire", bytes.NewBufferString(fire))
	req.Header.Set("Authorization", "Bearer "+s.Token())
	req.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if res["outcome"] != "queued" {
		t.Fatalf("%v", res)
	}

	req, _ = http.NewRequest("GET", base+"/v1/queue?status=pending", nil)
	req.Header.Set("Authorization", "Bearer "+s.Token())
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var q map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&q)
	resp.Body.Close()
	items, _ := q["items"].([]any)
	if len(items) < 1 {
		t.Fatalf("queue empty: %v", q)
	}
}

func TestWorkspaceAndJobs(t *testing.T) {
	s, base := startTestServer(t)
	client := &http.Client{Timeout: 5 * time.Second}

	wsDir := t.TempDir()
	jobDir := filepath.Join(wsDir, "hello")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jobMD := "---\nworking_dir: .\nharness: claude\ntriggers:\n  - cron: \"0 * * * *\"\n---\nDo work.\n"
	if err := os.WriteFile(filepath.Join(jobDir, "JOB.md"), []byte(jobMD), 0o644); err != nil {
		t.Fatal(err)
	}

	payload := `{"path":` + mustQuote(wsDir) + `,"name":"testws"}`
	req, _ := http.NewRequest("POST", base+"/v1/workspaces", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer "+s.Token())
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var ws map[string]any
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("status=%d %s", resp.StatusCode, b)
	}
	_ = json.Unmarshal(b, &ws)
	if ws["id"] == "" {
		t.Fatalf("%v", ws)
	}
	// trebi.yaml minted
	if _, err := os.Stat(filepath.Join(wsDir, "trebi.yaml")); err != nil {
		t.Fatal(err)
	}

	req, _ = http.NewRequest("GET", base+"/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+s.Token())
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var jobs map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&jobs)
	resp.Body.Close()
	list, _ := jobs["jobs"].([]any)
	if len(list) < 1 {
		t.Fatalf("jobs: %v", jobs)
	}
}

func TestShadowAllowsManualBlocksTrigger(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := api.New(st, api.Options{Token: "t", Shadow: true})
	addr, err := s.ListenAndServe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	base := "http://" + addr
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "j")
	wsID := addWorkspace(t, base, "t", wsDir, "shadow-ws")

	// manual OK
	req, _ := http.NewRequest("POST", base+"/v1/runs", bytes.NewBufferString(
		`{"job":"j","workspace":`+mustQuote(wsID)+`,"reason":"manual","harness":"claude"}`))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("manual: %d", resp.StatusCode)
	}

	// trigger blocked
	req, _ = http.NewRequest("POST", base+"/v1/runs", bytes.NewBufferString(`{"job":"j","reason":"trigger"}`))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("trigger: %d", resp.StatusCode)
	}
}

func TestInboxWiredFromAlerts(t *testing.T) {
	s, base := startTestServer(t)
	// Raise via service, then list via HTTP
	id, err := s.Alerts().Raise(t.Context(), alerts.KindWaiting, "subj", "title", "body", "hint", "r1", "ws", "job")
	if err != nil || id == "" {
		t.Fatalf("%v %s", err, id)
	}
	req, _ := http.NewRequest("GET", base+"/v1/inbox", nil)
	req.Header.Set("Authorization", "Bearer "+s.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	items, _ := body["items"].([]any)
	if len(items) < 1 {
		t.Fatalf("inbox empty: %v", body)
	}
}

func TestRunsCreateFakeHarnessCompletes(t *testing.T) {
	s, base := startTestServer(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "demo")
	wsID := addWorkspace(t, base, s.Token(), wsDir, "fake-ws")
	payload := `{"job":"demo","workspace":` + mustQuote(wsID) + `,"harness":"fake","prompt_override":"hi"}`
	req, _ := http.NewRequest("POST", base+"/v1/runs", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer "+s.Token())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var run map[string]any
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 201 {
		t.Fatalf("status=%d %s", resp.StatusCode, b)
	}
	_ = json.Unmarshal(b, &run)
	if run["status"] != "complete" || run["end_reason"] != "sentinel" {
		t.Fatalf("want complete/sentinel got %v", run)
	}
}

func TestCloudStub(t *testing.T) {
	s, base := startTestServer(t)
	req, _ := http.NewRequest("GET", base+"/v1/cloud/status", nil)
	req.Header.Set("Authorization", "Bearer "+s.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "not_linked" {
		t.Fatalf("%v", body)
	}
}

func TestStreamStaysOpenAndDeliversLateEvent(t *testing.T) {
	s, base := startTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/v1/stream", nil)
	req.Header.Set("Authorization", "Bearer "+s.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	// Append a bus event after more than two poll intervals.
	go func() {
		time.Sleep(600 * time.Millisecond)
		req2, _ := http.NewRequest("POST", base+"/v1/system/drain", nil)
		req2.Header.Set("Authorization", "Bearer "+s.Token())
		if r2, err := http.DefaultClient.Do(req2); err == nil {
			r2.Body.Close()
		}
	}()

	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "system.drain.entered") {
			return // stream stayed open and delivered the late event
		}
	}
	t.Fatalf("stream closed without late event: %v", sc.Err())
}

func TestAuthTokenRules(t *testing.T) {
	s, base := startTestServer(t)

	// Wrong bearer token.
	req, _ := http.NewRequest("GET", base+"/v1/system/health", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: status=%d", resp.StatusCode)
	}

	// Query token on a non-stream route is rejected.
	resp, err = http.Get(base + "/v1/system/health?token=" + s.Token())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("query token on health: status=%d", resp.StatusCode)
	}

	// Query token on /v1/stream is accepted.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ = http.NewRequestWithContext(ctx, "GET", base+"/v1/stream?token="+s.Token(), nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	code := resp.StatusCode
	cancel()
	resp.Body.Close()
	if code != 200 {
		t.Fatalf("query token on stream: status=%d", code)
	}
}

func TestJobRunEndpointLaunchesFakeHarness(t *testing.T) {
	s, base := startTestServer(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "demo")
	addWorkspace(t, base, s.Token(), wsDir, "jobrun-ws")

	// Unknown job → 404 through the A4 resolver, never a launch.
	code, body, _ := doJSON(t, "POST", base+"/v1/jobs/nope/run", s.Token(), nil)
	if code != 404 || errCode(body) != "not_found" {
		t.Fatalf("unknown job: want 404 not_found got %d %v", code, body)
	}

	code, run, _ := doJSON(t, "POST", base+"/v1/jobs/demo/run", s.Token(), nil)
	if code != 201 {
		t.Fatalf("status=%d %v", code, run)
	}
	if run["job"] != "demo" {
		t.Fatalf("job=%v", run["job"])
	}
	if st, _ := run["status"].(string); st != "running" && st != "complete" {
		t.Fatalf("status=%v: %v", st, run)
	}
}

func startTestServer(t *testing.T) (*api.Server, string) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Setenv("TREBI_FAKE_HARNESS", "1")
	t.Cleanup(func() { home.SetBase("") })

	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	s := api.New(st, api.Options{Token: "test-token"})
	addr, err := s.ListenAndServe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	return s, "http://" + addr
}

func mustQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
