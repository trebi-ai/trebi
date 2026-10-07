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

	"github.com/trebi-ai/trebi/internal/request"
	"github.com/trebi-ai/trebi/internal/requests"
)

// requestTestServer records each request body and answers GET with the
// request that the test sets.
type requestTestServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies map[string][]byte
	tokens map[string]string
	gets   int
	reply  requests.Request
}

func newRequestTestServer(t *testing.T) *requestTestServer {
	t.Helper()
	s := &requestTestServer{bodies: map[string][]byte{}, tokens: map[string]string{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies[r.Method+" "+r.URL.Path] = body
		s.tokens[r.Method+" "+r.URL.Path] = r.Header.Get("X-Trebi-Run-Token")
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
	s.reply = requests.Request{ID: "R1", Status: requests.StatusPending, Forms: []requests.Form{{Type: requests.TypeReview}}}
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
		Forms []struct {
			Title string
			Draft struct{ Format, Text string }
		}
	}
	if err := json.Unmarshal(s.body("POST /v1/requests"), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Forms) != 1 || got.Forms[0].Title != "Send it" || got.Forms[0].Draft.Text != draft || got.Forms[0].Draft.Format != "markdown" {
		t.Fatalf("forms %+v", got.Forms)
	}
}

func TestRequestCreateOptions(t *testing.T) {
	s := newRequestTestServer(t)
	s.reply = requests.Request{ID: "R1", Status: requests.StatusPending}
	if _, err := runCLI(t, s.client(), "request", "create", "--type", "choose", "--title", "Pick", "--option", "a=Plan A", "--option", "b=Plan B", "--multi"); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Forms []struct {
			Options []requests.Option
			Multi   bool
		}
	}
	if err := json.Unmarshal(s.body("POST /v1/requests"), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Forms) != 1 || len(got.Forms[0].Options) != 2 || got.Forms[0].Options[1].Label != "Plan B" || !got.Forms[0].Multi {
		t.Fatalf("body %+v", got)
	}
	if _, err := runCLI(t, s.client(), "request", "create", "--type", "choose", "--title", "Pick", "--option", "nolabel"); err == nil {
		t.Fatal("option without a label passes")
	}
}

func TestRequestCreateFormsFile(t *testing.T) {
	s := newRequestTestServer(t)
	s.reply = requests.Request{ID: "R1", Status: requests.StatusPending}
	path := filepath.Join(t.TempDir(), "forms.json")
	forms := `[{"type":"confirm","title":"Tag it"},{"type":"choose","title":"Pick","options":[{"id":"a","label":"A"}]}]`
	if err := os.WriteFile(path, []byte(forms), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, s.client(), "request", "create", "--forms-file", path, "--title", "Ship the release"); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Title string
		Forms []struct{ Type, Title string }
	}
	if err := json.Unmarshal(s.body("POST /v1/requests"), &got); err != nil {
		t.Fatal(err)
	}
	if got.Title != "Ship the release" || len(got.Forms) != 2 || got.Forms[1].Type != "choose" {
		t.Fatalf("body %+v", got)
	}
	if _, err := runCLI(t, s.client(), "request", "create", "--forms-file", path, "--type", "confirm"); err == nil || !strings.Contains(err.Error(), "--type") {
		t.Fatalf("form flag with --forms-file: %v", err)
	}
	if _, err := runCLI(t, s.client(), "request", "create", "--title", "x"); err == nil {
		t.Fatal("create with no type passes")
	}
}

func TestRequestCreateSendsRunToken(t *testing.T) {
	s := newRequestTestServer(t)
	s.reply = requests.Request{ID: "R1", Status: requests.StatusPending}
	t.Setenv("TREBI_RUN_TOKEN", "tok")
	if _, err := runCLI(t, s.client(), "request", "create", "--type", "confirm", "--title", "Go"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	got := s.tokens["POST /v1/requests"]
	s.mu.Unlock()
	if got != "tok" {
		t.Fatalf("run token %q, want tok", got)
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
	s.reply = requests.Request{ID: "R1", Status: requests.StatusPending, Forms: []requests.Form{{Idx: 0, Type: requests.TypeConfirm}}}
	if _, err := runCLI(t, s.client(), "request", "answer", "R1", "respond", "--message", "why?"); err != nil {
		t.Fatal(err)
	}
	var got request.RequestAnswerBody
	if err := json.Unmarshal(s.body("POST /v1/requests/R1/answer"), &got); err != nil {
		t.Fatal(err)
	}
	if got.Via != "cli" || len(got.Forms) != 1 || got.Forms[0].Decision != "respond" || got.Forms[0].Message != "why?" {
		t.Fatalf("body %+v", got)
	}
}

func TestRequestAnswerManyForms(t *testing.T) {
	s := newRequestTestServer(t)
	s.reply = requests.Request{ID: "R1", Status: requests.StatusPending, Forms: []requests.Form{{Idx: 0}, {Idx: 1}}}
	if _, err := runCLI(t, s.client(), "request", "answer", "R1", "approve"); err == nil || !strings.Contains(err.Error(), "--answers-file") {
		t.Fatalf("err %v", err)
	}
	path := filepath.Join(t.TempDir(), "answers.json")
	if err := os.WriteFile(path, []byte(`[{"idx":0,"decision":"approve"},{"idx":1,"decision":"reject"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, s.client(), "request", "answer", "R1", "--answers-file", path); err != nil {
		t.Fatal(err)
	}
	var got request.RequestAnswerBody
	if err := json.Unmarshal(s.body("POST /v1/requests/R1/answer"), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Forms) != 2 || got.Forms[1].Decision != "reject" || got.Via != "cli" {
		t.Fatalf("body %+v", got)
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
