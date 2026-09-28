package cli

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

	"github.com/trebi-ai/trebi/internal/requests"
)

func TestRequestExitCode(t *testing.T) {
	t.Parallel()
	answered := func(d requests.Decision) requests.Request {
		return requests.Request{Status: requests.StatusAnswered, Answer: &requests.Answer{Decision: d}}
	}
	cases := []struct {
		r    requests.Request
		want int
	}{
		{answered(requests.DecisionApprove), requestExitApproved},
		{answered(requests.DecisionEdit), requestExitApproved},
		{answered(requests.DecisionChoose), requestExitApproved},
		{answered(requests.DecisionSubmit), requestExitApproved},
		{answered(requests.DecisionReject), requestExitDeclined},
		{answered(requests.DecisionRespond), requestExitDeclined},
		{requests.Request{Status: requests.StatusExpired, Answer: &requests.Answer{Decision: requests.DecisionExpired}}, requestExitClosed},
		{requests.Request{Status: requests.StatusDismissed}, requestExitClosed},
		{requests.Request{Status: requests.StatusCancelled}, requestExitClosed},
		{requests.Request{Status: requests.StatusPending}, requestExitTimeout},
	}
	for _, c := range cases {
		if got := requestExitCode(c.r); got != c.want {
			t.Errorf("%s/%v: exit %d, want %d", c.r.Status, c.r.Answer, got, c.want)
		}
	}
}

// requestTestServer records each request body and answers GET with the
// request that the test sets.
type requestTestServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies map[string][]byte
	gets   int
	reply  requests.Request
}

func newRequestTestServer(t *testing.T) *requestTestServer {
	t.Helper()
	s := &requestTestServer{bodies: map[string][]byte{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies[r.Method+" "+r.URL.Path] = body
		if r.Method == http.MethodGet {
			s.gets++
		}
		reply := s.reply
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/v1/requests" {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"request": reply, "next": "end_turn", "message": "End your turn now."})
			return
		}
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *requestTestServer) client() *APIClient {
	return &APIClient{base: s.srv.URL, httpClient: s.srv.Client()}
}

func (s *requestTestServer) body(key string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[key]
}

func TestRequestCreateDraftFileKeepsBytes(t *testing.T) {
	s := newRequestTestServer(t)
	s.reply = requests.Request{ID: "R1", Status: requests.StatusPending, Delivery: requests.DeliveryResume}
	draft := "Hi Ana,\r\n\n`code` \"q\" <b> \t\nend "
	path := filepath.Join(t.TempDir(), "reply.md")
	if err := os.WriteFile(path, []byte(draft), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, s.client(), "request", "create", "--type", "review", "--title", "Send it", "--draft-file", path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "R1") || !strings.Contains(out, "End your turn now.") {
		t.Fatalf("output %q", out)
	}
	var got struct {
		Draft struct{ Format, Text string }
	}
	if err := json.Unmarshal(s.body("POST /v1/requests"), &got); err != nil {
		t.Fatal(err)
	}
	if got.Draft.Text != draft || got.Draft.Format != "markdown" {
		t.Fatalf("draft %q format %s", got.Draft.Text, got.Draft.Format)
	}
}

func TestRequestCreateOptions(t *testing.T) {
	s := newRequestTestServer(t)
	s.reply = requests.Request{ID: "R1", Status: requests.StatusPending}
	if _, err := runCLI(t, s.client(), "request", "create", "--type", "choose", "--title", "Pick", "--option", "a=Plan A", "--option", "b=Plan B", "--multi"); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Options []requests.Option
		Multi   bool
	}
	if err := json.Unmarshal(s.body("POST /v1/requests"), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Options) != 2 || got.Options[1].Label != "Plan B" || !got.Multi {
		t.Fatalf("body %+v", got)
	}
	if _, err := runCLI(t, s.client(), "request", "create", "--type", "choose", "--title", "Pick", "--option", "nolabel"); err == nil {
		t.Fatal("option without a label passes")
	}
}

func TestWaitForRequestCodes(t *testing.T) {
	s := newRequestTestServer(t)
	for _, c := range []struct {
		r    requests.Request
		want int
	}{
		{requests.Request{ID: "R1", Status: requests.StatusAnswered, Answer: &requests.Answer{Decision: requests.DecisionApprove}}, requestExitApproved},
		{requests.Request{ID: "R1", Status: requests.StatusAnswered, Answer: &requests.Answer{Decision: requests.DecisionReject}}, requestExitDeclined},
		{requests.Request{ID: "R1", Status: requests.StatusExpired}, requestExitClosed},
		{requests.Request{ID: "R1", Status: requests.StatusPending}, requestExitTimeout},
	} {
		s.mu.Lock()
		s.reply = c.r
		s.mu.Unlock()
		r, err := waitForRequest(s.client(), "R1", 10*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if got := requestExitCode(r); got != c.want {
			t.Errorf("%s: exit %d, want %d", c.r.Status, got, c.want)
		}
	}
}

func TestRequestAnswerRefusedForRun(t *testing.T) {
	s := newRequestTestServer(t)
	t.Setenv("TREBI_RUN_TOKEN", "tok")
	for _, args := range [][]string{{"request", "answer", "R1", "approve"}, {"request", "dismiss", "R1"}} {
		if _, err := runCLI(t, s.client(), args...); err == nil || !strings.Contains(err.Error(), "a person does that") {
			t.Fatalf("%v: err %v", args, err)
		}
	}
	if s.body("POST /v1/requests/R1/answer") != nil {
		t.Fatal("the refused answer reached the daemon")
	}
}

func TestRequestAnswerSendsCLI(t *testing.T) {
	s := newRequestTestServer(t)
	s.reply = requests.Request{ID: "R1", Status: requests.StatusAnswered, Answer: &requests.Answer{Decision: requests.DecisionRespond}}
	if _, err := runCLI(t, s.client(), "request", "answer", "R1", "respond", "--message", "why?"); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(s.body("POST /v1/requests/R1/answer"), &got); err != nil {
		t.Fatal(err)
	}
	if got["decision"] != "respond" || got["message"] != "why?" || got["via"] != "cli" {
		t.Fatalf("body %v", got)
	}
}

func TestRequestGetAlias(t *testing.T) {
	s := newRequestTestServer(t)
	s.reply = requests.Request{ID: "R1", Title: "Send it", Status: requests.StatusPending}
	out, err := runCLI(t, s.client(), "request", "get", "R1", "--json")
	if err != nil || !strings.Contains(out, "Send it") {
		t.Fatalf("out %q err %v", out, err)
	}
}
