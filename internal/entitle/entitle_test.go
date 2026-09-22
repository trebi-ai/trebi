package entitle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFeatureNamesMatchContract pins the four Feature values to the
// vendored contract's features array, set-equal with no extras. A rename
// on either side fails here.
func TestFeatureNamesMatchContract(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "cloudlink", "testdata", "contract", "entitlement.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Features []string `json:"features"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}

	local := []Feature{Watchdog, RemoteAccess, HostedIngress, Backup}
	if len(local) != len(fixture.Features) {
		t.Fatalf("contract has %d features, entitle declares %d", len(fixture.Features), len(local))
	}
	contract := map[string]bool{}
	for _, f := range fixture.Features {
		contract[f] = true
	}
	for _, f := range local {
		if !contract[string(f)] {
			t.Errorf("Feature %q missing from the contract", f)
		}
		delete(contract, string(f))
	}
	for f := range contract {
		t.Errorf("contract feature %q has no entitle.Feature", f)
	}
}

func TestStateHas(t *testing.T) {
	t.Parallel()
	s := State{
		Linked:      true,
		Plan:        "pro_cloud",
		Features:    []Feature{Watchdog, HostedIngress},
		RefreshedAt: time.Now(),
	}
	if !s.Has(Watchdog) || !s.Has(HostedIngress) {
		t.Errorf("granted features not found: %v", s.Features)
	}
	if s.Has(Backup) || s.Has(RemoteAccess) {
		t.Errorf("ungranted features reported granted: %v", s.Features)
	}
	if (State{}).Has(Watchdog) {
		t.Error("zero state granted watchdog")
	}
}
