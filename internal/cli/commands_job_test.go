package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

func TestIndexSchedules(t *testing.T) {
	t.Parallel()
	rows := []map[string]any{
		{
			"workspace": "ws1", "job": "alpha", "spec": "0 9 * * *",
			"timezone": "UTC", "next_due_at": "2026-08-29T09:00:00Z", "last_fired_at": "2026-08-28T09:00:00Z",
		},
		{
			"workspace": "ws1", "job": "alpha", "spec": "@every 1h",
			"timezone": "UTC", "next_due_at": "2026-08-28T11:00:00Z", "last_fired_at": "2026-08-28T10:00:00Z",
		},
		{
			"workspace": "ws2", "job": "beta", "spec": "0 * * * *",
			"timezone": "America/Sao_Paulo", "next_due_at": "", "last_fired_at": "",
		},
	}
	got := indexSchedules(rows)
	alpha := got["ws1/alpha"]
	if alpha.Spec != "0 9 * * *, @every 1h" {
		t.Fatalf("spec=%q", alpha.Spec)
	}
	if alpha.Timezone != "UTC" {
		t.Fatalf("tz=%q", alpha.Timezone)
	}
	if alpha.Next != "2026-08-28T11:00:00Z" {
		t.Fatalf("next=%q (want earliest)", alpha.Next)
	}
	if alpha.Last != "2026-08-28T10:00:00Z" {
		t.Fatalf("last=%q (want latest)", alpha.Last)
	}
	beta := got["ws2/beta"]
	if beta.Spec != "0 * * * *" || beta.Timezone != "America/Sao_Paulo" {
		t.Fatalf("beta=%+v", beta)
	}
}

func TestJobScheduleKey(t *testing.T) {
	t.Parallel()
	got := jobScheduleKey(map[string]any{"workspace_id": "ws1", "name": "alpha"})
	if got != "ws1/alpha" {
		t.Fatalf("got %q", got)
	}
}

func TestCommandsAlphabetical(t *testing.T) {
	t.Parallel()
	app := NewApp(Options{})
	assertCommandsSorted(t, "trebi", app.Commands)
}

func assertCommandsSorted(t *testing.T, path string, cmds []*urfaveCommand) {
	t.Helper()
	var visible []string
	for _, c := range cmds {
		if c == nil || c.Hidden {
			continue
		}
		visible = append(visible, c.Name)
		assertCommandsSorted(t, path+" "+c.Name, c.Commands)
	}
	if !sort.StringsAreSorted(visible) {
		t.Fatalf("%s subcommands not alphabetical: %v", path, visible)
	}
}

func TestRunFromEventCLI(t *testing.T) {
	var path string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		body = nil
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch path {
		case "/v1/events/01EV/jobs":
			_, _ = io.WriteString(w, `{"jobs":[{"workspace":"ws1","job":"reply","on_index":0,"manual":true,"ready":false,"asks":["body"],"gate":{"matched":true}}]}`)
		case "/v1/jobs/simulate":
			_, _ = io.WriteString(w, `{"summary":{"events":1,"matched":1,"ready":0},"results":[{"event_id":"01EV","on_index":0,"gate":{"matched":true},"ready":false,"missing":["body"]}]}`)
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"01RUN","status":"queued"}`)
		}
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}

	out, err := runCLI(t, client, "event", "jobs", "01EV")
	if err != nil || !strings.Contains(out, "reply") || !strings.Contains(out, "body") {
		t.Fatalf("event jobs: %v\n%s", err, out)
	}
	if _, err := runCLI(t, client, "event", "run", "01EV", "--job", "reply", "-w", "ws1", "--input", "tone=warm", "--trigger", "1"); err != nil {
		t.Fatal(err)
	}
	ev, _ := body["event"].(map[string]any)
	if path != "/v1/runs" || body["job"] != "reply" || ev["id"] != "01EV" || ev["on_index"] != float64(1) || body["inputs"].(map[string]any)["tone"] != "warm" {
		t.Fatalf("event run %s %v", path, body)
	}
	out, err = runCLI(t, client, "job", "simulate", "reply", "-w", "ws1", "--event", "01EV", "--input", "body=hi")
	if err != nil || path != "/v1/jobs/simulate" || body["job"] != "reply" || !strings.Contains(out, "events=1 matched=1 ready=0") {
		t.Fatalf("simulate: %v %v\n%s", err, body, out)
	}
}
