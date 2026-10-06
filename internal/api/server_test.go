package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/dispatch"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

func TestHealth(t *testing.T) {
	_, base := startTestServer(t)

	resp, err := http.Get(base + "/v1/system/health")
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

func TestSystemOverview(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "demo")
	addWorkspace(t, base, wsDir, "overview-ws")

	status, body, _ := doJSON(t, "GET", base+"/v1/system/overview", nil)
	if status != 200 {
		t.Fatalf("status=%d body=%v", status, body)
	}
	if body["version"] == "" || body["workspaces"] != float64(1) {
		t.Fatalf("%v", body)
	}
	for _, key := range []string{"doctor", "harnesses", "runs", "inbox", "drain_state"} {
		if _, ok := body[key]; !ok {
			t.Errorf("overview omits %q: %v", key, body)
		}
	}
	runsBlock, _ := body["runs"].(map[string]any)
	if runsBlock["running"] != float64(0) || runsBlock["waiting"] != float64(0) {
		t.Fatalf("runs %v", runsBlock)
	}
}

func TestRunsLifecycle(t *testing.T) {
	_, base := startTestServer(t)
	client := &http.Client{Timeout: 5 * time.Second}
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "demo")
	wsID := addWorkspace(t, base, wsDir, "life-ws")

	// create run
	payload := `{"workspace":` + mustQuote(wsID) + `,"harness":"claude","prompt_override":"hello TREBI_RUN_COMPLETE"}`
	req, _ := http.NewRequest("POST", base+"/v1/jobs/demo/run", bytes.NewBufferString(payload))
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
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("kill %d", resp.StatusCode)
	}

	req, _ = http.NewRequest("GET", base+"/v1/runs/"+id, nil)
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

// TestRunMarkEndpoint covers POST /v1/runs/{id}/mark: success maps to
// complete, the machine status stays, and clear drops the override.
func TestRunMarkEndpoint(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "markable")
	wsID := addWorkspace(t, base, wsDir, "mark-ws")

	code, run, _ := doJSON(t, "POST", base+"/v1/jobs/markable/run", map[string]any{
		"workspace": wsID, "harness": "claude", "prompt_override": "hello TREBI_RUN_COMPLETE",
	})
	if code != 201 {
		t.Fatalf("launch=%d %v", code, run)
	}
	id, _ := run["id"].(string)
	if id == "" {
		t.Fatalf("no run id: %v", run)
	}
	// Kill guarantees a terminal row for the mark.
	if code, body, _ := doJSON(t, "POST", base+"/v1/runs/"+id+"/kill", nil); code != 200 {
		t.Fatalf("kill=%d %v", code, body)
	}
	code, machine, _ := doJSON(t, "GET", base+"/v1/runs/"+id, nil)
	if code != 200 {
		t.Fatalf("get=%d %v", code, machine)
	}
	if machine["outcome"] != nil {
		t.Fatalf("fresh run has outcome: %v", machine)
	}

	if code, _, _ := doJSON(t, "POST", base+"/v1/runs/"+id+"/mark", map[string]any{"status": "bogus"}); code != 400 {
		t.Fatalf("bogus status=%d want 400", code)
	}

	code, marked, _ := doJSON(t, "POST", base+"/v1/runs/"+id+"/mark", map[string]any{
		"status": "success", "note": "digest was sent",
	})
	if code != 200 {
		t.Fatalf("mark=%d %v", code, marked)
	}
	if marked["status"] != machine["status"] {
		t.Fatalf("mark changed machine status: %v -> %v", machine["status"], marked["status"])
	}
	outcome, _ := marked["outcome"].(map[string]any)
	if outcome["status"] != "complete" || outcome["note"] != "digest was sent" {
		t.Fatalf("outcome=%v", outcome)
	}

	code, cleared, _ := doJSON(t, "POST", base+"/v1/runs/"+id+"/mark", map[string]any{"clear": true})
	if code != 200 {
		t.Fatalf("clear=%d %v", code, cleared)
	}
	if cleared["outcome"] != nil {
		t.Fatalf("outcome after clear: %v", cleared["outcome"])
	}
}

func TestRulesTestAndQueueFire(t *testing.T) {
	_, base := startTestServer(t)
	client := &http.Client{Timeout: 5 * time.Second}

	body := `{"match":"event.n===1","event":{"n":1}}`
	req, _ := http.NewRequest("POST", base+"/v1/rules/test", bytes.NewBufferString(body))
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

	fire := `{"connector":"test","workspace":"` + fireWorkspace(t, base) + `","job":"j","match":"true","event":{"x":1}}`
	req, _ = http.NewRequest("POST", base+"/v1/rules/fire", bytes.NewBufferString(fire))
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
	_, base := startTestServer(t)
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
	s := api.New(st, api.Options{Shadow: true})
	addr, err := s.ListenAndServe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	base := "http://" + addr
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "j")
	wsID := addWorkspace(t, base, wsDir, "shadow-ws")

	// manual OK
	req, _ := http.NewRequest("POST", base+"/v1/jobs/j/run", bytes.NewBufferString(
		`{"workspace":`+mustQuote(wsID)+`,"reason":"manual","harness":"claude"}`))
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
	req, _ = http.NewRequest("POST", base+"/v1/jobs/j/run", bytes.NewBufferString(`{"reason":"trigger"}`))
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
	id, err := s.Alerts().Raise(t.Context(), alerts.RaiseOpts{
		Kind: alerts.KindWaiting, Subject: "subj", Title: "title", Body: "body", Hint: "hint",
		RunID: "r1", WorkspaceID: "ws", JobName: "job",
	})
	if err != nil || id == "" {
		t.Fatalf("%v %s", err, id)
	}
	req, _ := http.NewRequest("GET", base+"/v1/inbox", nil)
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
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "demo")
	wsID := addWorkspace(t, base, wsDir, "fake-ws")
	payload := `{"workspace":` + mustQuote(wsID) + `,"harness":"fake","prompt_override":"hi"}`
	req, _ := http.NewRequest("POST", base+"/v1/jobs/demo/run", bytes.NewBufferString(payload))
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
	_, base := startTestServer(t)
	req, _ := http.NewRequest("GET", base+"/v1/cloud/status", nil)
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
	_, base := startTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/v1/stream", nil)
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

func TestStreamCursorLatestSkipsHistory(t *testing.T) {
	_, base := startTestServer(t)
	req, _ := http.NewRequest("POST", base+"/v1/system/drain", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	dump := func(cursor string) string {
		resp, err := http.Get(base + "/v1/stream?follow=0&cursor=" + cursor)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if got := dump("0"); !strings.Contains(got, "system.drain.entered") {
		t.Fatalf("cursor=0 lost history: %q", got)
	}
	if got := dump("latest"); strings.Contains(got, "data:") {
		t.Fatalf("cursor=latest replayed history: %q", got)
	}
}

// TestOpenAPI pins the local-API contract: the daemon answers every route
// without a credential. A stray Authorization header or ?token= is ignored, so
// a client that still sends one is not refused. The bind address and the
// network around it are the whole protection.
func TestOpenAPI(t *testing.T) {
	s, base := startTestServer(t)

	// No credential at all.
	code, body, _ := doJSON(t, "GET", base+"/v1/system/health", nil)
	if code != 200 {
		t.Fatalf("health without a token: %d %v", code, body)
	}

	// A wrong bearer token is ignored, not refused.
	req, _ := http.NewRequest("GET", base+"/v1/system/health", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("health with a stray bearer: status=%d", resp.StatusCode)
	}

	// A stray query token is ignored on a plain route and on the stream.
	resp, err = http.Get(base + "/v1/system/health?token=stale")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("health with a query token: status=%d", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ = http.NewRequestWithContext(ctx, "GET", base+"/v1/stream?token=stale", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	code = resp.StatusCode
	cancel()
	resp.Body.Close()
	if code != 200 {
		t.Fatalf("stream: status=%d", code)
	}
	s.Shutdown(context.Background())
}

func TestJobRunEndpointLaunchesFakeHarness(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "demo")
	addWorkspace(t, base, wsDir, "jobrun-ws")

	// Unknown job → 404 through the A4 resolver, never a launch.
	code, body, _ := doJSON(t, "POST", base+"/v1/jobs/nope/run", nil)
	if code != 404 || errCode(body) != "not_found" {
		t.Fatalf("unknown job: want 404 not_found got %d %v", code, body)
	}

	code, run, _ := doJSON(t, "POST", base+"/v1/jobs/demo/run", nil)
	if code != 201 {
		t.Fatalf("status=%d %v", code, run)
	}
	if run["job_name"] != "demo" {
		t.Fatalf("job_name=%v", run["job_name"])
	}
	if st, _ := run["status"].(string); st != "running" && st != "complete" {
		t.Fatalf("status=%v: %v", st, run)
	}
}

func startTestServer(t *testing.T) (*api.Server, string) {
	t.Helper()
	return startTestServerOpts(t, api.Options{})
}

// startTestServerOpts starts a server with caller-chosen options.
func startTestServerOpts(t *testing.T, opts api.Options) (*api.Server, string) {
	t.Helper()
	return startTestServerSeeded(t, opts, nil)
}

func mustQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestIdempotentLaunch covers A6.8: a repeated key returns the first run.
func TestIdempotentLaunch(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "idem")
	wsID := addWorkspace(t, base, wsDir, "idem-ws")

	launch := map[string]any{
		"workspace": wsID, "harness": "fake", "prompt_override": "hi",
		"idempotency_key": "k1",
	}
	code, first, _ := doJSON(t, "POST", base+"/v1/jobs/idem/run", launch)
	if code != 201 {
		t.Fatalf("first launch: %d %v", code, first)
	}
	code, second, _ := doJSON(t, "POST", base+"/v1/jobs/idem/run", launch)
	if code != 201 {
		t.Fatalf("repeat launch: %d %v", code, second)
	}
	if first["id"] != second["id"] {
		t.Fatalf("repeat launched a second run: %v vs %v", first["id"], second["id"])
	}

	code, third, _ := doJSON(t, "POST", base+"/v1/jobs/idem/run", map[string]any{
		"workspace": wsID, "harness": "fake", "prompt_override": "hi",
	})
	if code != 201 || third["id"] == first["id"] {
		t.Fatalf("keyless launch must start a new run: %d %v", code, third)
	}

	// The Idempotency-Key header is the same contract as the body field.
	req, _ := http.NewRequest("POST", base+"/v1/jobs/idem/run",
		strings.NewReader(`{"workspace":"`+wsID+`","harness":"fake","prompt_override":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "k1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var byHeader map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&byHeader)
	if byHeader["id"] != first["id"] {
		t.Fatalf("header key must reuse the first run: %v", byHeader["id"])
	}
}

// TestListFilters covers A6.5 on runs, jobs, and workspaces.
func TestListFilters(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	writeJobFile(t, wsDir, "alpha")
	writeJobFile(t, wsDir, "beta")
	wsID := addWorkspace(t, base, wsDir, "filter-ws")

	code, run, _ := doJSON(t, "POST", base+"/v1/jobs/alpha/run", map[string]any{
		"workspace": wsID, "harness": "fake", "prompt_override": "deploy the thing",
	})
	if code != 201 {
		t.Fatalf("launch: %d %v", code, run)
	}

	for _, tc := range []struct {
		query string
		want  int
	}{
		{"?job=alpha", 1},
		{"?job=beta", 0},
		{"?q=deploy", 1},
		{"?q=nothing-matches", 0},
		{"?since=1h", 1},
		{"?since=" + url.QueryEscape(time.Now().UTC().Add(time.Hour).Format(time.RFC3339)), 0},
	} {
		code, body, _ := doJSON(t, "GET", base+"/v1/runs"+tc.query, nil)
		if code != 200 {
			t.Fatalf("%s: %d %v", tc.query, code, body)
		}
		list, _ := body["runs"].([]any)
		if len(list) != tc.want {
			t.Fatalf("%s: got %d runs, want %d", tc.query, len(list), tc.want)
		}
	}

	if code, body, _ := doJSON(t, "GET", base+"/v1/runs?since=bogus", nil); code != 400 {
		t.Fatalf("bad since must be 400: %d %v", code, body)
	}

	code, body, _ := doJSON(t, "GET", base+"/v1/jobs?q=alph", nil)
	jobs, _ := body["jobs"].([]any)
	if code != 200 || len(jobs) != 1 {
		t.Fatalf("job q filter: %d %v", code, body)
	}
	row, _ := jobs[0].(map[string]any)
	if row["last_run_at"] == "" || row["last_status"] != "complete" {
		t.Fatalf("job row enrichment: %v", row)
	}
	if _, ok := row["mtd_usd"]; !ok {
		t.Fatalf("job row must carry mtd_usd: %v", row)
	}

	if code, body, _ := doJSON(t, "GET", base+"/v1/jobs?active=maybe", nil); code != 400 {
		t.Fatalf("bad active must be 400: %d %v", code, body)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces", nil)
	list, _ := body["workspaces"].([]any)
	if code != 200 || len(list) != 1 {
		t.Fatalf("workspaces: %d %v", code, body)
	}
	ws, _ := list[0].(map[string]any)
	if ws["job_count"] != float64(2) {
		t.Fatalf("job_count: %v", ws)
	}
}

// TestJobValidateText covers A6.7: check a definition without writing a job.
func TestJobValidateText(t *testing.T) {
	_, base := startTestServer(t)

	code, body, _ := doJSON(t, "POST", base+"/v1/jobs/validate", map[string]any{
		"prompt": "Run the sync.", "cron": "0 3 * * *", "harness": "codex",
	})
	if code != 200 || body["ok"] != true {
		t.Fatalf("structured spec: %d %v", code, body)
	}
	md, _ := body["markdown"].(string)
	if !strings.Contains(md, "harness: codex") || !strings.Contains(md, "cron: 0 3 * * *") {
		t.Fatalf("composed front matter: %q", md)
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/jobs/validate", map[string]any{
		"content": "---\nharness: [broken\n---\n\nhi\n",
	})
	if code != 200 || body["ok"] != false || body["error"] == "" {
		t.Fatalf("bad yaml must report ok=false: %d %v", code, body)
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/jobs/validate", map[string]any{
		"prompt": "Read it.", "on": map[string]any{"event": "whatsapp", "rooms": []string{"r1"}, "allowlist": []string{"u1", "u2"}},
	})
	md, _ = body["markdown"].(string)
	if code != 200 || body["ok"] != true || !strings.Contains(md, "rooms:\n") || !strings.Contains(md, "- r1") || !strings.Contains(md, "- u2") {
		t.Fatalf("event scope: %d %v", code, body)
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/jobs/validate", map[string]any{
		"content": "---\nharness: fake\nallowlist: [a]\ntriggers:\n  - event: email\n---\n\nhi\n",
	})
	if code != 200 || body["ok"] != false || body["error"] != `job key "allowlist" is removed; set "allowlist" on the event trigger` {
		t.Fatalf("job-level allowlist: %d %v", code, body)
	}

	agentMD := "---\ndescription: Test agent.\nharness: fake\nemail:\n  accept: [admins]\nkit:\n  connectors: [email]\n---\n\nhi\n"
	code, body, _ = doJSON(t, "POST", base+"/v1/jobs/validate", map[string]any{"content": agentMD})
	if code != 200 || body["ok"] != false {
		t.Fatalf("operator keys in a job must fail: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "POST", base+"/v1/jobs/validate", map[string]any{"content": agentMD, "kind": "agent"})
	if code != 200 || body["ok"] != true {
		t.Fatalf("operator keys in an agent must pass: %d %v", code, body)
	}
	if code, body, _ = doJSON(t, "POST", base+"/v1/jobs/validate", map[string]any{"content": agentMD, "kind": "bogus"}); code != 400 {
		t.Fatalf("bad kind must be 400: %d %v", code, body)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/jobs", nil)
	jobs, _ := body["jobs"].([]any)
	if code != 200 || len(jobs) != 0 {
		t.Fatalf("validate must write nothing: %d %v", code, body)
	}
}

// TestFileTreeAndBase64 covers A6.9.
func TestFileTreeAndBase64(t *testing.T) {
	_, base := startTestServer(t)
	wsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wsDir, "reports", "q1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "reports", "q1", "a.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, wsDir, "tree-ws")

	code, body, _ := doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/files", nil)
	if code != 200 {
		t.Fatalf("list: %d %v", code, body)
	}
	if dirRows(body) != 0 {
		t.Fatalf("plain list must carry no folders: %v", body)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/files?recursive=1", nil)
	if code != 200 || dirRows(body) == 0 {
		t.Fatalf("recursive list must carry folders: %d %v", code, body)
	}

	png := []byte{0x89, 'P', 'N', 'G', 0x00, 0x01}
	req, _ := http.NewRequest("PUT", base+"/v1/workspaces/"+wsID+"/files/img.png?encoding=base64",
		strings.NewReader(base64.StdEncoding.EncodeToString(png)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("base64 put: %d", resp.StatusCode)
	}
	got, err := os.ReadFile(filepath.Join(wsDir, "img.png"))
	if err != nil || !bytes.Equal(got, png) {
		t.Fatalf("stored bytes: %v %v", got, err)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/files/img.png?encoding=base64", nil)
	if code != 200 || body["content"] != base64.StdEncoding.EncodeToString(png) {
		t.Fatalf("base64 get: %d %v", code, body)
	}
}

func TestSkillFromFolderSource(t *testing.T) {
	_, base := startTestServer(t)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("# deploy\n\nOne.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, body, _ := doJSON(t, "POST", base+"/v1/skills",
		map[string]any{"name": "deploy", "source": map[string]any{"kind": "folder", "path": src}})
	if code != 201 {
		t.Fatalf("create: %d %v", code, body)
	}
	got, _ := body["source"].(map[string]any)
	if got["kind"] != "folder" || got["path"] != src {
		t.Fatalf("source not kept: %v", body)
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/skills/deploy/refresh", nil)
	if code != 200 || body["updated"] != false {
		t.Fatalf("unchanged refresh: %d %v", code, body)
	}

	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("# deploy\n\nTwo.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body, _ = doJSON(t, "POST", base+"/v1/skills/deploy/refresh", nil)
	if code != 200 || body["updated"] != true {
		t.Fatalf("changed refresh: %d %v", code, body)
	}
	sk, _ := body["skill"].(map[string]any)
	if !strings.Contains(sk["skill_md"].(string), "Two.") {
		t.Fatalf("body not pulled: %v", sk)
	}
}

func TestSkillWithoutSourceRefresh(t *testing.T) {
	_, base := startTestServer(t)
	if code, body, _ := doJSON(t, "POST", base+"/v1/skills",
		map[string]any{"name": "local-one", "skill_md": "# local\n"}); code != 201 {
		t.Fatalf("create: %d %v", code, body)
	}
	code, body, _ := doJSON(t, "POST", base+"/v1/skills/local-one/refresh", nil)
	if code != 200 || body["updated"] != false || body["note"] == nil {
		t.Fatalf("refresh: %d %v", code, body)
	}
}

func TestSkillInheritedListAndGuard(t *testing.T) {
	s, base := startTestServer(t)
	user := t.TempDir()
	dir := filepath.Join(user, ".claude", "skills", "sling")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# Sling\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Skills().WithUserHome(user)

	code, body, _ := doJSON(t, "GET", base+"/v1/skills", nil)
	if code != 200 {
		t.Fatalf("list: %d %v", code, body)
	}
	rows, _ := body["skills"].([]any)
	if len(rows) != 1 {
		t.Fatalf("list=%v", body)
	}
	row, _ := rows[0].(map[string]any)
	if row["name"] != "sling" || row["provenance"] != "inherited" {
		t.Fatalf("row %+v", row)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/skills/sling", nil)
	if code != 200 || body["provenance"] != "inherited" {
		t.Fatalf("get: %d %v", code, body)
	}

	code, body, _ = doJSON(t, "PUT", base+"/v1/skills/sling", map[string]any{"skill_md": "# no\n"})
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("put: %d %v", code, body)
	}
	code, body, _ = doJSON(t, "DELETE", base+"/v1/skills/sling?confirm=1", nil)
	if code != 409 || errCode(body) != "conflict" {
		t.Fatalf("delete: %d %v", code, body)
	}
}

func TestBulkVerbs(t *testing.T) {
	_, base := startTestServer(t)

	code, body, _ := doJSON(t, "POST", base+"/v1/runs/kill", map[string]any{"ids": []string{}})
	if code != 400 {
		t.Fatalf("empty ids must be a validation error: %d %v", code, body)
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/runs/kill",
		map[string]any{"ids": []string{"01MISSINGA", "01MISSINGB"}})
	if code != 200 {
		t.Fatalf("bulk kill: %d %v", code, body)
	}
	failed, _ := body["failed"].(map[string]any)
	if len(failed) != 2 {
		t.Fatalf("both ids must fail on their own: %v", body)
	}

	code, body, _ = doJSON(t, "POST", base+"/v1/inbox/resolve-all", nil)
	if code != 200 || body["resolved"] == nil {
		t.Fatalf("resolve-all: %d %v", code, body)
	}
}

func TestWriteAPIJSON_UnspecifiedHost(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := api.New(st, api.Options{})
	if err := s.WriteAPIJSON("0.0.0.0:64119"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "api.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["url"] != "http://127.0.0.1:64119" {
		t.Fatalf("url=%v", doc["url"])
	}
	if int(doc["port"].(float64)) != 64119 {
		t.Fatalf("port=%v", doc["port"])
	}
	// A dual-stack wildcard reports [::] on macOS; the URL stays IPv4.
	if err := s.WriteAPIJSON("[::]:64120"); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(dir, "api.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["url"] != "http://127.0.0.1:64120" {
		t.Fatalf("wildcard url=%v", doc["url"])
	}
}

// dirRows counts folder entries in a file list response.
func dirRows(body map[string]any) int {
	n := 0
	rows, _ := body["files"].([]any)
	for _, row := range rows {
		if m, ok := row.(map[string]any); ok && m["dir"] == true {
			n++
		}
	}
	return n
}

// TestCreateRunQueuesBehindLiveRun proves a manual launch waits in the queue
// while a run of the job is live, and a full queue answers 429.
func TestCreateRunQueuesBehindLiveRun(t *testing.T) {
	_, base, dir := startAPI(t)
	wsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wsDir, "busy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "busy", "JOB.md"), []byte("---\nharness: fake\n---\n\nGo.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, wsDir, "busy-ws")
	st := mustOpenStore(t, filepath.Join(dir, "trebi.db"))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := st.DB().Exec(`INSERT INTO runs (id,workspace_id,job_id,kind,status,trace_id,started_at,updated_at)
		VALUES ('live',?,?,'job','running','trace',?,?)`, wsID, indexedJobID(t, st, wsID, "busy"), now, now); err != nil {
		t.Fatal(err)
	}
	launch := map[string]any{"workspace": wsID, "job": "busy", "kind": "job", "reason": "manual", "harness": "fake"}
	for i := 0; i < dispatch.LaneCap; i++ {
		code, body, _ := doJSON(t, "POST", base+"/v1/runs", launch)
		if code != http.StatusAccepted || body["queued"] != true || body["queue_id"] == "" {
			t.Fatalf("launch %d: %d %v, want 202 queued", i, code, body)
		}
	}
	code, body, _ := doJSON(t, "POST", base+"/v1/runs", launch)
	e, _ := body["error"].(map[string]any)
	if code != http.StatusTooManyRequests || e == nil || e["code"] != "queue_full" {
		t.Fatalf("launch at cap: %d %v, want 429 queue_full", code, body)
	}
}

// startTestServerSeeded runs seed on the store before the server reads it.
func startTestServerSeeded(t *testing.T, opts api.Options, seed func(*store.Store)) (*api.Server, string) {
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
	if seed != nil {
		seed(st)
	}

	if opts.LookupLatest == nil {
		opts.LookupLatest = func() (string, string, error) {
			return api.Version, "", nil
		}
	}
	s := api.New(st, opts)
	addr, err := s.ListenAndServe("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	return s, "http://" + addr
}
