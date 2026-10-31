package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/hometest"

	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/connectors/protocol/fakeadapter"
	"github.com/trebi-ai/trebi/internal/visor"
)

// TestMain lets the test binary run as a fake adapter for the local
// conformance check, or as the holder of a visor session.
func TestMain(m *testing.M) {
	if fakeadapter.Enabled() {
		os.Exit(fakeadapter.Main())
	}
	if code, ok := visor.Hold(os.Args); ok {
		os.Exit(code)
	}
	hometest.Main(m)
}

func TestConnectorConformanceLocal(t *testing.T) {
	dir := t.TempDir()
	sb := &fakeadapter.Sandbox{
		Result: protocol.InitializeResult{
			Features: []string{"rooms.list"}, Events: []protocol.EventDecl{{Type: "message"}},
		},
		Methods:   []protocol.LoginMethod{{ID: "qr", Type: protocol.LoginPhone, Available: true}},
		Rooms:     []protocol.Room{{ID: "r1"}},
		Reference: true,
	}
	path := filepath.Join(dir, "sandbox.json")
	if err := sb.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	// The runner gives the adapter a clean env, so the command sets the
	// switch itself.
	command := fakeadapter.Env + "=sandbox:" + path + " " + `"` + os.Args[0] + `"`
	manifestYAML := `schema: trebi-connector/1
name: chat
title: Chat
description: A test chat.
version: 1.0.0
setup:
  login: [{id: qr, type: phone}]
events:
  protocol: trebi-connector/1
  command: '` + command + `'
  types: [{type: message}]
channel:
  features: [rooms.list]
`
	if err := os.WriteFile(filepath.Join(dir, "trebi-connector.yaml"), []byte(manifestYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, nil, "connector", "conformance", "--manifest", dir, "--room", "r1")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"initialize", "shutdown"} {
		if !strings.Contains(out, want) {
			t.Fatalf("no %q in\n%s", want, out)
		}
	}
	if statusColumn(out, "fail") || !statusColumn(out, "pass") {
		t.Fatalf("want pass rows and no fail row:\n%s", out)
	}

	// A promise the adapter does not keep fails the command.
	manifestYAML = strings.Replace(manifestYAML, "[rooms.list]", "[rooms.list, reactions]", 1)
	if err := os.WriteFile(filepath.Join(dir, "trebi-connector.yaml"), []byte(manifestYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runCLI(t, nil, "connector", "conformance", "--manifest", dir); err == nil {
		t.Fatalf("a missing feature must fail:\n%s", out)
	}
}

func TestConnectorConformanceLive(t *testing.T) {
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "POST /v1/connectors/wa/conformance" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &sent)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"wa","instance":"wa","ok":false,"results":[{"check":"ping","status":"fail","message":"timeout"}]}`))
	}))
	t.Cleanup(srv.Close)
	client := &APIClient{base: srv.URL, httpClient: srv.Client()}
	out, err := runCLI(t, client, "connector", "conformance", "wa", "--room", "r9")
	if err == nil || !strings.Contains(out, "timeout") {
		t.Fatalf("err = %v, out = %s", err, out)
	}
	if sent["room"] != "r9" {
		t.Fatalf("body = %v", sent)
	}
}

// statusColumn reports whether a row of the conformance table has status in
// its Status column.
func statusColumn(out, status string) bool {
	for _, line := range strings.Split(out, "\n") {
		cells := strings.Split(line, "│")
		if len(cells) > 4 && strings.TrimSpace(cells[3]) == status {
			return true
		}
	}
	return false
}
