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
	client := &APIClient{base: srv.URL, token: "t", httpClient: srv.Client()}
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
	client := &APIClient{base: srv.URL, token: "t", httpClient: srv.Client()}
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
	client := &APIClient{base: srv.URL, token: "t", httpClient: srv.Client()}
	out, err := runCLI(t, client, "runs", "transcript", "01ABC")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"run_id": "01ABC"`) {
		t.Fatalf("out %q", out)
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
	client := &APIClient{base: srv.URL, token: "t", httpClient: srv.Client()}
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
	c400 := &APIClient{base: srv400.URL, token: "t", httpClient: srv400.Client()}
	_, err := runCLI(t, c400, "runs", "message", "01ABC", "hi")
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("want server message, got %v", err)
	}
}

func TestJobListKind(t *testing.T) {

	var jobsHits, wsHits, asstHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/jobs":
			jobsHits.Add(1)
			_, _ = w.Write([]byte(`{"jobs":[{"name":"demo","kind":"job","workspace_id":"ws1","harness":"claude","active":true},{"name":"asst","kind":"assistant","workspace_id":"ws1"}]}`))
		case r.URL.Path == "/v1/workspaces":
			wsHits.Add(1)
			_, _ = w.Write([]byte(`{"workspaces":[{"id":"ws1","name":"one"},{"id":"ws2","name":"two"}]}`))
		case strings.HasSuffix(r.URL.Path, "/assistant"):
			asstHits.Add(1)
			id := strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/assistant"), "/v1/workspaces/")
			if id == "ws2" {
				http.Error(w, "down", 500)
				return
			}
			_, _ = w.Write([]byte(`{"name":"buddy","harness":"claude","active":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, token: "t", httpClient: srv.Client()}

	out, err := runCLI(t, client, "job", "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "asst") {
		t.Fatalf("default kind=job leaked assistant: %q", out)
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

	jobsHits.Store(0)
	out, err = runCLI(t, client, "job", "list", "--kind", "assistant", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if jobsHits.Load() != 0 {
		t.Fatal("--kind assistant must not hit /v1/jobs")
	}
	if wsHits.Load() == 0 || asstHits.Load() == 0 {
		t.Fatal("--kind assistant should fan out workspaces")
	}
	var env struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&env); err != nil {
		t.Fatalf("json %v %q", err, out)
	}
	if len(env.Jobs) != 1 {
		t.Fatalf("jobs envelope %q", out)
	}
	if !strings.Contains(out, "1 workspace(s) unreachable") {
		t.Fatalf("skip count missing: %q", out)
	}

	_, err = runCLI(t, client, "job", "list", "--kind", "bogus")
	if err == nil {
		t.Fatal("bogus kind")
	}
	_, err = runCLI(t, client, "job", "list", "--kind", "assistant", "--schedule")
	if err == nil || !strings.Contains(err.Error(), "--schedule") {
		t.Fatalf("schedule+assistant: %v", err)
	}
}

func TestJobListKindBogusNoHTTP(t *testing.T) {

	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, token: "t", httpClient: srv.Client()}
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
	client := &APIClient{base: srv.URL, token: "t", httpClient: srv.Client()}
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
	app := NewApp()
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
		_, _ = w.Write([]byte(`{"name":"mail","last_event_id":""}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, token: "t", httpClient: srv.Client()}
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
	client := &APIClient{base: srv.URL, token: "t", httpClient: srv.Client()}

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
	client := &APIClient{base: srv.URL, token: "t", httpClient: srv.Client()}
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
		_, _ = w.Write([]byte(`{"agents":{}}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, token: "t", httpClient: srv.Client()}
	out, err := runCLI(t, client, "runs", "agents")
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
	client := &APIClient{base: srv.URL, token: "t", httpClient: srv.Client()}
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
	client := &APIClient{base: srv.URL, token: "t", httpClient: srv.Client()}
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
	client := &APIClient{base: srv.URL, token: "t", httpClient: srv.Client()}
	out, err := runCLI(t, client, "secret", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"secrets"`) {
		t.Fatalf("%q", out)
	}
}

func runCLI(t *testing.T, client *APIClient, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	app := NewApp()
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
	client := &APIClient{base: srv.URL, token: "t", httpClient: srv.Client()}

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
	client := &APIClient{base: srv.URL, token: "t", httpClient: srv.Client()}

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
	client := &APIClient{base: srv.URL, token: "t", httpClient: srv.Client()}
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
	client := &APIClient{base: srv.URL, token: "t", httpClient: srv.Client()}

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
	client := &APIClient{base: srv.URL, token: "t", httpClient: srv.Client()}

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
	client := &APIClient{base: srv.URL, token: "t", httpClient: srv.Client()}

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
	client := &APIClient{base: srv.URL, token: "t", httpClient: srv.Client()}

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
