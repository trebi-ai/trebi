package daemon_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/trebi-ai/trebi/internal/alerts"
	"github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/daemon"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

// TestCleanInstallInboxIsEmpty lists what a clean install must not put in the
// Inbox: a new user sees no finding before they set anything up.
func TestCleanInstallInboxIsEmpty(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	if _, err := config.Seed(); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cp := daemon.New(st, daemon.Options{})
	ctx := context.Background()
	cp.Alerts.SyncDoctor(ctx, cp.Alerts.CollectDoctorFindings())

	page, err := cp.Alerts.List(ctx, alerts.ListOpts{InboxFilter: alerts.InboxFilter{Kind: alerts.InboxKind("health")}, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range page.Items {
		t.Errorf("clean install has inbox item %+v", it)
	}
}
