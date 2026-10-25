package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConnectorScopeCommands proves scope shows names, sets a list or
// global, and add and apply send --scope.
func TestConnectorScopeCommands(t *testing.T) {
	var got []string
	scope := `["ws_aaaa1111"]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/workspaces":
			_, _ = w.Write([]byte(`{"workspaces":[{"id":"ws_aaaa1111","name":"ocral"},{"id":"ws_bbbb2222","name":"btd"}]}`))
			return
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/connectors/"):
			_, _ = w.Write([]byte(`{"id":"con_01k6abcdefghjkmnpqrstvwxyz","slug":"wa","scope":` + scope + `}`))
			return
		}
		got = append(got, r.Method+" "+r.URL.Path+" "+string(b))
		_, _ = w.Write([]byte(`{"id":"con_01k6abcdefghjkmnpqrstvwxyz","slug":"wa","scope":["global"]}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}

	out, err := runCLI(t, client, "connector", "scope", "wa")
	if err != nil || strings.TrimSpace(out) != "wa: ocral" {
		t.Fatalf("show %q %v", out, err)
	}
	out, err = runCLI(t, client, "connector", "scope", "wa", "global")
	if err != nil || strings.TrimSpace(out) != "wa: global" {
		t.Fatalf("set global %q %v", out, err)
	}
	if _, err := runCLI(t, client, "connector", "scope", "wa", "ocral, btd"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, client, "connector", "add", "whatsapp", "--scope", "global"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("type: whatsapp\nslug: wa\nfrom: catalog\nscope: [ocral]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, client, "connector", "apply", "--file", path); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, client, "connector", "apply", "--file", path, "--scope", "btd"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`PATCH /v1/connectors/wa {"scope":["global"]}`,
		`PATCH /v1/connectors/wa {"scope":["ocral","btd"]}`,
		`"scope":["global"]`,
		`PATCH /v1/connectors/con_01k6abcdefghjkmnpqrstvwxyz {"scope":["ocral"]}`,
		`PATCH /v1/connectors/con_01k6abcdefghjkmnpqrstvwxyz {"scope":["btd"]}`,
	}
	if len(got) != len(want) {
		t.Fatalf("requests %q", got)
	}
	for i, w := range want {
		if !strings.Contains(got[i], w) {
			t.Fatalf("request %d = %s; want %s", i, got[i], w)
		}
	}
	if !strings.HasPrefix(got[2], "POST /v1/connectors/install ") {
		t.Fatalf("add %s", got[2])
	}
}

// TestConnectorEventsCommand proves events shows each type with its counts,
// and sends all, none, and only with --other as a receive patch.
func TestConnectorEventsCommand(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "GET" {
			got = append(got, r.Method+" "+r.URL.Path+" "+string(b))
		}
		_, _ = w.Write([]byte(`{"id":"con_01k6abcdefghjkmnpqrstvwxyz","slug":"vnc","receive":{"mode":"some","types":["screen.stable"]},
			"event_counts":[{"type":"screen.stable","description":"Screen is stable","declared":true,"kept":2,"skipped":0},
			{"type":"screen.changed","description":"Screen changed","declared":true,"kept":0,"skipped":21}]}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}

	out, err := runCLI(t, client, "connector", "events", "vnc")
	if err != nil || !strings.Contains(out, "vnc: receives some events") {
		t.Fatalf("show %q %v", out, err)
	}
	for _, want := range []string{"screen.stable", "screen.changed", "21", "(other)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show %q lacks %q", out, want)
		}
	}
	for _, args := range [][]string{{"all"}, {"none"}, {"only", "screen.stable,state.changed", "--other"}} {
		if _, err := runCLI(t, client, append([]string{"connector", "events", "vnc"}, args...)...); err != nil {
			t.Fatal(args, err)
		}
	}
	if _, err := runCLI(t, client, "connector", "events", "vnc", "all", "--other"); err == nil {
		t.Fatal("all --other passed")
	}
	want := []string{
		`PATCH /v1/connectors/vnc {"receive":{"mode":"all"}}`,
		`PATCH /v1/connectors/vnc {"receive":{"mode":"none"}}`,
		`PATCH /v1/connectors/vnc {"receive":{"mode":"some","other":true,"types":["screen.stable","state.changed"]}}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
