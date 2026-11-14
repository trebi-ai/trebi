package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/hometest"
)

func TestMain(m *testing.M) { hometest.Main(m) }

const dmgBody = "fake dmg bytes"

// rig holds the fakes of every effect and one temp "Applications" folder.
// A bundle is a folder with a VERSION file.
type rig struct {
	t       *testing.T
	apps    string
	bundle  string
	newVer  string // VERSION that the fake dmg copy writes
	team    string
	running []int // RunningCount per Health call; the last value repeats
	startFn func(r *rig) error

	calls    []string
	upVer    string
	up       bool
	healthN  int
	dmgCode  int
	signErr  error
	mountErr error
}

// Doer
func (r *rig) Do(req *http.Request) (*http.Response, error) {
	r.log("get")
	return &http.Response{StatusCode: r.dmgCode, Status: fmt.Sprint(r.dmgCode), Body: io.NopCloser(strings.NewReader(dmgBody))}, nil
}

// Disk
func (r *rig) Attach(_ context.Context, dmg, mnt string) error {
	r.log("attach")
	if _, err := os.Stat(dmg); err != nil {
		r.t.Errorf("attach without dmg: %v", err)
	}
	return r.mountErr
}
func (r *rig) Detach(context.Context, string) error { r.log("detach"); return nil }
func (r *rig) Copy(_ context.Context, _, dst string) error {
	r.log("copy")
	r.writeBundle(dst, r.newVer)
	return nil
}

// Signer
func (r *rig) Verify(context.Context, string) (string, error) {
	r.log("verify")
	return r.team, r.signErr
}

// Daemon
func (r *rig) Health(context.Context) (Health, error) {
	if !r.up {
		return Health{}, errors.New("daemon is down")
	}
	n := r.running[min(r.healthN, len(r.running)-1)]
	r.healthN++
	return Health{Version: r.upVer, RunningCount: n}, nil
}
func (r *rig) Drain(context.Context) error   { r.log("drain"); return nil }
func (r *rig) Undrain(context.Context) error { r.log("undrain"); return nil }

// Agents
func (r *rig) Stop() error { r.log("stop"); r.up = false; return nil }
func (r *rig) StartDaemon() error {
	r.log("start")
	if r.startFn != nil {
		return r.startFn(r)
	}
	r.up, r.upVer = true, r.version(r.bundle)
	return nil
}
func (r *rig) RestartTray() error { r.log("tray"); return nil }

// Clock
func (r *rig) After(time.Duration) <-chan time.Time {
	r.log("wait")
	c := make(chan time.Time, 1)
	c <- time.Time{}
	return c
}

func (r *rig) callList() string { return strings.Join(r.calls, " ") }

func TestCheck(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		tag  string
		want bool
	}{{"v1.1.0", true}, {"v1.0.0", false}, {"v0.9.9", false}} {
		u := New("1.0.0", func(context.Context) (Release, error) { return Release{Tag: tc.tag}, nil })
		r, newer, err := u.Check(t.Context())
		if err != nil || newer != tc.want || r.Tag != tc.tag {
			t.Errorf("%s: release %v newer %v err %v", tc.tag, r, newer, err)
		}
	}
	boom := errors.New("boom")
	if _, _, err := New("1.0.0", func(context.Context) (Release, error) { return Release{}, boom }).Check(t.Context()); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	if _, newer, _ := New("dev", func(context.Context) (Release, error) { return Release{Tag: "v9.0.0"}, nil }).Check(t.Context()); newer {
		t.Error("dev build claims an update")
	}
}

func TestNewer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		latest, current string
		want            bool
	}{
		{"0.3.0", "0.2.9", true},
		{"0.2.9", "0.2.9", false},
		{"0.2.8", "0.2.9", false},
		{"v0.3.0", "0.2.9", true},
		{"0.3.0", "dev", false},
		{"0.3.0", "0.2.9-3-gabc", true},
		{"", "0.2.9", false},
	} {
		if got := Newer(tc.latest, tc.current); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tc.latest, tc.current, got, tc.want)
		}
	}
}

func TestEligible(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.updater(nil).Eligible(); err != nil {
		t.Fatalf("writable bundle: %v", err)
	}

	if err := New("1", nil).Eligible(); !errors.Is(err, ErrManual) {
		t.Errorf("no bundle: %v", err)
	}

	cask := filepath.Join(t.TempDir(), "Caskroom", "trebi", "1.0.0", "Trebi.app")
	r.writeBundle(cask, "1.0.0")
	if err := New("1", nil, WithBundle(cask)).Eligible(); !errors.Is(err, ErrManual) || !strings.Contains(err.Error(), "Homebrew") {
		t.Errorf("caskroom: %v", err)
	}

	link := filepath.Join(r.apps, "Link.app")
	if err := os.Symlink(r.bundle, link); err != nil {
		t.Fatal(err)
	}
	if err := New("1", nil, WithBundle(link)).Eligible(); !errors.Is(err, ErrManual) {
		t.Errorf("link: %v", err)
	}

	if os.Geteuid() == 0 {
		t.Skip("root can write everywhere")
	}
	ro := newRig(t)
	if err := os.Chmod(ro.apps, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro.apps, 0o755) })
	if err := ro.updater(nil).Eligible(); !errors.Is(err, ErrManual) || !strings.Contains(err.Error(), "not writable") {
		t.Errorf("read-only folder: %v", err)
	}
}

func TestStage(t *testing.T) {
	t.Parallel()
	t.Run("good", func(t *testing.T) {
		r := newRig(t)
		st, err := r.updater(nil).Stage(t.Context(), r.release("v2.0.0"))
		if err != nil {
			t.Fatal(err)
		}
		if st.Version != "2.0.0" || r.version(st.App) != "2.0.0" {
			t.Fatalf("staged %+v version %q", st, r.version(st.App))
		}
		if filepath.Dir(st.Dir) != r.apps {
			t.Errorf("stage folder %s is not next to the bundle", st.Dir)
		}
		if got := r.callList(); got != "get attach copy verify detach" {
			t.Errorf("calls: %s", got)
		}
		if r.version(r.bundle) != "1.0.0" {
			t.Error("Stage changed the installed bundle")
		}
	})
	t.Run("bad checksum", func(t *testing.T) {
		r := newRig(t)
		rel := r.release("v2.0.0")
		rel.SHA256 = sum("other")
		_, err := r.updater(nil).Stage(t.Context(), rel)
		if !errors.Is(err, ErrChecksum) {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(r.callList(), "attach") {
			t.Error("mounted a file with a bad checksum")
		}
	})
	t.Run("bad team", func(t *testing.T) {
		r := newRig(t)
		r.team = "EVILTEAM00"
		_, err := r.updater(nil).Stage(t.Context(), r.release("v2.0.0"))
		if err == nil || !strings.Contains(err.Error(), "EVILTEAM00") {
			t.Fatalf("err = %v", err)
		}
		r.noStageDir()
	})
	t.Run("bad signature", func(t *testing.T) {
		r := newRig(t)
		r.signErr = errors.New("a sealed resource is missing")
		if _, err := r.updater(nil).Stage(t.Context(), r.release("v2.0.0")); err == nil {
			t.Fatal("no error")
		}
		r.noStageDir()
		if !strings.HasSuffix(r.callList(), "detach") {
			t.Errorf("volume stays mounted: %s", r.callList())
		}
	})
	t.Run("download status", func(t *testing.T) {
		r := newRig(t)
		r.dmgCode = 404
		if _, err := r.updater(nil).Stage(t.Context(), r.release("v2.0.0")); err == nil {
			t.Fatal("no error")
		}
	})
	t.Run("mount error", func(t *testing.T) {
		r := newRig(t)
		r.mountErr = errors.New("hdiutil failed")
		if _, err := r.updater(nil).Stage(t.Context(), r.release("v2.0.0")); err == nil {
			t.Fatal("no error")
		}
		r.noStageDir()
	})
	t.Run("no checksum", func(t *testing.T) {
		r := newRig(t)
		rel := r.release("v2.0.0")
		rel.SHA256 = ""
		if _, err := r.updater(nil).Stage(t.Context(), rel); err == nil {
			t.Fatal("no error")
		}
	})
}

func (r *rig) noStageDir() {
	r.t.Helper()
	m, _ := filepath.Glob(filepath.Join(r.apps, ".trebi-update-*"))
	if len(m) != 0 {
		r.t.Errorf("stage folder left: %v", m)
	}
}

func (r *rig) stage(u *Updater) Staged {
	r.t.Helper()
	st, err := u.Stage(r.t.Context(), r.release("v2.0.0"))
	if err != nil {
		r.t.Fatal(err)
	}
	r.calls = nil
	return st
}

func TestApplyWhenIdle(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	u := r.updater(nil)
	st := r.stage(u)
	// Two runs, one run, then none; the recheck after the drain sees none.
	r.running = []int{2, 1, 0, 0}
	r.healthN = 0
	if err := u.ApplyWhenIdle(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	want := "wait wait drain stop start undrain tray"
	if got := r.callList(); got != want {
		t.Fatalf("calls:\n got %s\nwant %s", got, want)
	}
	if r.version(r.bundle) != "2.0.0" {
		t.Errorf("bundle version %q", r.version(r.bundle))
	}
	if _, err := os.Stat(r.bundle + ".old"); !os.IsNotExist(err) {
		t.Error(".old stays")
	}
	r.noStageDir()
}

func TestApplyWhenIdleRunStartsBeforeDrain(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	u := r.updater(nil)
	st := r.stage(u)
	// Idle, then a run starts before the drain; the next poll sees idle.
	r.running = []int{0, 1, 0, 0}
	r.healthN = 0
	if err := u.ApplyWhenIdle(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	want := "drain undrain wait drain stop start undrain tray"
	if got := r.callList(); got != want {
		t.Fatalf("calls:\n got %s\nwant %s", got, want)
	}
}

func TestApplyWhenIdleCanceledWhileBusy(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	u := r.updater(nil)
	st := r.stage(u)
	r.running = []int{3}
	r.healthN = 0
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := u.ApplyWhenIdle(ctx, st); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(r.callList(), "stop") || r.version(r.bundle) != "1.0.0" {
		t.Errorf("swapped while busy: %s", r.callList())
	}
}

func TestApplyRollbackOnVersionMismatch(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.newVer = "1.9.9" // the bundle is not the release version
	u := r.updater(nil)
	st := r.stage(u)
	err := u.ApplyWhenIdle(t.Context(), st)
	if err == nil || !strings.Contains(err.Error(), "want 2.0.0") {
		t.Fatalf("err = %v", err)
	}
	if r.version(r.bundle) != "1.0.0" {
		t.Errorf("bundle version %q after rollback", r.version(r.bundle))
	}
	if r.upVer != "1.0.0" || !r.up {
		t.Errorf("daemon: up %v version %q", r.up, r.upVer)
	}
	want := "drain stop start stop start undrain"
	if got := r.callList(); got != want {
		t.Fatalf("calls:\n got %s\nwant %s", got, want)
	}
	if _, err := os.Stat(r.bundle + ".old"); !os.IsNotExist(err) {
		t.Error(".old stays after rollback")
	}
	r.noStageDir()
}

func TestApplyRollbackWhenDaemonDoesNotAnswer(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	u := r.updater(nil)
	st := r.stage(u)
	first := true
	r.startFn = func(r *rig) error {
		if first { // the new daemon never comes up
			first = false
			return nil
		}
		r.up, r.upVer = true, r.version(r.bundle)
		return nil
	}
	if err := u.ApplyWhenIdle(t.Context(), st); err == nil || !strings.Contains(err.Error(), "does not answer") {
		t.Fatalf("err = %v", err)
	}
	if r.version(r.bundle) != "1.0.0" || !r.up {
		t.Errorf("bundle %q up %v", r.version(r.bundle), r.up)
	}
	if strings.Contains(r.callList(), "tray") {
		t.Error("restarted the tray after a failed update")
	}
}

func TestApplyRestoresOldBundleWhenMoveFails(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	u := r.updater(nil)
	st := r.stage(u)
	if err := os.RemoveAll(st.App); err != nil { // the second move fails
		t.Fatal(err)
	}
	err := u.ApplyWhenIdle(t.Context(), st)
	if err == nil || !strings.Contains(err.Error(), "move new app") {
		t.Fatalf("err = %v", err)
	}
	if r.version(r.bundle) != "1.0.0" || !r.up {
		t.Errorf("bundle %q up %v", r.version(r.bundle), r.up)
	}
	if !strings.HasSuffix(r.callList(), "start undrain") {
		t.Errorf("daemon is not resumed: %s", r.callList())
	}
}

func TestPrepare(t *testing.T) {
	t.Parallel()
	t.Run("none", func(t *testing.T) {
		r := newRig(t)
		p, err := r.updater(func(context.Context) (Release, error) { return r.release("v1.0.0"), nil }).Prepare(t.Context())
		if err != nil || p.Kind != PlanNone {
			t.Fatalf("plan %+v err %v", p, err)
		}
	})
	t.Run("ready then busy", func(t *testing.T) {
		r := newRig(t)
		u := r.updater(nil)
		p, err := u.Prepare(t.Context())
		if err != nil || p.Kind != PlanReady || p.Staged.Version != "2.0.0" {
			t.Fatalf("plan %+v err %v", p, err)
		}
		if _, err := u.Prepare(t.Context()); !errors.Is(err, ErrBusy) {
			t.Fatalf("second Prepare: %v", err)
		}
		if err := u.ApplyWhenIdle(t.Context(), p.Staged); err != nil {
			t.Fatal(err)
		}
		if _, err := u.Prepare(t.Context()); errors.Is(err, ErrBusy) {
			t.Fatal("still busy after the update")
		}
	})
	t.Run("no dmg", func(t *testing.T) {
		r := newRig(t)
		u := r.updater(func(context.Context) (Release, error) {
			rel := r.release("v2.0.0")
			rel.DMGURL = ""
			return rel, nil
		})
		p, err := u.Prepare(t.Context())
		if !errors.Is(err, ErrCheck) || p.Kind != PlanNone {
			t.Fatalf("plan %+v err %v", p, err)
		}
		if _, err := u.Prepare(t.Context()); errors.Is(err, ErrBusy) {
			t.Error("busy after a failed check")
		}
	})
	t.Run("check error", func(t *testing.T) {
		r := newRig(t)
		cause := errors.New("status 404")
		_, err := r.updater(func(context.Context) (Release, error) { return Release{}, cause }).Prepare(t.Context())
		if !errors.Is(err, ErrCheck) || !errors.Is(err, cause) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("manual", func(t *testing.T) {
		r := newRig(t)
		r.bundle = filepath.Join(r.apps, "Caskroom", "Trebi.app")
		r.writeBundle(r.bundle, "1.0.0")
		p, err := r.updater(nil).Prepare(t.Context())
		if err != nil || p.Kind != PlanManual || !errors.Is(p.Reason, ErrManual) || p.Release.DMGURL == "" {
			t.Fatalf("plan %+v err %v", p, err)
		}
		if strings.Contains(r.callList(), "get") {
			t.Error("downloaded for a manual update")
		}
	})
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, apps: t.TempDir(), newVer: "2.0.0", team: TeamID, running: []int{0}, up: true, dmgCode: 200}
	r.bundle = filepath.Join(r.apps, "Trebi.app")
	r.writeBundle(r.bundle, "1.0.0")
	r.upVer = "1.0.0"
	return r
}

func (r *rig) writeBundle(dir, ver string) {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "Contents"), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte(ver), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) version(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "VERSION"))
	if err != nil {
		return ""
	}
	return string(b)
}

func (r *rig) updater(fetch func(context.Context) (Release, error)) *Updater {
	if fetch == nil {
		fetch = func(context.Context) (Release, error) { return r.release("v2.0.0"), nil }
	}
	return New("1.0.0", fetch,
		WithBundle(r.bundle), WithWorkDir(r.t.TempDir()), WithHTTP(r), WithDisk(r), WithSigner(r),
		WithDaemon(r), WithAgents(r), WithClock(r), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
}

func (r *rig) release(tag string) Release {
	return Release{Tag: tag, DMGURL: "https://example.test/Trebi.dmg", SHA256: sum(dmgBody)}
}

func (r *rig) log(s string) { r.calls = append(r.calls, s) }
