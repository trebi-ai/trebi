package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/cloudlink"
	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
	"github.com/trebi-ai/trebi/internal/workspaces"
)

// TestCloudLoginReturnGone proves a cloud return with no open login
// answers gone, so the cloud page says the login is not open.
func TestCloudLoginReturnGone(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	st, err := store.OpenPath(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	login := cloudLogin{conn: connectors.New(st, workspaces.New(st), nil)}
	for _, state := range []string{"", "dev.unknown"} {
		got := login.ReturnLogin(context.Background(), cloudlink.LoginReturn{State: state, Code: "ac_1"})
		if got.Status != cloudlink.LoginGone {
			t.Errorf("state %q: result = %+v, want gone", state, got)
		}
	}
}
