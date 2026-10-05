package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urfave/cli/v3"
)

const (
	testRunA = "01M45YXHC82B1BZ20D138P8KFE"
	testRunB = "01M45YXHC82B1BZ20D138P8KFF"
)

// runTestServer is a fake daemon for the wait and watch commands. Each GET of
// a run pops the next status of that run; the last status repeats.
type runTestServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	statuses map[string][]string
	outcome  string
	runtime  string
	polls    int
	waitMS   []string
	jobRuns  []string // newest first, as GET /v1/runs returns them
	nextRun  string   // starts after the first job run list
	live     string
	launched int
	delay    time.Duration // per run poll, like a server long-poll
}

func TestRunsWaitPollsUntilEnd(t *testing.T) {
	s := newRunTestServer(t)
	s.statuses[testRunA] = []string{"running", "running", "complete"}
	code := captureExit(t)
	out, err := runCLI(t, s.client(), "runs", "wait", testRunA)
	if err != nil {
		t.Fatal(err)
	}
	if *code != -1 || !strings.Contains(out, `"status": "complete"`) {
		t.Fatalf("exit %d out %s", *code, out)
	}
	if s.polls != 3 || s.waitMS[0] != "120000" {
		t.Fatalf("polls %d wait_ms %v", s.polls, s.waitMS)
	}
}

func TestRunsWaitOutlastsClientTimeout(t *testing.T) {
	s := newRunTestServer(t)
	s.statuses[testRunA] = []string{"complete"}
	s.delay = 300 * time.Millisecond
	client := s.client()
	client.httpClient.Timeout = 100 * time.Millisecond
	if _, err := runCLI(t, client, "runs", "wait", testRunA); err != nil {
		t.Fatalf("long-poll cut by the client timeout: %v", err)
	}
}

func TestRunsWaitExitCodes(t *testing.T) {
	cases := []struct {
		status, outcome, timeout string
		want                     int
	}{
		{status: "complete", want: -1},
		{status: "skipped", want: -1},
		{status: "failed", want: runExitFailed},
		{status: "timed_out", want: runExitFailed},
		{status: "parked", want: runExitParked},
		{status: "failed", outcome: "complete", want: -1},
		{status: "running", timeout: "1ms", want: runExitRunning},
	}
	for _, tc := range cases {
		s := newRunTestServer(t)
		s.statuses[testRunA] = []string{tc.status}
		s.outcome = tc.outcome
		code := captureExit(t)
		args := []string{"runs", "wait", testRunA}
		if tc.timeout != "" {
			args = append(args, "--timeout", tc.timeout)
		}
		_, _ = runCLI(t, s.client(), args...)
		if *code != tc.want {
			t.Errorf("%s/%s: exit %d, want %d", tc.status, tc.outcome, *code, tc.want)
		}
	}
}

func TestJobRunWaitPrintsResult(t *testing.T) {
	s := newRunTestServer(t)
	s.statuses[testRunA] = []string{"running", "failed"}
	code := captureExit(t)
	out, _ := runCLI(t, s.client(), "job", "run", "report", "--wait")
	if *code != runExitFailed || !strings.Contains(out, "run "+testRunA+" status=failed") {
		t.Fatalf("exit %d out %s", *code, out)
	}
}

func TestJobRunFollowPrintsEvents(t *testing.T) {
	s := newRunTestServer(t)
	s.statuses[testRunA] = []string{"running", "complete"}
	code := captureExit(t)
	out, err := runCLI(t, s.client(), "job", "run", "report", "-f")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[assistant] checking memory", "[tool] Bash done", "status=complete"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Count(out, "— running") != 1 || *code != -1 {
		t.Fatalf("exit %d; status must print once on change:\n%s", *code, out)
	}
}

func TestRunsWatchJobPicksLiveRun(t *testing.T) {
	s := newRunTestServer(t)
	s.jobRuns = []string{testRunB}
	s.live = testRunA
	s.statuses[testRunA] = []string{"running", "parked"}
	code := captureExit(t)
	out, _ := runCLI(t, s.client(), "runs", "watch", "report")
	if !strings.Contains(out, "watching run "+testRunA) || *code != runExitParked {
		t.Fatalf("exit %d out %s", *code, out)
	}
}

func TestRunsWatchNextWaitsForNewRun(t *testing.T) {
	s := newRunTestServer(t)
	s.jobRuns = []string{testRunB}
	s.nextRun = testRunA
	s.statuses[testRunA] = []string{"complete"}
	out, err := runCLI(t, s.client(), "runs", "watch", "report", "--next", "--json")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var run map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &run); err != nil || run["id"] != testRunA {
		t.Fatalf("last line %q: %v", lines[len(lines)-1], err)
	}
}

func TestRunsWatchNoRuns(t *testing.T) {
	s := newRunTestServer(t)
	if _, err := runCLI(t, s.client(), "runs", "watch", "report"); err == nil || !strings.Contains(err.Error(), "--next") {
		t.Fatalf("err %v", err)
	}
}

func newRunTestServer(t *testing.T) *runTestServer {
	t.Helper()
	s := &runTestServer{statuses: map[string][]string{}, runtime: "cli"}
	upgrader := websocket.Upgrader{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/run"):
			s.launched++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": testRunA, "status": "running"})
		case r.URL.Path == "/v1/runs":
			list := []map[string]any{}
			if r.URL.Query().Get("status") == "running" {
				if s.live != "" {
					list = append(list, map[string]any{"id": s.live})
				}
			} else if len(s.jobRuns) > 0 {
				list = append(list, map[string]any{"id": s.jobRuns[0]})
			}
			if s.nextRun != "" {
				s.jobRuns = append([]string{s.nextRun}, s.jobRuns...)
				s.nextRun = ""
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"runs": list})
		case strings.HasSuffix(r.URL.Path, "/attach"):
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			for _, f := range []map[string]any{
				{"type": "event", "data": map[string]any{"kind": "assistant", "text": "checking memory"}},
				{"type": "status", "data": map[string]any{"state": "running"}},
				{"type": "status", "data": map[string]any{"state": "running"}},
				{"type": "event", "data": map[string]any{"kind": "tool", "tool": "Bash", "status": "done"}},
				{"type": "ended"},
			} {
				_ = conn.WriteJSON(f)
			}
		case strings.HasPrefix(r.URL.Path, "/v1/runs/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/runs/")
			time.Sleep(s.delay)
			s.polls++
			s.waitMS = append(s.waitMS, r.URL.Query().Get("wait_ms"))
			seq := s.statuses[id]
			status := "running"
			if len(seq) > 0 {
				status = seq[0]
				if len(seq) > 1 {
					s.statuses[id] = seq[1:]
				}
			}
			run := map[string]any{"id": id, "job": "report", "status": status, "runtime": s.runtime}
			if s.outcome != "" {
				run["outcome"] = map[string]any{"status": s.outcome}
			}
			_ = json.NewEncoder(w).Encode(run)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *runTestServer) client() *APIClient {
	return &APIClient{base: s.srv.URL, httpClient: s.srv.Client()}
}

// captureExit records the code of a cli.Exit instead of ending the test.
func captureExit(t *testing.T) *int {
	t.Helper()
	code := -1
	prev := cli.OsExiter
	cli.OsExiter = func(c int) { code = c }
	t.Cleanup(func() { cli.OsExiter = prev })
	return &code
}
