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
		URL: "https://homelab:7433", Token: "tok",
	}
	if err := SaveLinkedWorkspace(link); err != nil {
		t.Fatal(err)
	}
	got, ok := FindLinked("ellis")
	if !ok || got.ID != "ws1" || got.Token != "tok" {
		t.Fatalf("%+v %v", got, ok)
	}
	if err := SaveStickyContext(StickyFromLinked(link)); err != nil {
		t.Fatal(err)
	}
	st, err := LoadStickyContext()
	if err != nil || st == nil || !st.IsRemote() || st.WorkspaceID != "ws1" {
		t.Fatalf("%+v %v", st, err)
	}

	// LoadAPIClient should target remote when sticky is remote.
	c, err := LoadAPIClient()
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsRemote() || c.Base != "https://homelab:7433" || c.Token != "tok" {
		t.Fatalf("%+v", c)
	}

	// -w name resolves to linked remote.
	t.Setenv("TREBI_WORKSPACE", "ellis")
	c2, err := LoadAPIClient()
	if err != nil {
		t.Fatal(err)
	}
	if !c2.IsRemote() || c2.Base != "https://homelab:7433" {
		t.Fatalf("%+v", c2)
	}

	if got := EffectiveWorkspace(nil); got != "ws1" {
		t.Fatalf("EffectiveWorkspace want ws1 got %s", got)
	}

	if err := RemoveLinkedWorkspace("ws1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := FindLinked("ws1"); ok {
		t.Fatal("expected removed")
	}
}
