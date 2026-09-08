package api_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
)

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
