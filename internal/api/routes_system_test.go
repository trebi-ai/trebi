package api_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/api"
	"github.com/trebi-ai/trebi/internal/home"
)

// TestSystemVersionFields pins the release contract: version is always
// present, ui_version only when a UI is embedded (Part C of the release
// versioning plan).
func TestSystemVersionFields(t *testing.T) {
	oldVersion, oldUI := api.Version, api.UIVersion
	t.Cleanup(func() { api.Version, api.UIVersion = oldVersion, oldUI })
	api.Version, api.UIVersion = "0.2.1", ""

	s, base := startTestServer(t)

	code, body, _ := doJSON(t, "GET", base+"/v1/system/health", s.Token(), nil)
	if code != 200 {
		t.Fatalf("health %d %v", code, body)
	}
	if body["version"] != "0.2.1" {
		t.Fatalf("health version=%v", body["version"])
	}
	if _, ok := body["ui_version"]; ok {
		t.Fatalf("plain build must omit ui_version: %v", body)
	}

	api.UIVersion = "0.5.3"
	code, body, _ = doJSON(t, "GET", base+"/v1/system/health", s.Token(), nil)
	if code != 200 || body["ui_version"] != "0.5.3" {
		t.Fatalf("health with ui %d %v", code, body)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/system/overview", s.Token(), nil)
	if code != 200 || body["ui_version"] != "0.5.3" {
		t.Fatalf("overview with ui %d %v", code, body)
	}
}

func TestSystemUpdateStubHasSource(t *testing.T) {
	s, base := startTestServer(t)
	code, body, _ := doJSON(t, "GET", base+"/v1/system/update", s.Token(), nil)
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
	s, base := startTestServer(t)
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

	code, body, _ := doJSON(t, "GET", base+"/v1/system/logs?lines=3", s.Token(), nil)
	if code != 200 {
		t.Fatalf("lines %d %v", code, body)
	}
	got := logLines(t, body)
	if len(got) != 3 {
		t.Fatalf("lines=3 got %d %v", len(got), got)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/system/logs?grep=hello", s.Token(), nil)
	if code != 200 {
		t.Fatalf("grep %d %v", code, body)
	}
	got = logLines(t, body)
	if len(got) != 2 {
		t.Fatalf("grep hello got %d %v", len(got), got)
	}

	code, body, _ = doJSON(t, "GET", base+"/v1/system/logs?level=warn", s.Token(), nil)
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
	code, body, _ = doJSON(t, "GET", base+"/v1/system/logs?lines=500", s.Token(), nil)
	if code != 200 {
		t.Fatalf("cap %d %v", code, body)
	}
	got = logLines(t, body)
	if len(got) != 200 {
		t.Fatalf("cap 200 got %d", len(got))
	}
}

func TestSystemLogsRunFilter(t *testing.T) {
	s, base := startTestServer(t)
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

	code, resp, _ := doJSON(t, "GET", base+"/v1/system/logs?run=01RUN", s.Token(), nil)
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

	code, resp, _ = doJSON(t, "GET", base+"/v1/system/logs?run=01RUN&grep=finished", s.Token(), nil)
	if code != 200 {
		t.Fatalf("run+grep %d %v", code, resp)
	}
	if got = logLines(t, resp); len(got) != 1 || !strings.Contains(got[0], "run finished") {
		t.Fatalf("run+grep got %v", got)
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
