package harness

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/store"
)

// fakeVersions is a program file whose content is its version.
type fakeVersions struct {
	vendor string
	latest string
	next   string // the version that Update writes
	fail   error  // Update error
}

func (f *fakeVersions) Source(name string) (VersionSource, bool) { return f, name == "claude" }
func (f *fakeVersions) VendorBin(string) string                  { return f.vendor }

func (f *fakeVersions) Installed(_ context.Context, bin string) (string, error) {
	b, err := os.ReadFile(bin)
	return strings.TrimSpace(string(b)), err
}
func (f *fakeVersions) Latest(context.Context) (string, error) { return f.latest, nil }
func (f *fakeVersions) Update(_ context.Context, bin string) error {
	if f.fail != nil {
		return f.fail
	}
	return os.WriteFile(bin, []byte(f.next), 0o755)
}
func (f *fakeVersions) AutoUpdateOff() map[string]string {
	return map[string]string{"X_AUTOUPDATE": "0"}
}

type fakeRuns struct {
	mu     sync.Mutex
	active bool
}

func (r *fakeRuns) HarnessActive(context.Context, string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

func (r *fakeRuns) set(v bool) {
	r.mu.Lock()
	r.active = v
	r.mu.Unlock()
}

// fakeAuth was logged in; okAfter decides the probe after an update.
type fakeAuth struct{ okAfter bool }

func (a fakeAuth) GetState(context.Context, string) (State, error) {
	return State{AuthOK: sql.NullInt64{Int64: 1, Valid: true}}, nil
}

func (a fakeAuth) Probe(_ context.Context, name, _ string) (AuthStatus, error) {
	if a.okAfter {
		return AuthStatus{Tool: name, Status: AuthOK}, nil
	}
	return AuthStatus{Tool: name, Status: AuthMissing, Detail: "401"}, nil
}

func versionEnv(t *testing.T, mode string) (*store.Store, *fakeVersions) {
	t.Helper()
	dir := t.TempDir()
	home.SetBase(dir)
	t.Setenv("TREBI_HOME", dir)
	t.Cleanup(func() { home.SetBase("") })
	cfg := "tools:\n  claude:\n    enabled: true\n"
	if mode != "" {
		cfg += "    updates: " + mode + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	vendor := filepath.Join(dir, "vendor", "claude")
	if err := os.MkdirAll(filepath.Dir(vendor), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vendor, []byte("1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenPath(filepath.Join(dir, "trebi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, &fakeVersions{vendor: vendor, latest: "1.1.0", next: "1.1.0"}
}

func readProgram(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func currentProgram() string { return filepath.Join(config.BinDir(), "claude", "current", "claude") }

func TestAutoUpdateWaitsForRun(t *testing.T) {
	st, src := versionEnv(t, "")
	runs := &fakeRuns{active: true}
	m := NewVersionManager(st, src, runs, fakeAuth{okAfter: true})
	ctx := context.Background()

	m.Tick(ctx)
	if v := readProgram(t, src.vendor); v != "1.0.0" {
		t.Fatalf("updated during a run: %s", v)
	}
	vs, _ := m.State(ctx, "claude")
	if vs.Version != "1.0.0" || vs.Latest != "1.1.0" || !vs.UpdateAvailable() {
		t.Fatalf("state %+v", vs)
	}

	runs.set(false)
	m.Tick(ctx)
	if v := readProgram(t, src.vendor); v != "1.1.0" {
		t.Fatalf("no update after the run: %s", v)
	}
	vs, _ = m.State(ctx, "claude")
	if vs.Version != "1.1.0" || vs.LastUpdate == nil || !vs.LastUpdate.OK || vs.LastUpdate.From != "1.0.0" {
		t.Fatalf("state %+v last %+v", vs, vs.LastUpdate)
	}
	if v := readProgram(t, filepath.Join(config.BinDir(), "claude", "1.0.0", "claude")); v != "1.0.0" {
		t.Fatalf("last good copy %q", v)
	}
	if _, err := os.Lstat(filepath.Join(config.BinDir(), "claude", "current")); !os.IsNotExist(err) {
		t.Fatalf("auto mode keeps no current link: %v", err)
	}
}

func TestFailedCheckGoesBackToLastGood(t *testing.T) {
	st, src := versionEnv(t, "")
	m := NewVersionManager(st, src, &fakeRuns{}, fakeAuth{okAfter: false})
	ctx := context.Background()

	if _, err := m.Check(ctx, "claude"); err != nil {
		t.Fatal(err)
	}
	if err := m.RequestUpdate(ctx, "claude"); err == nil {
		t.Fatal("want the check error")
	}
	if v := readProgram(t, currentProgram()); v != "1.0.0" {
		t.Fatalf("current %q want the last good copy", v)
	}
	vs, _ := m.State(ctx, "claude")
	if vs.Version != "1.0.0" || vs.LastUpdate == nil || vs.LastUpdate.OK || vs.LastUpdate.To != "1.1.0" {
		t.Fatalf("state %+v last %+v", vs, vs.LastUpdate)
	}
	if !failedBefore(vs) {
		t.Fatal("auto must not try the same version again")
	}
	evs, err := st.ListBus(ctx, 0, []string{"harness.updated"}, 10)
	if err != nil || len(evs) == 0 {
		t.Fatalf("no harness.updated: %v", err)
	}
	var a map[string]any
	if err := json.Unmarshal(evs[len(evs)-1].Attrs, &a); err != nil || a["update_ok"] != "false" || a["version"] != "1.0.0" {
		t.Fatalf("attrs %s", evs[len(evs)-1].Attrs)
	}

	src.fail = errors.New("network down")
	src.latest = "1.2.0"
	if err := m.RequestUpdate(ctx, "claude"); err == nil {
		t.Fatal("want the update error")
	}
	vs, _ = m.State(ctx, "claude")
	if vs.LastUpdate.Error != "network down" {
		t.Fatalf("last %+v", vs.LastUpdate)
	}
}

func TestKeepAndAuto(t *testing.T) {
	st, src := versionEnv(t, "keep")
	m := NewVersionManager(st, src, &fakeRuns{}, fakeAuth{okAfter: true})
	ctx := context.Background()

	if err := m.Keep(ctx, "claude", ""); err != nil {
		t.Fatal(err)
	}
	if v := readProgram(t, currentProgram()); v != "1.0.0" {
		t.Fatalf("kept %q", v)
	}
	if ver, _, ok := KeptSince("claude"); !ok || ver != "1.0.0" {
		t.Fatalf("KeptSince %q %v", ver, ok)
	}
	cfg, _ := config.Load()
	if p := cfg.Tools.Pin("claude"); p == nil || p.Version != "1.0.0" {
		t.Fatalf("pin %+v", p)
	}
	if err := m.Keep(ctx, "claude", "0.9.0"); !errors.Is(err, ErrNoVersionInstall) {
		t.Fatalf("named version: %v", err)
	}

	// Update to the latest in keep mode keeps the new version.
	if err := m.RequestUpdate(ctx, "claude"); err != nil {
		t.Fatal(err)
	}
	if v := readProgram(t, currentProgram()); v != "1.1.0" {
		t.Fatalf("kept after update %q", v)
	}

	if err := m.Auto(ctx, "claude"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(config.BinDir(), "claude", "current")); !os.IsNotExist(err) {
		t.Fatalf("auto keeps current: %v", err)
	}
}

func TestOutsideVersionChangeRecorded(t *testing.T) {
	st, src := versionEnv(t, "keep")
	m := NewVersionManager(st, src, &fakeRuns{}, fakeAuth{okAfter: true})
	ctx := context.Background()

	m.Tick(ctx)
	if err := os.WriteFile(src.vendor, []byte("1.2.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	m.Tick(ctx)
	vs, _ := m.State(ctx, "claude")
	if vs.Version != "1.2.0" || vs.LastUpdate == nil || vs.LastUpdate.From != "1.0.0" || vs.LastUpdate.To != "1.2.0" || !vs.LastUpdate.OK {
		t.Fatalf("state %+v last %+v", vs, vs.LastUpdate)
	}
}

func TestUpdatingGate(t *testing.T) {
	st, src := versionEnv(t, "")
	m := NewVersionManager(st, src, &fakeRuns{}, nil)
	if m.Updating("claude") {
		t.Fatal("no update runs")
	}
	m.begin("claude")
	if !m.Updating("claude") {
		t.Fatal("the gate must hold new runs")
	}
	m.updating["claude"] = m.now().Add(-UpdateGateMax)
	if m.Updating("claude") {
		t.Fatal("the gate opens after UpdateGateMax")
	}
	var nilManager *VersionManager
	if nilManager.Updating("claude") {
		t.Fatal("nil manager")
	}
}

func TestNewerVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.0.10", "1.0.9", true},
		{"1.0.9", "1.0.10", false},
		{"2.0.0", "2.0.0", false},
		{"1.0.0", "1.0.0-beta.1", true},
		{"1.0.0-beta.1", "1.0.0", false},
		{"", "1.0.0", false},
	}
	for _, c := range cases {
		if got := NewerVersion(c.a, c.b); got != c.want {
			t.Errorf("NewerVersion(%q, %q) = %v", c.a, c.b, got)
		}
	}
	if v := ParseVersion("grok 1.0.40 (eb1a2256660d) [stable]"); v != "1.0.40" {
		t.Errorf("ParseVersion %q", v)
	}
	if v := ParseVersion("rust-v0.162.0"); v != "0.162.0" {
		t.Errorf("ParseVersion tag %q", v)
	}
}
