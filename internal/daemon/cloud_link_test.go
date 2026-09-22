package daemon_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

// TestControlPlane_CloudLinkUsesBootConfig proves the daemon hands the
// resolved cloud block to the link service when it builds the control
// plane. Without that, Link answers "cloud api url is not configured"
// until some later link-state change reconfigures the service.
func TestControlPlane_CloudLinkUsesBootConfig(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })

	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"device_id":"01m3a1b2c3d4e5f6g7h8j9k0m1"}`))
	}))
	defer srv.Close()

	cfg := "cloud:\n  api_url: " + srv.URL + "\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cp := daemon.New(st, daemon.Options{})
	if _, err := cp.Cloud.Link(context.Background(), "01m3code", "laptop"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	if path != "/v1/devices" {
		t.Fatalf("register path = %q; want /v1/devices", path)
	}
}
