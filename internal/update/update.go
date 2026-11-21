// Package update replaces the installed Trebi.app with the bundle of a newer
// release. It downloads the release .dmg, checks it, and swaps the bundle
// when no run is active. Every effect sits behind a small interface.
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
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// TeamID is the Developer ID team that signs the release.
const TeamID = "Q56WK6TB88"

const appName = "Trebi.app"

// ErrManual means that Trebi cannot replace this copy. The user downloads
// the new version.
var ErrManual = errors.New("update needs a manual download")

// ErrChecksum means that the downloaded file differs from SHA256SUMS.
var ErrChecksum = errors.New("checksum does not match")

// ErrCheck means that Prepare cannot find a usable release.
var ErrCheck = errors.New("update check failed")

// ErrBusy means that an update is already in progress.
var ErrBusy = errors.New("an update is already in progress")

// Release is one published version.
type Release struct {
	Tag    string
	DMGURL string
	SHA256 string // hex digest of the .dmg, from SHA256SUMS
}

// Version is the tag without the leading "v".
func (r Release) Version() string { return strings.TrimPrefix(r.Tag, "v") }

// Staged is a verified copy of the new bundle, next to the installed one.
type Staged struct {
	App     string
	Dir     string
	Version string
}

// Doer sends an HTTP request. *http.Client satisfies it.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Disk mounts the .dmg and copies bundles.
type Disk interface {
	Attach(ctx context.Context, dmg, mountpoint string) error
	Detach(ctx context.Context, mountpoint string) error
	Copy(ctx context.Context, src, dst string) error
}

// Signer checks the code signature of a bundle and returns its team ID.
type Signer interface {
	Verify(ctx context.Context, app string) (teamID string, err error)
}

// Health is what the update needs from the daemon health.
type Health struct {
	Version      string
	RunningCount int
}

// Daemon reads the health of the daemon and controls its drain mode.
type Daemon interface {
	Health(ctx context.Context) (Health, error)
	Drain(ctx context.Context) error
	Undrain(ctx context.Context) error
}

// Agents controls the LaunchAgents. RestartTray ends the calling process
// when the tray calls it, so it is the last step of an update.
type Agents interface {
	Stop() error
	StartDaemon() error
	RestartTray() error
}

// Clock waits.
type Clock interface {
	After(d time.Duration) <-chan time.Time
}

type systemClock struct{}

func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Updater checks for a release and installs it.
type Updater struct {
	current string
	fetch   func(context.Context) (Release, error)

	bundle  string
	teamID  string
	workDir string
	http    Doer
	disk    Disk
	signer  Signer
	daemon  Daemon
	agents  Agents
	clock   Clock
	log     *slog.Logger

	idleEvery   time.Duration
	verifyEvery time.Duration
	verifyTries int

	busy atomic.Bool
}

// Option sets one dependency or value of an Updater.
type Option func(*Updater)

// WithBundle sets the path of the installed .app.
func WithBundle(path string) Option { return func(u *Updater) { u.bundle = path } }

// WithWorkDir sets the folder for the download and the mount point.
func WithWorkDir(dir string) Option { return func(u *Updater) { u.workDir = dir } }

// WithHTTP sets the client that downloads the .dmg.
func WithHTTP(d Doer) Option { return func(u *Updater) { u.http = d } }

// WithDisk sets the .dmg mounter.
func WithDisk(d Disk) Option { return func(u *Updater) { u.disk = d } }

// WithSigner sets the signature check.
func WithSigner(s Signer) Option { return func(u *Updater) { u.signer = s } }

// WithDaemon sets the health and drain calls.
func WithDaemon(d Daemon) Option { return func(u *Updater) { u.daemon = d } }

// WithAgents sets the LaunchAgent control.
func WithAgents(a Agents) Option { return func(u *Updater) { u.agents = a } }

// WithClock sets the clock.
func WithClock(c Clock) Option { return func(u *Updater) { u.clock = c } }

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(u *Updater) { u.log = l } }

// New builds an Updater. current is the running version. fetch returns the
// newest release. Use WithBundle, WithDaemon, and WithAgents to allow an
// install.
func New(current string, fetch func(context.Context) (Release, error), opts ...Option) *Updater {
	u := &Updater{
		current:     current,
		fetch:       fetch,
		teamID:      TeamID,
		workDir:     os.TempDir(),
		http:        &http.Client{},
		disk:        SystemDisk{},
		signer:      SystemSigner{},
		clock:       systemClock{},
		log:         slog.Default().With("source", "update"),
		idleEvery:   30 * time.Second,
		verifyEvery: time.Second,
		verifyTries: 90,
	}
	for _, o := range opts {
		o(u)
	}
	return u
}

// Check fetches the newest release. The bool is true when it is newer than
// the running version.
func (u *Updater) Check(ctx context.Context) (Release, bool, error) {
	r, err := u.fetch(ctx)
	if err != nil {
		return Release{}, false, err
	}
	return r, Newer(r.Tag, u.current), nil
}

// Eligible returns an error that wraps ErrManual when Trebi cannot replace
// the installed bundle without an administrator: no bundle, a Homebrew
// Caskroom path, a symbolic link, or a folder that the user cannot write.
func (u *Updater) Eligible() error {
	if u.bundle == "" {
		return fmt.Errorf("%w: Trebi does not run from an app", ErrManual)
	}
	if info, err := os.Lstat(u.bundle); err != nil || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a link", ErrManual, u.bundle)
	}
	resolved, err := filepath.EvalSymlinks(u.bundle)
	if err != nil {
		resolved = u.bundle
	}
	for _, p := range []string{u.bundle, resolved} {
		if strings.Contains(filepath.ToSlash(p), "/Caskroom/") {
			return fmt.Errorf("%w: Homebrew owns %s", ErrManual, p)
		}
	}
	for _, dir := range []string{filepath.Dir(u.bundle), u.bundle, filepath.Join(u.bundle, "Contents")} {
		if !writable(dir) {
			return fmt.Errorf("%w: %s is not writable", ErrManual, dir)
		}
	}
	return nil
}

// PlanKind says what to do about a release.
type PlanKind int

const (
	// PlanNone: the running version is the newest.
	PlanNone PlanKind = iota
	// PlanManual: a newer release exists, but the user must download it.
	PlanManual
	// PlanReady: Staged holds the new bundle. Call ApplyWhenIdle.
	PlanReady
)

// Plan is the result of Prepare.
type Plan struct {
	Kind    PlanKind
	Release Release
	Staged  Staged
	Reason  error // for PlanManual
}

// Prepare checks for a release and stages it when this copy can update
// itself. A PlanReady plan holds the updater until ApplyWhenIdle returns.
func (u *Updater) Prepare(ctx context.Context) (Plan, error) {
	if !u.busy.CompareAndSwap(false, true) {
		return Plan{}, ErrBusy
	}
	plan, err := u.prepare(ctx)
	if err != nil || plan.Kind != PlanReady {
		u.busy.Store(false)
	}
	return plan, err
}

// Stage downloads the .dmg, checks its digest, copies the app out of it into
// a hidden folder next to the installed bundle, and checks the signature.
func (u *Updater) Stage(ctx context.Context, r Release) (Staged, error) {
	if err := u.Eligible(); err != nil {
		return Staged{}, err
	}
	if r.DMGURL == "" || r.SHA256 == "" {
		return Staged{}, fmt.Errorf("release %s has no .dmg or no checksum", r.Tag)
	}
	work, err := os.MkdirTemp(u.workDir, "trebi-update-")
	if err != nil {
		return Staged{}, err
	}
	defer os.RemoveAll(work)
	dmg := filepath.Join(work, "Trebi.dmg")
	if err := u.download(ctx, r.DMGURL, dmg, r.SHA256); err != nil {
		return Staged{}, err
	}
	mnt := filepath.Join(work, "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		return Staged{}, err
	}
	if err := u.disk.Attach(ctx, dmg, mnt); err != nil {
		return Staged{}, fmt.Errorf("mount: %w", err)
	}
	defer func() {
		if err := u.disk.Detach(context.WithoutCancel(ctx), mnt); err != nil {
			u.log.Warn("detach failed", "error", err)
		}
	}()

	parent := filepath.Dir(u.bundle)
	u.removeStale(parent)
	dir, err := os.MkdirTemp(parent, ".trebi-update-")
	if err != nil {
		return Staged{}, err
	}
	app := filepath.Join(dir, filepath.Base(u.bundle))
	if err := u.disk.Copy(ctx, filepath.Join(mnt, appName), app); err != nil {
		_ = os.RemoveAll(dir)
		return Staged{}, fmt.Errorf("copy app: %w", err)
	}
	team, err := u.signer.Verify(ctx, app)
	if err == nil && team != u.teamID {
		err = fmt.Errorf("team %q, want %q", team, u.teamID)
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return Staged{}, fmt.Errorf("signature: %w", err)
	}
	return Staged{App: app, Dir: dir, Version: r.Version()}, nil
}

// ApplyWhenIdle waits until no run is active, pauses new runs, replaces the
// bundle, restarts the agents, and checks the version. It restores the old
// bundle when a step fails. It ends the tray last.
func (u *Updater) ApplyWhenIdle(ctx context.Context, st Staged) error {
	defer u.busy.Store(false)
	defer os.RemoveAll(st.Dir)
	if err := u.drainWhenIdle(ctx); err != nil {
		return err
	}
	// A cancel must not stop a swap half way.
	return u.swap(context.WithoutCancel(ctx), st)
}

// drainWhenIdle polls until no run is active, then drains. A run can start
// between the poll and the drain, so it counts again after the drain.
func (u *Updater) drainWhenIdle(ctx context.Context) error {
	for {
		if h, err := u.daemon.Health(ctx); err == nil && h.RunningCount == 0 {
			if err := u.daemon.Drain(ctx); err != nil {
				return fmt.Errorf("drain: %w", err)
			}
			if h, err := u.daemon.Health(ctx); err == nil && h.RunningCount == 0 {
				return nil
			}
			if err := u.daemon.Undrain(ctx); err != nil {
				return fmt.Errorf("undrain: %w", err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-u.clock.After(u.idleEvery):
		}
	}
}

func (u *Updater) swap(ctx context.Context, st Staged) error {
	old := u.bundle + ".old"
	_ = os.RemoveAll(old)
	if err := u.agents.Stop(); err != nil {
		return u.resumeAfter(ctx, fmt.Errorf("stop daemon: %w", err))
	}
	if err := os.Rename(u.bundle, old); err != nil {
		return u.resumeAfter(ctx, fmt.Errorf("move old app: %w", err))
	}
	if err := os.Rename(st.App, u.bundle); err != nil {
		cause := fmt.Errorf("move new app: %w", err)
		if rerr := os.Rename(old, u.bundle); rerr != nil {
			cause = errors.Join(cause, fmt.Errorf("restore old app: %w", rerr))
		}
		return u.resumeAfter(ctx, cause)
	}
	if err := u.agents.StartDaemon(); err != nil {
		return u.rollback(ctx, old, fmt.Errorf("start daemon: %w", err))
	}
	if err := u.waitVersion(ctx, st.Version); err != nil {
		return u.rollback(ctx, old, err)
	}
	var errs []error
	if err := u.daemon.Undrain(ctx); err != nil {
		errs = append(errs, fmt.Errorf("undrain: %w", err))
	}
	if err := os.RemoveAll(old); err != nil {
		errs = append(errs, fmt.Errorf("remove old app: %w", err))
	}
	if err := u.agents.RestartTray(); err != nil {
		errs = append(errs, fmt.Errorf("restart tray: %w", err))
	}
	return errors.Join(errs...)
}

// rollback puts the old bundle back and starts the old daemon.
func (u *Updater) rollback(ctx context.Context, old string, cause error) error {
	if err := u.agents.Stop(); err != nil {
		cause = errors.Join(cause, fmt.Errorf("stop daemon: %w", err))
	}
	if err := os.RemoveAll(u.bundle); err != nil {
		return errors.Join(cause, fmt.Errorf("remove new app: %w", err))
	}
	if err := os.Rename(old, u.bundle); err != nil {
		return errors.Join(cause, fmt.Errorf("restore old app: %w", err))
	}
	return u.resumeAfter(ctx, cause)
}

// resumeAfter starts the daemon, leaves drain mode, and returns cause.
func (u *Updater) resumeAfter(ctx context.Context, cause error) error {
	if err := u.agents.StartDaemon(); err != nil {
		return errors.Join(cause, fmt.Errorf("start daemon: %w", err))
	}
	if err := u.waitVersion(ctx, ""); err != nil {
		return errors.Join(cause, err)
	}
	if err := u.daemon.Undrain(ctx); err != nil {
		return errors.Join(cause, fmt.Errorf("undrain: %w", err))
	}
	return cause
}

// waitVersion polls the health until the daemon answers. A want other than
// "" must match the version at once.
func (u *Updater) waitVersion(ctx context.Context, want string) error {
	var last error
	for range u.verifyTries {
		h, err := u.daemon.Health(ctx)
		switch {
		case err != nil:
			last = err
		case want != "" && strings.TrimPrefix(h.Version, "v") != want:
			return fmt.Errorf("version after restart is %s, want %s", h.Version, want)
		default:
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-u.clock.After(u.verifyEvery):
		}
	}
	return fmt.Errorf("daemon does not answer after restart: %w", last)
}

// IsDMGAsset reports whether name is the release .dmg.
func IsDMGAsset(name string) bool {
	return strings.HasPrefix(name, "Trebi-") && strings.HasSuffix(name, "-universal.dmg")
}

// Newer reports whether latest's major.minor.patch triple is greater than
// current's. A current that is not a version (dev) is never older.
func Newer(latest, current string) bool {
	lt, ok := parseTriple(latest)
	if !ok {
		return false
	}
	ct, ok := parseTriple(current)
	if !ok {
		return false
	}
	for i := range lt {
		if lt[i] != ct[i] {
			return lt[i] > ct[i]
		}
	}
	return false
}

func parseTriple(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	if v == "" || v == "dev" {
		return out, false
	}
	core := v
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core = core[:i]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func writable(dir string) bool {
	f, err := os.CreateTemp(dir, ".trebi-write-*")
	if err != nil {
		return false
	}
	_ = f.Close()
	return os.Remove(f.Name()) == nil
}

func (u *Updater) prepare(ctx context.Context) (Plan, error) {
	r, newer, err := u.Check(ctx)
	if err != nil {
		return Plan{}, fmt.Errorf("%w: %w", ErrCheck, err)
	}
	if !newer {
		return Plan{Kind: PlanNone, Release: r}, nil
	}
	if r.DMGURL == "" {
		return Plan{}, fmt.Errorf("%w: release %s has no .dmg", ErrCheck, r.Tag)
	}
	if err := u.Eligible(); err != nil {
		return Plan{Kind: PlanManual, Release: r, Reason: err}, nil
	}
	st, err := u.Stage(ctx, r)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Kind: PlanReady, Release: r, Staged: st}, nil
}

func (u *Updater) removeStale(parent string) {
	stale, _ := filepath.Glob(filepath.Join(parent, ".trebi-update-*"))
	for _, p := range stale {
		_ = os.RemoveAll(p)
	}
}

func (u *Updater) download(ctx context.Context, url, dst, want string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := u.http.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: %s", resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	sum := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, sum), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); !strings.EqualFold(got, want) {
		return fmt.Errorf("%w: got %s, want %s", ErrChecksum, got, want)
	}
	return nil
}
