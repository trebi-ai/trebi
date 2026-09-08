package api_test

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAssistantGetDisabled(t *testing.T) {
	s, base := startTestServer(t)
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, "TREBI.md"),
		[]byte("---\nharness: fake\ndisabled: true\n---\n\nHi.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsID := addWorkspace(t, base, s.Token(), wsDir, "asst-ws")
	code, body, _ := doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/assistant", s.Token(), nil)
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	if body["disabled"] != true {
		t.Fatalf("disabled=%v want true in %v", body["disabled"], body)
	}

	if err := os.WriteFile(filepath.Join(wsDir, "TREBI.md"),
		[]byte("---\nharness: fake\n---\n\nHi.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body, _ = doJSON(t, "GET", base+"/v1/workspaces/"+wsID+"/assistant", s.Token(), nil)
	if code != 200 {
		t.Fatalf("enabled %d %v", code, body)
	}
	if body["disabled"] != false {
		t.Fatalf("disabled=%v want false in %v", body["disabled"], body)
	}
}
