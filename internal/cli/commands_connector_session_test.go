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

	"github.com/trebi-ai/trebi/internal/connectors/protocol"
	"github.com/trebi-ai/trebi/internal/connectors/protocol/fakeadapter"
)

// TestMain lets the test binary run as a fake adapter for the local
// conformance check.
func TestMain(m *testing.M) {
	if fakeadapter.Enabled() {
		os.Exit(fakeadapter.Main())
	}
	os.Exit(m.Run())
}

func TestConnectorConformanceLocal(t *testing.T) {
	dir := t.TempDir()
	sb := &fakeadapter.Sandbox{
		Result:   protocol.InitializeResult{Features: []string{"rooms.list"}, Events: []protocol.EventDecl{{Type: "message"}}},
		Rooms:    []protocol.Room{{ID: "r1"}},
		LoggedIn: true,
	}
	path := filepath.Join(dir, "sandbox.json")
	if err := sb.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeadapter.Env, "sandbox:"+path)
	manifestYAML := `schema: trebi-connector/1
name: chat
title: Chat
description: A test chat.
version: 1.0.0
events:
  protocol: trebi-connector/1
  command: '"` + os.Args[0] + `"'
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
	for _, want := range []string{"initialize", "send once", "shutdown"} {
		if !strings.Contains(out, want) {
			t.Fatalf("no %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "fail") {
		t.Fatalf("a check failed:\n%s", out)
	}

	// A promise the adapter does not keep fails the command.
	if _, err := runCLI(t, nil, "connector", "conformance", "--command", `"`+os.Args[0]+`"`, "--json"); err != nil {
		t.Fatalf("command only: %v", err)
	}
	manifestYAML = strings.Replace(manifestYAML, "[rooms.list]", "[rooms.list, reactions]", 1)
	if err := os.WriteFile(filepath.Join(dir, "trebi-connector.yaml"), []byte(manifestYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, nil, "connector", "conformance", "--manifest", dir); err == nil {
		t.Fatal("a missing feature must fail")
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
