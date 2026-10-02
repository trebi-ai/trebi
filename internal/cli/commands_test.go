package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/urfave/cli/v3"
)

func TestRunsShowAcceptsJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/runs/01ABC" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"01ABC","status":"complete"}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	out, err := runCLI(t, client, "runs", "show", "--json", "01ABC")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"id": "01ABC"`) {
		t.Fatalf("out %q", out)
	}
}

func TestJobValidateJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/validate") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"error":""}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	out, err := runCLI(t, client, "job", "validate", "--json", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"ok": true`) {
		t.Fatalf("out %q", out)
	}
}

func TestRunsTranscript(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/runs/01ABC/transcript" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"run_id":"01ABC","turns":[{"role":"assistant","text":"hi"}]}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	out, err := runCLI(t, client, "runs", "transcript", "01ABC")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"run_id": "01ABC"`) {
		t.Fatalf("out %q", out)
	}
}

func TestRunsTurnsTable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/runs/01ABC/turns":
			_, _ = w.Write([]byte(`{"turns":[
				{"n":1,"reason":"manual","status":"complete","end_reason":"result","started_at":"2026-09-30T12:00:00Z","duration_ms":61000,"session_id":"sess-a","tokens":{"cost_usd":0.0123}},
				{"n":2,"reason":"request","status":"running","started_at":"2026-09-30T14:00:00Z","session_id":"sess-a","tokens":{}}]}`))
		case "/v1/runs":
			_, _ = w.Write([]byte(`{"runs":[{"id":"01ABC","job":"demo","status":"complete","turn_count":2}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}

	out, err := runCLI(t, client, "runs", "turns", "01ABC")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"TURN", "REASON", "SESSION", "manual", "request", "1m1s", "0.0123", "sess-a"} {
		if !strings.Contains(strings.ToUpper(out), strings.ToUpper(want)) {
			t.Fatalf("turns table lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "manual") > strings.Index(out, "request") {
		t.Fatalf("turns out of order:\n%s", out)
	}

	out, err = runCLI(t, client, "runs", "turns", "--json", "01ABC")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Turns []map[string]any `json:"turns"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil || len(body.Turns) != 2 {
		t.Fatalf("turns json %q err %v", out, err)
	}

	out, err = runCLI(t, client, "runs", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToUpper(out), "TURNS") || !strings.Contains(out, "2") {
		t.Fatalf("runs list lacks the turns column:\n%s", out)
	}
}

func TestRunsMessageJoinAndError(t *testing.T) {

	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/message") {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	if _, err := runCLI(t, client, "runs", "message", "01ABC", "deploy", "the", "branch"); err != nil {
		t.Fatal(err)
	}
	if gotBody != `{"text":"deploy the branch"}` {
		t.Fatalf("body %s", gotBody)
	}

	srv400 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"validation","message":"not running"}}`))
	}))
	t.Cleanup(srv400.Close)
	c400 := &APIClient{base: srv400.URL, httpClient: srv400.Client()}
	_, err := runCLI(t, c400, "runs", "message", "01ABC", "hi")
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("want server message, got %v", err)
	}
}

func TestJobListKind(t *testing.T) {

	var jobsHits, wsHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/jobs":
			jobsHits.Add(1)
			_, _ = w.Write([]byte(`{"jobs":[{"name":"demo","kind":"job","workspace_id":"ws1","harness":"claude","active":true},{"name":"agents/cmo","kind":"agent","workspace_id":"ws1"}]}`))
		case r.URL.Path == "/v1/workspaces":
			wsHits.Add(1)
			_, _ = w.Write([]byte(`{"workspaces":[{"id":"ws1","name":"one"},{"id":"ws2","name":"two"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}

	out, err := runCLI(t, client, "job", "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "agents/cmo") {
		t.Fatalf("default kind=job leaked agent rows: %q", out)
	}
	if strings.Contains(out, "KIND") {
		t.Fatalf("kind column present for default: %q", out)
	}

	jobsHits.Store(0)
	wsHits.Store(0)
	out, err = runCLI(t, client, "job", "list", "--kind", "all")
	if err != nil {
		t.Fatal(err)
	}
	if jobsHits.Load() == 0 {
		t.Fatal("--kind all should hit /v1/jobs")
	}
	if !strings.Contains(strings.ToUpper(out), "KIND") {
		t.Fatalf("kind column missing for --kind all: %q", out)
	}

	if _, err = runCLI(t, client, "job", "list", "--kind", "assistant"); err == nil {
		t.Fatal("assistant must no longer be a job list kind")
	}
	_, err = runCLI(t, client, "job", "list", "--kind", "bogus")
	if err == nil {
		t.Fatal("bogus kind")
	}
}

func TestJobListKindBogusNoHTTP(t *testing.T) {

	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	if _, err := runCLI(t, client, "job", "list", "--kind", "bogus"); err == nil {
		t.Fatal("expected usage error")
	}
	if called.Load() {
		t.Fatal("HTTP request made")
	}
}

func TestDoctorIDAndFix(t *testing.T) {

	var fixes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/system/doctor" && r.Method == "GET":
			_, _ = w.Write([]byte(`{"findings":[{"id":"config-missing","severity":"warn","code":"CFG","message":"no config"},{"id":"unknown-x","severity":"info","code":"X","message":"nope"}]}`))
		case strings.HasSuffix(r.URL.Path, "/fix"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/system/doctor/"), "/fix")
			fixes = append(fixes, id)
			if id == "unknown-x" {
				_, _ = w.Write([]byte(`{"fixed":false,"id":"unknown-x","note":"no automatic fix"}`))
				return
			}
			_, _ = w.Write([]byte(`{"fixed":"` + id + `","action":"wrote_default_config"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	out, err := runCLI(t, client, "doctor")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "config-missing") {
		t.Fatalf("id column missing: %q", out)
	}
	out, err = runCLI(t, client, "doctor", "--fix")
	if err != nil {
		t.Fatal(err)
	}
	if len(fixes) != 2 {
		t.Fatalf("fixes %v", fixes)
	}
	if !strings.Contains(out, "skip") {
		t.Fatalf("unfixable should skip: %q", out)
	}
}

func TestDoctorJSONOffline(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })
	var buf bytes.Buffer
	app := NewApp(Options{})
	app.Writer = &buf
	app.ErrWriter = &buf
	prev := app.Before
	app.Before = func(ctx context.Context, c *cli.Command) (context.Context, error) {
		if prev != nil {
			ctx, _ = prev(ctx, c)
		}
		c.Root().Metadata[appStateKey] = &appState{clientErr: fmt.Errorf("reading api.json")}
		return ctx, nil
	}
	err := app.Run(context.Background(), []string{"trebi", "doctor", "--json"})
	if err == nil {
		t.Fatal("expected non-zero exit")
	}
	var m map[string]any
	if json.Unmarshal(buf.Bytes(), &m) != nil {
		t.Fatalf("not JSON: %q", buf.String())
	}
}

func TestConnectorCursorBodies(t *testing.T) {

	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"con_01k6abcdefghjkmnpqrstvwxyz","slug":"mail","last_event_id":"evt-1"}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	if _, err := runCLI(t, client, "connector", "cursor", "reset", "mail"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "PUT" || gotPath != "/v1/connectors/mail/cursor" || gotBody != "" {
		t.Fatalf("reset %s %s body=%q", gotMethod, gotPath, gotBody)
	}
	if _, err := runCLI(t, client, "connector", "cursor", "set", "mail", "evt-9"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"last_event_id":"evt-9"`) {
		t.Fatalf("set body %s", gotBody)
	}
	out, err := runCLI(t, client, "connector", "cursor", "show", "mail")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SLUG", "ID", "con_01k6abcdefghjkmnpqrstvwxyz", "evt-1"} {
		if !strings.Contains(strings.ToUpper(out), strings.ToUpper(want)) {
			t.Fatalf("cursor show missing %q:\n%s", want, out)
		}
	}
}

// TestConnectorSetMergesInputs proves set sends the stored inputs back as
// ***, so the daemon keeps them, and adds the inputs given.
func TestConnectorSetMergesInputs(t *testing.T) {
	var patchBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "PATCH" {
			b, _ := io.ReadAll(r.Body)
			patchBody = string(b)
		}
		_, _ = w.Write([]byte(`{"name":"api","inputs":{"TOKEN":"***"}}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	if _, err := runCLI(t, client, "connector", "set", "api", "--input", "REGION=eu", "--track", "minor"); err != nil {
		t.Fatal(err)
	}
	if patchBody != `{"inputs":{"REGION":"eu","TOKEN":"***"},"track":"minor"}` {
		t.Fatalf("patch body %s", patchBody)
	}
}

// TestConnectorSetCreatesInline proves set with --manifest-file creates an
// inline entry when the connector does not exist.
func TestConnectorSetCreatesInline(t *testing.T) {
	var method, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no"}}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		method, body = r.Method+" "+r.URL.Path, string(b)
		_, _ = w.Write([]byte(`{"name":"tick"}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	path := filepath.Join(t.TempDir(), "m.yaml")
	if err := os.WriteFile(path, []byte("name: tick\nevents:\n  command: tick.sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, client, "connector", "set", "tick", "--manifest-file", path); err != nil {
		t.Fatal(err)
	}
	if method != "POST /v1/connectors" || body != `{"from":"inline","manifest":{"events":{"command":"tick.sh"},"name":"tick"},"slug":"tick"}` {
		t.Fatalf("%s %s", method, body)
	}
}

// TestConnectorApplyCreatesOrPatches proves apply reads a slug-keyed file,
// creates the connection when the slug is new, and else sends a PATCH to
// its id.
func TestConnectorApplyCreatesOrPatches(t *testing.T) {
	var got []string
	exists := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"con_01k6abcdefghjkmnpqrstvwxyz","slug":"wa-work"}`))
			return
		}
		got = append(got, r.Method+" "+r.URL.Path+" "+string(b))
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("type: whatsapp\nslug: wa-work\ntitle: Work phone\nfrom: catalog\nversion: 0.7.0\ntrack: minor\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, client, "connector", "apply", "--file", path); err != nil {
		t.Fatal(err)
	}
	exists = true
	if _, err := runCLI(t, client, "connector", "apply", "--file", path); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`POST /v1/connectors {"from":"catalog","slug":"wa-work","title":"Work phone","track":"minor","type":"whatsapp","version":"0.7.0"}`,
		`PATCH /v1/connectors/con_01k6abcdefghjkmnpqrstvwxyz {"title":"Work phone","track":"minor","version":"0.7.0"}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// The old name-keyed form has no slug.
	if err := os.WriteFile(path, []byte("whatsapp:\n  from: catalog\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, client, "connector", "apply", "--file", path); err == nil || !strings.Contains(err.Error(), "slug") {
		t.Fatalf("name-keyed file err = %v", err)
	}
}

// TestConnectorAddSendsSlugAndTitle proves add sends the slug and title of
// the new connection, and install stays an alias.
func TestConnectorAddSendsSlugAndTitle(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+string(b))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"con_01k6abcdefghjkmnpqrstvwxyz","slug":"wa-work"}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	out, err := runCLI(t, client, "connector", "add", "whatsapp", "--slug", "wa-work", "--title", "Work phone")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "wa-work") || !strings.Contains(out, "con_01k6abcdefghjkmnpqrstvwxyz") {
		t.Fatalf("out = %s", out)
	}
	if _, err := runCLI(t, client, "connector", "install", "whatsapp"); err != nil {
		t.Fatalf("install alias: %v", err)
	}
	if len(got) != 2 || !strings.HasPrefix(got[0], "POST /v1/connectors/install ") ||
		!strings.Contains(got[0], `"slug":"wa-work"`) || !strings.Contains(got[0], `"title":"Work phone"`) {
		t.Fatalf("requests %q", got)
	}
	if strings.Contains(got[1], `"slug"`) || strings.Contains(got[1], `"as"`) {
		t.Fatalf("add with no slug must let the daemon pick it: %s", got[1])
	}
	if _, err := runCLI(t, client, "connector", "add", "whatsapp", "--as", "x"); err == nil {
		t.Fatal("--as must be gone")
	}
}

// TestConnectorSetSlug proves set --slug sends the new slug and names it.
func TestConnectorSetSlug(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method == "PATCH" && r.URL.Path == "/v1/connectors/wa" {
			body = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	out, err := runCLI(t, client, "connector", "set", "wa", "--slug", "wa-work", "--title", "Work phone")
	if err != nil {
		t.Fatal(err)
	}
	if body != `{"slug":"wa-work","title":"Work phone"}` || !strings.Contains(out, "connector wa-work set") {
		t.Fatalf("body %s out %s", body, out)
	}
}

func TestConnectorLoginFollowsSteps(t *testing.T) {
	var submitted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/connectors/wa/login":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"flow_id":"f1","instance":"wa","step":{"kind":"qr","data":"QRDATA"}}`))
		case "GET /v1/connectors/wa/login/f1":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: ready\ndata: {}\n\n"+
				"event: step\ndata: {\"flow_id\":\"f1\",\"step\":{\"kind\":\"qr\",\"data\":\"QRDATA\"}}\n\n"+
				"event: step\ndata: {\"flow_id\":\"f1\",\"step\":{\"kind\":\"input\",\"fields\":[{\"name\":\"code\",\"label\":\"Code\"}]}}\n\n"+
				"event: done\ndata: {\"flow_id\":\"f1\",\"done\":{\"flow_id\":\"f1\",\"ok\":true,\"account\":{\"id\":\"+1555\"}}}\n\n")
		case "POST /v1/connectors/wa/login/f1":
			b, _ := io.ReadAll(r.Body)
			submitted = string(b)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	out, err := runCLIWithInput(t, client, "123456\n", "connector", "login", "wa")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Scan this code with the app:", "█", "Code: ", "logged in as +1555"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if submitted != `{"fields":{"code":"123456"}}` {
		t.Fatalf("submit %s", submitted)
	}
}

func TestConnectorSessionRequests(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.RequestURI()+" "+string(b))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"override":{"patch":{"x":1}}}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	runs := [][]string{
		{"connector", "rooms", "wa", "--query", "fam"},
		{"connector", "threads", "wa", "r1"},
		{"connector", "bind", "wa", "r1", "--workspace", "ws1", "--operator", "agents/triage"},
		{"connector", "logout", "wa"},
		{"connector", "restart", "wa"},
		{"connector", "override", "set", "wa", "--workspace", "ws1", "--disabled"},
	}
	for _, args := range runs {
		if _, err := runCLI(t, client, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	want := []string{
		"GET /v1/connectors/wa/rooms?q=fam ",
		"GET /v1/connectors/wa/rooms/r1/threads ",
		`POST /v1/connectors/wa/rooms/r1/bind {"operator":"agents/triage","workspace":"ws1"}`,
		"POST /v1/connectors/wa/logout {}",
		"POST /v1/connectors/wa/restart ",
		"GET /v1/workspaces/ws1/connectors/wa ",
		`PUT /v1/workspaces/ws1/connectors/wa {"disabled":true,"patch":{"x":1}}`,
	}
	for i, w := range want {
		if i >= len(got) || strings.TrimSpace(got[i]) != strings.TrimSpace(w) {
			t.Fatalf("request %d = %q, want %q (all %q)", i, got, w, got)
		}
	}
	// A connection is one ref: the instance flag is gone.
	for _, args := range [][]string{
		{"connector", "threads", "wa", "r1", "--instance", "wa@ws1"},
		{"connector", "restart", "wa", "--instance", "wa@ws1"},
		{"connector", "override", "set", "wa", "--workspace", "ws1", "--input", "B=2"},
	} {
		if _, err := runCLI(t, client, args...); err == nil {
			t.Errorf("%v: want a flag error", args)
		}
	}
}

func TestWorkspaceRemoveClearsSticky(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })
	if err := home.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"deleted":"ws1"}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}

	st1 := StickyContext{WorkspaceID: "ws1", Name: "one"}
	if err := st1.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, client, "workspace", "remove", "--force", "ws1"); err != nil {
		t.Fatal(err)
	}
	st, err := LoadStickyContext()
	if err != nil {
		t.Fatal(err)
	}
	if st != nil {
		t.Fatalf("sticky should be cleared, got %+v", st)
	}

	st2 := StickyContext{WorkspaceID: "ws-other", Name: "other"}
	if err := st2.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, client, "workspace", "remove", "--force", "ws1"); err != nil {
		t.Fatal(err)
	}
	st, err = LoadStickyContext()
	if err != nil || st == nil || st.WorkspaceID != "ws-other" {
		t.Fatalf("other sticky lost: %+v %v", st, err)
	}
}

func TestRulesTestEventFileAndStdin(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"matched":true,"vars":{"k":"v"},"error":""}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	f := filepath.Join(t.TempDir(), "ev.json")
	if err := os.WriteFile(f, []byte(`{"type":"mail"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, client, "rules", "test", "--match", "true", "--event-file", f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"type":"mail"`) || !strings.Contains(out, "matched=true") {
		t.Fatalf("file path out=%q body=%s", out, gotBody)
	}

	old := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(`{"type":"stdin"}`))
	_ = w.Close()
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old })
	out, err = runCLI(t, client, "rules", "test", "--match", "true")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"type":"stdin"`) {
		t.Fatalf("stdin body %s", gotBody)
	}
	_ = out
}

func TestRunsAgentsEmptyNote(t *testing.T) {

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sessions":{}}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	out, err := runCLI(t, client, "runs", "sessions")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "fresh daemon") && !strings.Contains(out, "in-memory") {
		t.Fatalf("missing empty note: %q", out)
	}
}

func TestWorkspaceFlagResolvesName(t *testing.T) {
	var gotJobsQ string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/workspaces":
			_, _ = w.Write([]byte(`{"workspaces":[{"id":"ws_ops","name":"ops"},{"id":"ws_stg","name":"staging"}]}`))
		case "/v1/jobs":
			gotJobsQ = r.URL.Query().Get("workspace")
			_, _ = w.Write([]byte(`{"jobs":[{"name":"report","kind":"job","workspace_id":"ws_stg"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	if _, err := runCLI(t, client, "-w", "staging", "job", "list", "--json"); err != nil {
		t.Fatal(err)
	}
	if gotJobsQ != "ws_stg" {
		t.Fatalf("workspace query %q want ws_stg", gotJobsQ)
	}
}

func TestSecretSetFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.txt")
	if err := os.WriteFile(path, []byte("sk-from-file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != "/v1/secrets/eval_api_key" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"eval_api_key"}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	if _, err := runCLI(t, client, "secret", "set", "--from-file", path, "eval_api_key"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"value":"sk-from-file"`) {
		t.Fatalf("body %s", gotBody)
	}
}

func TestSecretListTableJSON(t *testing.T) {

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"secrets":["alpha","beta"]}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	out, err := runCLI(t, client, "secret", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"secrets"`) {
		t.Fatalf("%q", out)
	}
}

func TestLicenseShowUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"supported":false,"source":"none"}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	out, err := runCLI(t, client, "license", "show")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "license: unsupported") {
		t.Fatalf("%q", out)
	}
}

func runCLI(t *testing.T, client *APIClient, args ...string) (string, error) {
	t.Helper()
	return runCLIWithInput(t, client, "", args...)
}

// runCLIWithInput runs the CLI with stdin set to input.
func runCLIWithInput(t *testing.T, client *APIClient, input string, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	app := NewApp(Options{})
	if input != "" {
		app.Reader = strings.NewReader(input)
	}
	app.Writer = &buf
	app.ErrWriter = &buf
	prev := app.Before
	app.Before = func(ctx context.Context, c *cli.Command) (context.Context, error) {
		if prev != nil {
			ctx, _ = prev(ctx, c)
		}
		st := appStateFor(c)
		st.client = client
		if client == nil {
			st.client = nil
			st.clientErr = fmt.Errorf("API client is not configured")
		} else {
			st.clientErr = nil
		}
		return ctx, nil
	}
	err := app.Run(context.Background(), append([]string{"trebi"}, args...))
	return buf.String(), err
}

func TestConfigShowUsesAPIEntries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/config" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"config":{},"entries":[
			{"key":"defaults.command","value":"grok","source":"config","default":"claude","description":"Default harness CLI when a job omits command"},
			{"key":"terminal","value":"Ghostty","source":"config","default":"Terminal","description":"macOS app"}
		]}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}

	out, err := runCLI(t, client, "config", "show")
	if err != nil {
		t.Fatal(err)
	}
	// Server values win over anything in the local file.
	for _, want := range []string{"SOURCE", "DEFAULT", "DESCRIPTION", "grok", "Ghostty", "claude"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}

	// --key filters the same entry list.
	out, err = runCLI(t, client, "config", "show", "--key", "terminal")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "defaults.command") || !strings.Contains(out, "Ghostty") {
		t.Fatalf("filter: %s", out)
	}
}

func TestConfigShowOfflineListsAllKeys(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })

	out, err := runCLI(t, nil, "config", "show", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var entries []config.PathEntry
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if len(entries) != len(config.SortedPaths()) {
		t.Fatalf("got %d entries, want %d", len(entries), len(config.SortedPaths()))
	}
	for _, e := range entries {
		if e.Description == "" || e.Source == "" || e.Default == "" || e.Value == "" {
			t.Fatalf("incomplete row: %+v", e)
		}
	}
}

func TestConfigBareShowsHelp(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })

	out, err := runCLI(t, nil, "config")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "COMMANDS") || !strings.Contains(out, "show") {
		t.Fatalf("want help, got: %s", out)
	}
	// Bare config must not render the table.
	if strings.Contains(out, "DESCRIPTION") {
		t.Fatalf("bare config ran show: %s", out)
	}
}

func TestTruncateDisplay(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"short", 40, "short"},
		{"exactly-ten", 11, "exactly-ten"},
		{"abcdefghij", 5, "ab..."},
		{"abcdefghij", 3, "abc"},
		{"abcdefghij", 0, "abcdefghij"},
		// Multi-byte runes must not be split mid-character.
		{"→→→→→→→→→→", 5, "→→..."},
		{"日本語のテキストです", 6, "日本語..."},
	}
	for _, c := range cases {
		if got := truncateDisplay(c.in, c.max); got != c.want {
			t.Errorf("truncateDisplay(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
		if !utf8.ValidString(truncateDisplay(c.in, c.max)) {
			t.Errorf("truncateDisplay(%q, %d) split a rune", c.in, c.max)
		}
	}
}

func TestConfigShowTruncatesValue(t *testing.T) {
	long := strings.Repeat("x", 120)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"config":{},"entries":[
			{"key":"notify.email","value":"` + long + `","source":"config","default":"-","description":"Shell cmd"}
		]}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}

	out, err := runCLI(t, client, "config", "show")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, long) {
		t.Fatal("table printed the untruncated value")
	}
	if !strings.Contains(out, "...") {
		t.Fatalf("want ellipsis: %s", out)
	}

	// --json keeps the full value.
	out, err = runCLI(t, client, "config", "show", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, long) {
		t.Fatal("--json truncated the value")
	}
}

func TestFileListAndPut(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/workspaces":
			_, _ = w.Write([]byte(`{"workspaces":[{"id":"ws1","name":"one"}]}`))
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/files"):
			_, _ = w.Write([]byte(`{"files":[{"path":"a.py","size_bytes":3,"mtime":"2026-08-28T14:00:00Z"}],"truncated":false}`))
		case r.Method == "PUT":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"path":"a.py","hash":"abc","size_bytes":3,"created":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	out, err := runCLI(t, client, "file", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"a.py"`) {
		t.Fatalf("list: %s", out)
	}
	out, err = runCLI(t, client, "file", "put", "a.py")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != "PUT" || !strings.HasSuffix(gotPath, "/files/a.py") {
		t.Fatalf("put %s %s body=%q out=%s", gotMethod, gotPath, gotBody, out)
	}
}

func TestJobFileListAndPut(t *testing.T) {
	var gotMethod, gotPath, gotRawQuery string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotRawQuery = r.Method, r.URL.Path, r.URL.RawQuery
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/jobs/report/files"):
			_, _ = w.Write([]byte(`{"files":[{"path":"a.py","size_bytes":3,"mtime":"2026-08-28T14:00:00Z"}],"truncated":false}`))
		case r.Method == "PUT":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"path":"a.py","hash":"abc","size_bytes":3,"created":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}

	out, err := runCLI(t, client, "--workspace", "ws1", "job", "file", "list", "--json", "report")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"a.py"`) {
		t.Fatalf("list: %s", out)
	}
	if gotPath != "/v1/jobs/report/files" || !strings.Contains(gotRawQuery, "workspace=ws1") {
		t.Fatalf("list path %s query %s", gotPath, gotRawQuery)
	}

	out, err = runCLI(t, client, "--workspace", "ws1", "job", "file", "put", "report", "a.py")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != "PUT" || gotPath != "/v1/jobs/report/files/a.py" {
		t.Fatalf("put %s %s body=%q out=%s", gotMethod, gotPath, gotBody, out)
	}
	if !strings.Contains(gotRawQuery, "workspace=ws1") {
		t.Fatalf("put missing workspace: %s", gotRawQuery)
	}
}

func TestFlagsAfterPositional(t *testing.T) {
	var gotPath, gotRawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotRawQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/jobs/report/memory":
			_, _ = w.Write([]byte(`{"content":"ledger text","entries":[]}`))
		case "/v1/jobs/report":
			_, _ = w.Write([]byte(`{"name":"report","workspace_id":"staging"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}

	out, err := runCLI(t, client, "job", "memory", "show", "report", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"content": "ledger text"`) {
		t.Fatalf("expected JSON (--json after name); got %q", out)
	}

	out, err = runCLI(t, client, "job", "show", "report", "-w", "staging")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/jobs/report" || !strings.Contains(gotRawQuery, "workspace=staging") {
		t.Fatalf("expected -w after name; path %s query %s out %s", gotPath, gotRawQuery, out)
	}
}

func TestRenderLocalOnly(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	app := &cli.Command{Writer: &buf}
	err := renderLocalOnly(app, fmt.Errorf("precondition_failed: cursor login needs a browser on the daemon machine"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(buf.String(), "log in on the daemon machine") {
		t.Fatalf("output %q", buf.String())
	}
}

// `trebi workspace set --git-enabled` must send git_enabled, so a rename plus
// git tracking in one call reaches the API as both fields.
func TestWorkspaceSetSendsGitEnabled(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ws1","name":"platform-ops","git_enabled":true}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}

	if _, err := runCLI(t, client, "workspace", "set", "ws1", "--name", "platform-ops", "--git-enabled"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"git_enabled":true`) || !strings.Contains(gotBody, `"name":"platform-ops"`) {
		t.Fatalf("body %s", gotBody)
	}
}

// `workspace show` must reach GET /v1/workspaces/{id}. An alias on a sibling
// command once shadowed it, so the call silently listed instead.
func TestWorkspaceShowHitsGetRoute(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ws1","name":"ops","git_enabled":true}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}

	out, err := runCLI(t, client, "workspace", "show", "ops", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/workspaces/ops" {
		t.Fatalf("path %q", gotPath)
	}
	if !strings.Contains(out, `"git_enabled": true`) {
		t.Fatalf("out %q", out)
	}
}

func TestWorkspaceMoveHitsPostRoute(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() { home.SetBase("") })
	if err := home.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	var gotMethod, gotPath, gotBody, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ws1","name":"ops","path":"/tmp/dest","rewritten":["alpha/JOB.md"]}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}

	if _, err := runCLI(t, client, "workspace", "move", "ops", "/tmp/dest"); err == nil {
		t.Fatal("want confirm error without --force")
	}
	out, err := runCLI(t, client, "workspace", "move", "ops", "/tmp/dest", "--force")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != "POST" || gotPath != "/v1/workspaces/ops/move" {
		t.Fatalf("%s %s", gotMethod, gotPath)
	}
	if !strings.Contains(gotQuery, "confirm=1") {
		t.Fatalf("query %s", gotQuery)
	}
	if !strings.Contains(gotBody, `"path":"/tmp/dest"`) {
		t.Fatalf("body %s", gotBody)
	}
	if !strings.Contains(out, "moved") || !strings.Contains(out, "/tmp/dest") {
		t.Fatalf("out %q", out)
	}

	gotQuery = ""
	if _, err := runCLI(t, client, "workspace", "move", "ops", "/tmp/dest", "--dry-run"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "confirm=1") || !strings.Contains(gotBody, `"dry_run":true`) {
		t.Fatalf("dry-run query=%s body=%s", gotQuery, gotBody)
	}
}

func TestConnectorEntryAndSearchCursor(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/connectors/catalog/entry":
			_, _ = w.Write([]byte(`{"registry":"official","name":"acme/weather","version":"1.2.0",
				"packages":[{"identifier":"@acme/weather"}],"remotes":[{"url":"https://w.example/mcp"}],
				"package_inputs":[[{"name":"WEATHER_API_KEY","kind":"env","required":true}]],"remote_inputs":[[]]}`))
		case "/v1/connectors/catalog":
			_, _ = w.Write([]byte(`{"entries":[{"registry":"official","name":"acme/weather"}],"next_cursor":"p2"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}

	out, err := runCLI(t, client, "connector", "entry", "acme/weather", "--registry", "official", "--version", "1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"@acme/weather", "WEATHER_API_KEY*", "https://w.example/mcp"} {
		if !strings.Contains(out, want) {
			t.Fatalf("entry output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(paths[0], "name=acme%2Fweather") || !strings.Contains(paths[0], "version=1.2.0") || !strings.Contains(paths[0], "registry=official") {
		t.Fatalf("entry path %s", paths[0])
	}

	out, err = runCLI(t, client, "connector", "search", "weather", "--cursor", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(paths[1], "cursor=p1") {
		t.Fatalf("search path %s", paths[1])
	}
	if !strings.Contains(out, "more: --cursor p2") {
		t.Fatalf("search output:\n%s", out)
	}
}
