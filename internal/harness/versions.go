package harness

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	config "github.com/trebi-ai/trebi/internal/config"
	"github.com/trebi-ai/trebi/internal/store"
)

// VersionSource is the version control of one harness program.
type VersionSource interface {
	Installed(ctx context.Context, bin string) (string, error) // `<bin> --version`, parsed
	Latest(ctx context.Context) (string, error)
	Update(ctx context.Context, bin string) error // the vendor command
	AutoUpdateOff() (env map[string]string)       // for Trebi runs
}

// VersionInstaller is a VersionSource that installs one named version.
type VersionInstaller interface {
	Install(ctx context.Context, bin, version string) error
}

// VersionSources finds the source and the vendor program of a harness.
type VersionSources interface {
	Source(name string) (VersionSource, bool)
	// VendorBin is the program of the vendor install, not the kept copy.
	VendorBin(name string) string
}

// RunActivity reports whether a run uses a harness now.
type RunActivity interface {
	HarnessActive(ctx context.Context, name string) bool
}

// AuthProber runs the live auth check after an update.
type AuthProber interface {
	Probe(ctx context.Context, name, source string) (AuthStatus, error)
	GetState(ctx context.Context, name string) (State, error)
}

// Update modes of tools.<name>.updates.
const (
	UpdatesAuto = "auto"
	UpdatesKeep = "keep"
)

// Version timing.
const (
	VersionCheckEvery = 6 * time.Hour
	versionTick       = time.Minute
	// UpdateGateMax is the longest time new runs wait for an update.
	UpdateGateMax = 2 * time.Minute
	updateTimeout = 10 * time.Minute
	// KeptMaxAge is the age of a kept version that doctor reports.
	KeptMaxAge = 90 * 24 * time.Hour
)

// Version errors.
var (
	ErrHarnessBusy      = errors.New("a run uses this harness; Trebi updates it when the run ends")
	ErrNoVersionSource  = errors.New("trebi cannot update this harness")
	ErrNoVersionInstall = errors.New("this harness cannot install a specific version")
	ErrNotInstalled     = errors.New("the harness program is not installed")
)

// LastUpdate is the result of the last update.
type LastUpdate struct {
	At    string `json:"at"`
	From  string `json:"from"`
	To    string `json:"to"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// VersionState is the version part of the harness_state row.
type VersionState struct {
	Version    string
	Latest     string
	CheckedAt  string
	LastUpdate *LastUpdate
}

// UpdateAvailable reports whether Latest is newer than Version.
func (v VersionState) UpdateAvailable() bool {
	return v.Version != "" && NewerVersion(v.Latest, v.Version)
}

// VersionManager checks, updates, and keeps the harness programs.
type VersionManager struct {
	st    *store.Store
	src   VersionSources
	runs  RunActivity
	auth  AuthProber
	log   *slog.Logger
	now   func() time.Time
	every time.Duration

	mu       sync.Mutex
	updating map[string]time.Time
	pending  map[string]bool
	seen     map[string]string // effective program path + mtime
}

// NewVersionManager builds the manager. runs and auth may be nil in tests.
func NewVersionManager(st *store.Store, src VersionSources, runs RunActivity, auth AuthProber) *VersionManager {
	return &VersionManager{
		st: st, src: src, runs: runs, auth: auth,
		log:      slog.Default().With("source", "harness_versions"),
		now:      time.Now,
		every:    VersionCheckEvery,
		updating: map[string]time.Time{},
		pending:  map[string]bool{},
		seen:     map[string]string{},
	}
}

// Run checks the harnesses that are on each minute until ctx ends. The
// first pass waits one tick, so the auth probe writes the state rows first.
func (m *VersionManager) Run(ctx context.Context) {
	t := time.NewTicker(versionTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.Tick(ctx)
	}
}

// Tick does one pass: it records outside changes, checks each 6 hours, and
// starts the auto updates and the pending updates.
func (m *VersionManager) Tick(ctx context.Context) {
	cfg, _ := config.Load()
	for _, name := range Names() {
		if _, ok := m.src.Source(name); !ok || !cfg.HarnessEnabled(name) {
			continue
		}
		m.tickOne(ctx, cfg, name)
	}
}

func (m *VersionManager) tickOne(ctx context.Context, cfg *config.Config, name string) {
	st, _ := m.State(ctx, name)
	changed := m.programChanged(name)
	if changed || m.due(st.CheckedAt) {
		var err error
		if st, err = m.check(ctx, name, m.due(st.CheckedAt)); err != nil {
			m.log.Debug("version check", "harness", name, "error", err)
			return
		}
	}
	m.mu.Lock()
	pending := m.pending[name]
	m.mu.Unlock()
	auto := updatesMode(cfg, name) == UpdatesAuto && st.UpdateAvailable() && !failedBefore(st)
	if !pending && !auto {
		return
	}
	if m.busy(ctx, name) {
		return
	}
	m.mu.Lock()
	delete(m.pending, name)
	m.mu.Unlock()
	if err := m.update(ctx, name); err != nil {
		m.log.Warn("harness update", "harness", name, "error", err)
	}
}

// failedBefore reports whether the last update to Latest failed. Trebi tries
// again for a newer version or on a click.
func failedBefore(st VersionState) bool {
	return st.LastUpdate != nil && !st.LastUpdate.OK && st.LastUpdate.To == st.Latest
}

func (m *VersionManager) due(checkedAt string) bool {
	if checkedAt == "" {
		return true
	}
	return m.now().Sub(store.ParseTS(checkedAt)) >= m.every
}

// programChanged reports whether the effective program changed on disk
// since the last tick. The first tick counts as a change.
func (m *VersionManager) programChanged(name string) bool {
	bin := m.bin(name)
	stamp := ""
	if real, err := filepath.EvalSymlinks(bin); err == nil {
		if fi, err := os.Stat(real); err == nil {
			stamp = real + "|" + fi.ModTime().UTC().Format(time.RFC3339Nano)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	prev, ok := m.seen[name]
	m.seen[name] = stamp
	return !ok || prev != stamp
}

// Updating reports whether an update of name runs now. The gate opens after
// UpdateGateMax, so a queued run waits at most that long.
func (m *VersionManager) Updating(name string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.updating[name]
	return ok && m.now().Sub(at) < UpdateGateMax
}

func (m *VersionManager) busy(ctx context.Context, name string) bool {
	return m.runs != nil && m.runs.HarnessActive(ctx, name)
}

// CanUpdate reports whether Trebi can update name.
func (m *VersionManager) CanUpdate(name string) bool {
	_, ok := m.src.Source(name)
	return ok
}

// CanInstallVersion reports whether name installs a named version.
func (m *VersionManager) CanInstallVersion(name string) bool {
	src, ok := m.src.Source(name)
	if !ok {
		return false
	}
	_, ok = src.(VersionInstaller)
	return ok
}

// Check reads the installed and the latest version now.
func (m *VersionManager) Check(ctx context.Context, name string) (VersionState, error) {
	return m.check(ctx, name, true)
}

func (m *VersionManager) check(ctx context.Context, name string, latest bool) (VersionState, error) {
	src, ok := m.src.Source(name)
	if !ok {
		return VersionState{}, ErrNoVersionSource
	}
	prev, err := m.State(ctx, name)
	if err != nil {
		return prev, err
	}
	next := prev
	bin := m.bin(name)
	if bin == "" {
		return prev, ErrNotInstalled
	}
	if next.Version, err = src.Installed(ctx, bin); err != nil {
		return prev, err
	}
	if latest {
		if next.Latest, err = src.Latest(ctx); err != nil {
			m.log.Debug("latest version", "harness", name, "error", err)
			next.Latest = prev.Latest
		}
		next.CheckedAt = m.now().UTC().Format(time.RFC3339Nano)
	}
	m.mu.Lock()
	_, ours := m.updating[name]
	m.mu.Unlock()
	if prev.Version != "" && next.Version != prev.Version && !ours {
		// The vendor CLI updated itself outside Trebi. The old program is
		// gone, so Trebi records the check and does not go back.
		next.LastUpdate = m.verify(ctx, name, prev.Version, next.Version)
	}
	if err := m.save(ctx, name, next); err != nil {
		return next, err
	}
	if next.Version != prev.Version || next.Latest != prev.Latest {
		m.publish(ctx, name, next)
	}
	return next, nil
}

// RequestUpdate updates name now. A run that uses the harness defers the
// update to the end of the run and returns ErrHarnessBusy.
func (m *VersionManager) RequestUpdate(ctx context.Context, name string) error {
	if _, ok := m.src.Source(name); !ok {
		return ErrNoVersionSource
	}
	if m.busy(ctx, name) {
		m.mu.Lock()
		m.pending[name] = true
		m.mu.Unlock()
		return ErrHarnessBusy
	}
	return m.update(ctx, name)
}

// update runs the vendor update with a last good copy and a check. A failed
// check points current to the last good copy.
func (m *VersionManager) update(ctx context.Context, name string) error {
	src, ok := m.src.Source(name)
	if !ok {
		return ErrNoVersionSource
	}
	vendor := m.src.VendorBin(name)
	if vendor == "" {
		return ErrNotInstalled
	}
	if !m.begin(name) {
		return ErrHarnessBusy
	}
	defer m.end(name)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), updateTimeout)
	defer cancel()
	prev, _ := m.State(ctx, name)
	from, err := src.Installed(ctx, vendor)
	if err != nil {
		return err
	}
	if err := copyProgram(vendor, m.storeDir(name, from), name); err != nil {
		return fmt.Errorf("last good copy: %w", err)
	}
	target := cmpOr(prev.Latest, from)
	next := prev
	next.Version = from
	if uerr := src.Update(ctx, vendor); uerr != nil {
		next.LastUpdate = m.lastUpdate(from, target, uerr.Error())
	} else if to, ierr := src.Installed(ctx, vendor); ierr != nil {
		next.LastUpdate = m.lastUpdate(from, target, ierr.Error())
	} else {
		next.LastUpdate = m.verify(ctx, name, from, to)
		if next.LastUpdate.OK {
			next.Version = to
		}
	}
	return m.finish(ctx, name, next, from)
}

// finish applies the result of an update: keep mode keeps the new program,
// a failure goes back to the last good copy.
func (m *VersionManager) finish(ctx context.Context, name string, next VersionState, from string) error {
	lu := next.LastUpdate
	switch {
	case !lu.OK:
		if err := m.pointCurrent(name, from); err != nil {
			m.log.Warn("roll back", "harness", name, "error", err)
		}
		next.Version = from
	case m.mode(name) == UpdatesKeep:
		if err := m.keepVendor(name, next.Version); err != nil {
			lu.OK, lu.Error = false, err.Error()
		}
	default:
		if err := m.removeCurrent(name); err != nil {
			m.log.Warn("remove current", "harness", name, "error", err)
		}
		m.prune(name, from)
	}
	if err := m.save(ctx, name, next); err != nil {
		return err
	}
	m.publish(ctx, name, next)
	if !lu.OK {
		return errors.New(lu.Error)
	}
	return nil
}

// verify runs the post-update check: the program answers and a login that
// worked before still works.
func (m *VersionManager) verify(ctx context.Context, name, from, to string) *LastUpdate {
	fail := ""
	if m.auth != nil {
		before, _ := m.auth.GetState(ctx, name)
		wasOK := before.AuthOK.Valid && before.AuthOK.Int64 == 1
		st, err := m.auth.Probe(ctx, name, SourceProbe)
		if wasOK && (err != nil || !st.OK()) {
			fail = "The login check failed after the update."
			if st.Detail != "" {
				fail += " " + st.Detail
			}
		}
	}
	return m.lastUpdate(from, to, fail)
}

func (m *VersionManager) lastUpdate(from, to, fail string) *LastUpdate {
	return &LastUpdate{At: m.now().UTC().Format(time.RFC3339Nano), From: from, To: to, OK: fail == "", Error: fail}
}

func (m *VersionManager) begin(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.updating[name]; ok {
		return false
	}
	m.updating[name] = m.now()
	return true
}

func (m *VersionManager) end(name string) {
	m.mu.Lock()
	delete(m.updating, name)
	m.mu.Unlock()
}

// Keep copies the program into the store and points current to it. A set
// version that is not installed is installed first, where the harness can.
func (m *VersionManager) Keep(ctx context.Context, name, version string) error {
	src, ok := m.src.Source(name)
	if !ok {
		return ErrNoVersionSource
	}
	vendor := m.src.VendorBin(name)
	if vendor == "" {
		return ErrNotInstalled
	}
	installed, err := src.Installed(ctx, vendor)
	if err != nil {
		return err
	}
	if version != "" && version != installed {
		if _, err := os.Stat(filepath.Join(m.storeDir(name, version), name)); err == nil {
			return m.finishKeep(ctx, name, version)
		}
		inst, ok := src.(VersionInstaller)
		if !ok {
			return ErrNoVersionInstall
		}
		if m.busy(ctx, name) {
			return ErrHarnessBusy
		}
		if !m.begin(name) {
			return ErrHarnessBusy
		}
		err := inst.Install(ctx, vendor, version)
		m.end(name)
		if err != nil {
			return err
		}
		if installed, err = src.Installed(ctx, vendor); err != nil {
			return err
		}
	}
	if err := m.keepVendor(name, installed); err != nil {
		return err
	}
	return m.finishKeep(ctx, name, installed)
}

func (m *VersionManager) finishKeep(ctx context.Context, name, version string) error {
	if err := m.pointCurrent(name, version); err != nil {
		return err
	}
	if err := setPin(name, "version", version); err != nil {
		return err
	}
	st, _ := m.State(ctx, name)
	st.Version = version
	if err := m.save(ctx, name, st); err != nil {
		return err
	}
	m.publish(ctx, name, st)
	return nil
}

// keepVendor copies the vendor program as version and points current to it.
func (m *VersionManager) keepVendor(name, version string) error {
	vendor := m.src.VendorBin(name)
	if vendor == "" {
		return ErrNotInstalled
	}
	if err := copyProgram(vendor, m.storeDir(name, version), name); err != nil {
		return err
	}
	if err := setPin(name, "version", version); err != nil {
		return err
	}
	return m.pointCurrent(name, version)
}

// Auto removes current, so runs use the vendor program again.
func (m *VersionManager) Auto(ctx context.Context, name string) error {
	if err := m.removeCurrent(name); err != nil {
		return err
	}
	_, err := m.check(ctx, name, false)
	if errors.Is(err, ErrNoVersionSource) || errors.Is(err, ErrNotInstalled) {
		return nil
	}
	return err
}

// KeptSince returns the kept version and the time Trebi kept it.
func KeptSince(name string) (string, time.Time, bool) {
	cur := filepath.Join(config.BinDir(), name, "current")
	fi, err := os.Lstat(cur)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return "", time.Time{}, false
	}
	target, err := os.Readlink(cur)
	if err != nil {
		return "", time.Time{}, false
	}
	return filepath.Base(target), fi.ModTime(), true
}

func (m *VersionManager) mode(name string) string {
	cfg, _ := config.Load()
	return updatesMode(cfg, name)
}

func updatesMode(cfg *config.Config, name string) string {
	if cfg != nil {
		if p := cfg.Tools.Pin(name); p != nil && p.Updates == UpdatesKeep {
			return UpdatesKeep
		}
	}
	return UpdatesAuto
}

// bin is the program that Trebi runs: the kept copy, else the vendor program.
func (m *VersionManager) bin(name string) string {
	cur := filepath.Join(config.BinDir(), name, "current", name)
	if fi, err := os.Stat(cur); err == nil && !fi.IsDir() {
		return cur
	}
	return m.src.VendorBin(name)
}

func (m *VersionManager) storeDir(name, version string) string {
	return filepath.Join(config.BinDir(), name, version)
}

// pointCurrent points bin/<name>/current to the version folder.
func (m *VersionManager) pointCurrent(name, version string) error {
	if _, err := os.Stat(filepath.Join(m.storeDir(name, version), name)); err != nil {
		return err
	}
	cur := filepath.Join(config.BinDir(), name, "current")
	tmp := cur + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(version, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, cur)
}

// removeCurrent removes the current link. A real folder stays: the user
// made it.
func (m *VersionManager) removeCurrent(name string) error {
	cur := filepath.Join(config.BinDir(), name, "current")
	fi, err := os.Lstat(cur)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	return os.Remove(cur)
}

// prune deletes the store copies except keep and the current target.
func (m *VersionManager) prune(name, keep string) {
	dir := filepath.Join(config.BinDir(), name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	kept, _, _ := KeptSince(name)
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() || n == keep || n == kept || n == "current" || !versionRe.MatchString(n) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, n))
	}
}

// copyProgram copies the real file of bin, and the files next to it whose
// names start with its name and "-", into dir as name.
func copyProgram(bin, dir, name string) error {
	real, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
		return nil
	}
	tmp := dir + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	if err := copyFile(real, filepath.Join(tmp, name)); err != nil {
		return err
	}
	base := filepath.Base(real)
	entries, _ := os.ReadDir(filepath.Dir(real))
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasPrefix(e.Name(), base+"-") {
			if err := copyFile(filepath.Join(filepath.Dir(real), e.Name()), filepath.Join(tmp, e.Name())); err != nil {
				return err
			}
		}
	}
	_ = os.RemoveAll(dir)
	return os.Rename(tmp, dir)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// setPin writes tools.<name>.<key> when it changes.
func setPin(name, key, value string) error {
	if !config.IsFirstClass(name) {
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if p := cfg.Tools.Pin(name); p != nil && key == "version" && p.Version == value {
		return nil
	}
	if err := cfg.SetPath("tools."+name+"."+key, value); err != nil {
		return err
	}
	return cfg.Save()
}

// State reads the version columns of name.
func (m *VersionManager) State(ctx context.Context, name string) (VersionState, error) {
	return ReadVersionState(ctx, m.st.DB(), name)
}

// ReadVersionState reads the version columns of name from db.
func ReadVersionState(ctx context.Context, db *sql.DB, name string) (VersionState, error) {
	var v VersionState
	var last string
	err := db.QueryRowContext(ctx, `SELECT COALESCE(version,''), COALESCE(latest_version,''),
		COALESCE(checked_at,''), COALESCE(last_update,'') FROM harness_state WHERE name=?`, name).
		Scan(&v.Version, &v.Latest, &v.CheckedAt, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	if last != "" {
		var lu LastUpdate
		if json.Unmarshal([]byte(last), &lu) == nil {
			v.LastUpdate = &lu
		}
	}
	return v, nil
}

func (m *VersionManager) save(ctx context.Context, name string, v VersionState) error {
	var last any
	if v.LastUpdate != nil {
		b, _ := json.Marshal(v.LastUpdate)
		last = string(b)
	}
	now := m.now().UTC().Format(time.RFC3339Nano)
	_, err := m.st.DB().ExecContext(ctx, `INSERT INTO harness_state
		(name, version, latest_version, checked_at, last_update, updated_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET version=excluded.version, latest_version=excluded.latest_version,
			checked_at=excluded.checked_at, last_update=excluded.last_update, updated_at=excluded.updated_at`,
		name, store.NullStr(v.Version), store.NullStr(v.Latest), store.NullStr(v.CheckedAt), last, now)
	return err
}

// publish appends harness.updated with the version. An update result adds
// update_ok and update_error, which raise or resolve the alert.
func (m *VersionManager) publish(ctx context.Context, name string, v VersionState) {
	attrs := map[string]string{"harness": name, "version": v.Version, "latest_version": v.Latest}
	if lu := v.LastUpdate; lu != nil && lu.At != "" && m.now().Sub(store.ParseTS(lu.At)) < time.Minute {
		attrs["update_ok"] = strconv.FormatBool(lu.OK)
		attrs["update_error"] = lu.Error
		attrs["update_from"] = lu.From
		attrs["update_to"] = lu.To
	}
	if _, err := m.st.Append(ctx, store.BusEvent{
		Topic: "harness.updated", Entity: "harness", EntityID: name, Action: "updated", Attrs: attrs,
	}); err != nil {
		m.log.Warn("harness.updated", "harness", name, "error", err)
	}
}

// versionRe finds a version in program output or a release tag.
var versionRe = regexp.MustCompile(`\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?`)

// ParseVersion returns the first version in s.
func ParseVersion(s string) string {
	return versionRe.FindString(s)
}

// NewerVersion reports whether a is newer than b. A pre-release suffix sorts
// before its release.
func NewerVersion(a, b string) bool {
	if a == "" || b == "" || a == b {
		return false
	}
	an, apre, _ := strings.Cut(a, "-")
	bn, bpre, _ := strings.Cut(b, "-")
	ap, bp := strings.Split(an, "."), strings.Split(bn, ".")
	for i := 0; i < len(ap) || i < len(bp); i++ {
		var x, y int
		if i < len(ap) {
			x, _ = strconv.Atoi(ap[i])
		}
		if i < len(bp) {
			y, _ = strconv.Atoi(bp[i])
		}
		if x != y {
			return x > y
		}
	}
	switch {
	case apre == "" && bpre != "":
		return true
	case apre != "" && bpre == "":
		return false
	}
	return apre > bpre
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
