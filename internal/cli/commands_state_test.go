package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stateTestServer records the last request and answers with reply.
type stateTestServer struct {
	srv    *httptest.Server
	method string
	uri    string
	body   map[string]any
	status int
	reply  string
}

func TestStateCommandRequests(t *testing.T) {
	t.Setenv("TREBI_RUN_ID", "")
	cases := []struct {
		name   string
		args   []string
		method string
		uri    string
		body   string
	}{
		{"summary", []string{"state", "--owner", "jb_3f9a1c7e04"}, "GET", "/v1/state/jb_3f9a1c7e04", ""},
		{"query", []string{"state", "--owner", "jb_3f9a1c7e04", "query", "papers", "--since", "7d", "--where", "v.score == 2", "--limit", "5"},
			"POST", "/v1/state/jb_3f9a1c7e04/papers/query", `{"limit":5,"since":"7d","where":"v.score == 2"}`},
		{"get", []string{"state", "--owner", "jb_3f9a1c7e04", "get", "papers", "doi:10/1", "a/b"},
			"POST", "/v1/state/jb_3f9a1c7e04/papers/get", `{"keys":["doi:10/1","a/b"]}`},
		{"put one", []string{"state", "--owner", "jb_3f9a1c7e04", "put", "papers", "k1", "--value", `{"n":1}`, "--at", "today"},
			"POST", "/v1/state/jb_3f9a1c7e04/papers/rows", `{"rows":[{"at":"today","key":"k1","value":{"n":1}}]}`},
		{"delete", []string{"state", "--owner", "jb_3f9a1c7e04", "delete", "papers", "k1", "--yes"},
			"POST", "/v1/state/jb_3f9a1c7e04/papers/delete?confirm=1", `{"keys":["k1"]}`},
		{"prune", []string{"state", "--owner", "jb_3f9a1c7e04", "prune", "papers", "--until", "180d"},
			"POST", "/v1/state/jb_3f9a1c7e04/papers/prune", `{"until":"180d"}`},
		{"drop", []string{"state", "--owner", "jb_3f9a1c7e04", "drop", "papers", "--yes"},
			"DELETE", "/v1/state/jb_3f9a1c7e04/papers?confirm=1", ""},
	}
	for _, c := range cases {
		s := newStateTestServer(t)
		if _, err := runCLI(t, s.client(), c.args...); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if s.method != c.method || s.uri != c.uri {
			t.Errorf("%s: %s %s, want %s %s", c.name, s.method, s.uri, c.method, c.uri)
		}
		got := ""
		if s.body != nil {
			b, _ := json.Marshal(s.body)
			got = string(b)
		}
		if got != c.body {
			t.Errorf("%s: body %s, want %s", c.name, got, c.body)
		}
	}
}

func TestStateOwnerSelfInRun(t *testing.T) {
	s := newStateTestServer(t)
	t.Setenv("TREBI_RUN_ID", "")
	if _, err := runCLI(t, s.client(), "state", "query", "papers"); err == nil || !strings.Contains(err.Error(), "--owner") {
		t.Fatalf("no owner outside a run: err %v", err)
	}
	t.Setenv("TREBI_RUN_ID", "run_1")
	if _, err := runCLI(t, s.client(), "state", "has", "papers", "k1"); err != nil {
		t.Fatal(err)
	}
	if s.uri != "/v1/state/self/papers/has" {
		t.Fatalf("uri %s", s.uri)
	}
}

func TestStatePutFileAndBatchError(t *testing.T) {
	s := newStateTestServer(t)
	t.Setenv("TREBI_RUN_ID", "run_1")
	s.status = http.StatusUnprocessableEntity
	s.reply = `{"error":{"code":"state_invalid","message":"2 of 3 rows failed","details":{"errors":[` +
		`{"line":1,"path":"key","msg":"key is empty"},{"line":3,"path":"value.topic","msg":"\"x\" is not one of [a b]"}]}}}`
	input := `{"key":"","value":{}}` + "\n\n" + `{"key":"b","value":{}}` + "\n" + `{"key":"c","value":{"topic":"x"}}` + "\n"
	_, err := runCLIWithInput(t, s.client(), input, "state", "put", "papers", "--mode", "merge", "-")
	if err == nil {
		t.Fatal("batch error passes")
	}
	want := "2 of 3 rows failed\nline 1: key: key is empty\nline 3: value.topic: \"x\" is not one of [a b]"
	if err.Error() != want {
		t.Fatalf("err %q, want %q", err.Error(), want)
	}
	rows, _ := s.body["rows"].([]any)
	if len(rows) != 3 || s.body["mode"] != "merge" {
		t.Fatalf("body %v", s.body)
	}
}

func TestParseStateRowsArray(t *testing.T) {
	t.Parallel()
	rows, err := parseStateRows([]byte(` [{"key":"a"},{"key":"b"}]`))
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows %v err %v", rows, err)
	}
	if _, err := parseStateRows([]byte("{\"key\":\"a\"}\nnot json\n")); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("err %v", err)
	}
}

func newStateTestServer(t *testing.T) *stateTestServer {
	t.Helper()
	s := &stateTestServer{status: http.StatusOK, reply: `{}`}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.method, s.uri, s.body = r.Method, r.URL.RequestURI(), nil
		_ = json.Unmarshal(raw, &s.body) // an empty body leaves nil
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		_, _ = io.WriteString(w, s.reply)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stateTestServer) client() *APIClient {
	return &APIClient{base: s.srv.URL, httpClient: s.srv.Client()}
}
