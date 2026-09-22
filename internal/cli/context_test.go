package cli

import (
	"testing"

	"github.com/trebi-ai/trebi/internal/home"
)

func TestStickyAndLinkedWorkspace(t *testing.T) {
	dir := t.TempDir()
	home.SetBase(dir)
	t.Cleanup(func() {
		home.SetBase("")
	})

	link := LinkedWorkspace{
		ID: "ws1", Name: "ellis", Path: "/proj",
		URL: "https://homelab:7433",
	}
	if err := link.Save(); err != nil {
		t.Fatal(err)
	}
	got, ok := FindLinked("ellis")
	if !ok || got.ID != "ws1" {
		t.Fatalf("%+v %v", got, ok)
	}
	sticky := link.Sticky()
	if err := sticky.Save(); err != nil {
		t.Fatal(err)
	}
	st, err := LoadStickyContext()
	if err != nil || st == nil || !st.IsRemote() || st.WorkspaceID != "ws1" {
		t.Fatalf("%+v %v", st, err)
	}

	if err := RemoveLinkedWorkspace("ws1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := FindLinked("ws1"); ok {
		t.Fatal("expected removed")
	}
}
