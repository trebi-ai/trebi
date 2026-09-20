package api_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/trebi-ai/trebi/ext"
	"github.com/trebi-ai/trebi/internal/api"
)

func TestInjectedUIAndEdition(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html>injected-spa</html>")},
	}
	s, base := startTestServerOpts(t, api.Options{
		Ext: ext.Set{
			Edition:        "pro",
			EditionVersion: "abc123",
			UI:             fsys,
			UIVersion:      "9.9.9",
		},
	})

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "injected-spa") {
		t.Fatalf("root: status=%d body=%q", resp.StatusCode, body)
	}

	code, health, _ := doJSON(t, "GET", base+"/v1/system/health", s.Token(), nil)
	if code != 200 {
		t.Fatalf("health %d %v", code, health)
	}
	if health["edition"] != "pro" || health["edition_version"] != "abc123" {
		t.Fatalf("edition fields: %v", health)
	}
	if health["ui_version"] != "9.9.9" {
		t.Fatalf("injected ui_version=%v", health["ui_version"])
	}
}

func TestInjectedUIVersionBeatsStamp(t *testing.T) {
	oldUI := api.UIVersion
	t.Cleanup(func() { api.UIVersion = oldUI })
	api.UIVersion = "0.1.0-stamp"
	fsys := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html>ok</html>")},
	}
	s, base := startTestServerOpts(t, api.Options{
		Ext: ext.Set{UI: fsys, UIVersion: "set-wins"},
	})
	code, health, _ := doJSON(t, "GET", base+"/v1/system/health", s.Token(), nil)
	if code != 200 || health["ui_version"] != "set-wins" {
		t.Fatalf("health %d %v", code, health)
	}
}

func TestSystemUpdateDetectsProAsset(t *testing.T) {
	old := api.Version
	t.Cleanup(func() { api.Version = old })
	api.Version = "0.2.9"
	s, base := startTestServerOpts(t, api.Options{
		LookupLatest: func() (string, string, error) {
			return "v0.3.0", "https://example.com/trebi_0.3.0_darwin_arm64.tar.gz", nil
		},
	})
	code, body, _ := doJSON(t, "GET", base+"/v1/system/update", s.Token(), nil)
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	if body["latest"] != "0.3.0" || body["update_available"] != true {
		t.Fatalf("update: %v", body)
	}
	if body["source"] != "github" {
		t.Fatalf("source=%v", body["source"])
	}
	if body["url"] != "https://example.com/trebi_0.3.0_darwin_arm64.tar.gz" {
		t.Fatalf("url=%v", body["url"])
	}
}
