package connectors

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/registry"
	"github.com/trebi-ai/trebi/internal/store"
)

// ErrNoCatalog is an install or update with no Trebi catalog configured.
var ErrNoCatalog = errors.New("no connector catalog is configured")

// maxRelease caps one release asset download.
const maxRelease = 256 << 20

// digestFile marks a complete snapshot folder.
const digestFile = ".digest"

// Catalog is the Trebi catalog that installs and updates read.
type Catalog interface {
	Versions(ctx context.Context, name string) ([]registry.IndexVersion, error)
	Snapshot(ctx context.Context, name, version string) ([]byte, error)
	Binary(ctx context.Context, name, version, platform string) ([]byte, error)
}

// InstallConfig wires the catalog and the release host of the installs.
type InstallConfig struct {
	Catalog   Catalog
	HTTP      *http.Client // release downloads; nil is a 10 min client
	GitHubURL string       // "" is https://github.com
}

// WithInstall sets the catalog and the release host.
func (s *Service) WithInstall(c InstallConfig) *Service {
	s.catalog = c.Catalog
	s.relHTTP = c.HTTP
	if s.relHTTP == nil {
		s.relHTTP = &http.Client{Timeout: 10 * time.Minute}
	}
	s.githubURL = strings.TrimRight(c.GitHubURL, "/")
	if s.githubURL == "" {
		s.githubURL = "https://github.com"
	}
	return s
}

// InstallRequest installs one catalog connector.
type InstallRequest struct {
	Name    string            `json:"name"`
	Version string            `json:"version,omitempty"`
	Track   string            `json:"track,omitempty"`
	Inputs  map[string]string `json:"inputs,omitempty"`
}

// Install reads one catalog version, installs its program into the version
// folder, writes the entry, and starts the instance (Part E.7). A failure
// removes the version folder that it made.
func (s *Service) Install(ctx context.Context, req InstallRequest) (Resolved, error) {
	if s.catalog == nil {
		return Resolved{}, ErrNoCatalog
	}
	if err := s.checkLoaded(); err != nil {
		return Resolved{}, err
	}
	if err := s.GuardReserved(req.Name); err != nil {
		return Resolved{}, err
	}
	if _, ok := s.Entry(req.Name); ok {
		return Resolved{}, fmt.Errorf("%w: %q", ErrExists, req.Name)
	}
	e := Entry{Name: req.Name, From: FromCatalog, Version: "0.0.0", Track: manifest.Track(req.Track), Inputs: req.Inputs}
	if err := e.Validate(); err != nil {
		return Resolved{}, err
	}
	version, err := s.pickVersion(ctx, req.Name, req.Version, "", "")
	if err != nil {
		return Resolved{}, err
	}
	created, err := s.installVersion(ctx, req.Name, version)
	if err != nil {
		return Resolved{}, err
	}
	e.Version = version
	if err := s.Create(ctx, e); err != nil {
		if created {
			s.removeVersion(ctx, req.Name, version)
		}
		return Resolved{}, err
	}
	s.appendBus(ctx, "connector.installed", req.Name, map[string]string{"version": version})
	return s.Resolve(req.Name, "")
}

// pickVersion returns the wanted version when it exists, else the newest
// version that is not yanked. With cur and a track, it returns the newest
// version on the track after cur, or "".
func (s *Service) pickVersion(ctx context.Context, name, want, cur string, track manifest.Track) (string, error) {
	vers, err := s.catalog.Versions(ctx, name)
	if err != nil {
		return "", err
	}
	if want != "" {
		want = strings.TrimPrefix(want, "v")
		if !slices.ContainsFunc(vers, func(v registry.IndexVersion) bool { return v.Version == want }) {
			return "", fmt.Errorf("connector %q has no catalog version %s: %w", name, want, ErrNotFound)
		}
		return want, nil
	}
	var from manifest.Version
	if cur != "" {
		if from, err = manifest.ParseVersion(cur); err != nil {
			return "", err
		}
	}
	best, bestV := "", manifest.Version{}
	for _, v := range vers {
		pv, err := manifest.ParseVersion(v.Version)
		if err != nil || v.Yanked || pv.Pre != "" {
			continue
		}
		if cur != "" && !track.Follows(from, pv) {
			continue
		}
		if best == "" || pv.CompareFull(bestV) > 0 {
			best, bestV = v.Version, pv
		}
	}
	if best == "" && cur == "" {
		return "", fmt.Errorf("connector %q has no published version: %w", name, ErrNotFound)
	}
	return best, nil
}

// installVersion unpacks one snapshot and installs its program. It reports
// whether it made the folder; a complete folder stays as it is.
func (s *Service) installVersion(ctx context.Context, name, version string) (bool, error) {
	dir := home.ConnectorVersionDir(name, version)
	if _, err := os.Stat(filepath.Join(dir, digestFile)); err == nil {
		return false, nil
	}
	snap, err := s.catalog.Snapshot(ctx, name, version)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return false, err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".install-*")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // best effort; empty after the rename
	if err := untarGz(snap, tmp); err != nil {
		return false, fmt.Errorf("unpack %s@%s: %w", name, version, err)
	}
	m, err := readManifest(tmp)
	if err != nil {
		return false, withLayer(err, name, LayerCatalog)
	}
	if m.Name != name || m.Version != version {
		return false, fmt.Errorf("unpack %s@%s: the snapshot holds %s@%s", name, version, m.Name, m.Version)
	}
	if err := m.Validate(); err != nil {
		return false, withLayer(err, name, LayerCatalog)
	}
	binPath, err := s.installBinary(ctx, m, filepath.Join(tmp, "bin"))
	if err != nil {
		return false, fmt.Errorf("install %s@%s: %w", name, version, err)
	}
	sum := sha256.Sum256(snap)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(tmp, digestFile), []byte(digest+"\n"), 0o600); err != nil {
		return false, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return false, err
	}
	if binPath != "" && strings.HasPrefix(binPath, tmp) {
		binPath = filepath.Join(dir, strings.TrimPrefix(binPath, tmp))
	}
	s.recordInstall(ctx, name, version, digest, binPath)
	return true, nil
}

// installBinary runs the first install method that works and checks
// version_command. It returns the path of the program, or "" when the
// manifest installs none.
func (s *Service) installBinary(ctx context.Context, m manifest.Manifest, bin string) (string, error) {
	in := m.Install
	if in == nil || in.Bin == "" {
		return "", nil
	}
	var errs []error
	methods := []struct {
		name string
		ok   bool
		run  func() error
	}{
		{"catalog", in.Catalog, func() error { return s.installCatalog(ctx, m, bin) }},
		{"github_release", in.GitHubRelease != nil, func() error { return s.installRelease(ctx, m, bin) }},
		{"brew", in.Brew != "", func() error { return runInstall(ctx, nil, "brew", "install", in.Brew) }},
		{"go", in.Go != "", func() error {
			return runInstall(ctx, []string{"GOBIN=" + bin}, "go", "install", expandInstall(in.Go, m.Version))
		}},
	}
	tried := false
	for _, mt := range methods {
		if !mt.ok {
			continue
		}
		tried = true
		if err := mt.run(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", mt.name, err))
			continue
		}
		errs = nil
		break
	}
	if len(errs) > 0 {
		return "", errors.Join(errs...)
	}
	pathEnv := bin + string(os.PathListSeparator) + os.Getenv("PATH")
	found := lookPath(in.Bin, pathEnv)
	if found == "" {
		if tried {
			return "", fmt.Errorf("the command %q is not on PATH after the install", in.Bin)
		}
		return "", nil // the user installs it; doctor reports connector_binary_missing
	}
	if in.VersionCommand != "" {
		cctx, cancel := context.WithTimeout(ctx, versionWait)
		defer cancel()
		cmd := exec.CommandContext(cctx, "sh", "-c", in.VersionCommand)
		cmd.Env = append(os.Environ(), "PATH="+pathEnv)
		out, err := cmd.Output()
		if err != nil && tried {
			return "", fmt.Errorf("%s: %w", in.VersionCommand, err)
		}
		if err == nil && !strings.Contains(string(out), m.Version) {
			s.log.Warn("connector version", "connector", m.Name, "want", m.Version, "got", strings.TrimSpace(string(out)))
		}
	}
	return found, nil
}

// expandInstall fills {version}, {os}, and {arch}.
func expandInstall(v, version string) string {
	return strings.NewReplacer("{version}", version, "{os}", runtime.GOOS, "{arch}", runtime.GOARCH).Replace(v)
}

// runInstall runs one install command when the tool is on PATH.
func runInstall(ctx context.Context, env []string, name string, args ...string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("%s is not installed", name)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, lastLine(out))
	}
	return nil
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}

// installCatalog writes the program from the catalog archive of this
// platform into bin. The catalog checks the digest.
func (s *Service) installCatalog(ctx context.Context, m manifest.Manifest, bin string) error {
	platform := runtime.GOOS + "/" + runtime.GOARCH
	if len(m.Platforms) > 0 && !slices.Contains(m.Platforms, platform) {
		return fmt.Errorf("no program for %s", platform)
	}
	data, err := s.catalog.Binary(ctx, m.Name, m.Version, platform)
	if err != nil {
		return err
	}
	prog, err := extractProgram(m.Install.Bin+".tar.gz", data, m.Install.Bin)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(bin, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(bin, m.Install.Bin), prog, 0o755) //nolint:gosec // the program must run
}

// installRelease downloads the release asset of this platform, checks it
// against the checksums file, and writes the program into bin.
func (s *Service) installRelease(ctx context.Context, m manifest.Manifest, bin string) error {
	gh := m.Install.GitHubRelease
	platform := runtime.GOOS + "/" + runtime.GOARCH
	if len(m.Platforms) > 0 && !slices.Contains(m.Platforms, platform) {
		return fmt.Errorf("no release for %s", platform)
	}
	base := s.githubURL + "/" + gh.Repo + "/releases/download/" + expandInstall(gh.Tag, m.Version) + "/"
	asset := expandInstall(gh.Asset, m.Version)
	data, err := s.download(ctx, base+asset)
	if err != nil {
		return err
	}
	if gh.Checksums != "" {
		sums, err := s.download(ctx, base+expandInstall(gh.Checksums, m.Version))
		if err != nil {
			return fmt.Errorf("checksums: %w", err)
		}
		want, ok := parseSums(sums)[asset]
		sum := sha256.Sum256(data)
		switch {
		case !ok:
			return fmt.Errorf("%s is not in %s", asset, gh.Checksums)
		case hex.EncodeToString(sum[:]) != want:
			return fmt.Errorf("%s does not match its checksum", asset)
		}
	}
	prog, err := extractProgram(asset, data, m.Install.Bin)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(bin, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(bin, m.Install.Bin), prog, 0o755) //nolint:gosec // the program must run
}

// download runs one GET against the release host.
func (s *Service) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.relHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxRelease))
}

// parseSums reads "<hex>  <name>" lines.
func parseSums(data []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 {
			out[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	return out
}

// extractProgram returns the file named bin from a tar.gz or zip asset, or
// the asset itself when it is not an archive.
func extractProgram(asset string, data []byte, bin string) ([]byte, error) {
	switch {
	case strings.HasSuffix(asset, ".tar.gz"), strings.HasSuffix(asset, ".tgz"):
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		tr := tar.NewReader(zr)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			if hdr.Typeflag == tar.TypeReg && path.Base(hdr.Name) == bin {
				return io.ReadAll(io.LimitReader(tr, maxRelease))
			}
		}
	case strings.HasSuffix(asset, ".zip"):
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if f.FileInfo().Mode().IsRegular() && path.Base(f.Name) == bin {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(io.LimitReader(rc, maxRelease))
			}
		}
	default:
		return data, nil
	}
	return nil, fmt.Errorf("%s has no file %q", asset, bin)
}

// untarGz unpacks a snapshot into dir. It keeps only folders and regular
// files, and refuses a path that leaves dir.
func untarGz(data []byte, dir string) error {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(hdr.Name)
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("unsafe path %q", hdr.Name)
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			total += hdr.Size
			if total > maxRelease {
				return errors.New("snapshot is too large")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			b, err := io.ReadAll(io.LimitReader(tr, hdr.Size))
			if err != nil {
				return err
			}
			if err := os.WriteFile(target, b, 0o600); err != nil {
				return err
			}
		}
	}
}

// recordInstall writes the derived connector_installs row.
func (s *Service) recordInstall(ctx context.Context, name, version, digest, binPath string) {
	if _, err := s.st.DB().ExecContext(ctx, `INSERT INTO connector_installs (name, version, digest, bin_path, installed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (name, version) DO UPDATE SET digest = excluded.digest, bin_path = excluded.bin_path, installed_at = excluded.installed_at`,
		name, version, digest, binPath, s.now().UTC().Format(time.RFC3339)); err != nil {
		s.log.Warn("connector install row", "connector", name, "version", version, "error", err)
	}
}

// removeVersion deletes one version folder and its install row.
func (s *Service) removeVersion(ctx context.Context, name, version string) {
	if err := os.RemoveAll(home.ConnectorVersionDir(name, version)); err != nil {
		s.log.Warn("remove connector version", "connector", name, "version", version, "error", err)
	}
	if _, err := s.st.DB().ExecContext(ctx, `DELETE FROM connector_installs WHERE name = ? AND version = ?`, name, version); err != nil {
		s.log.Warn("connector install row", "connector", name, "version", version, "error", err)
	}
}

// installedVersions lists the complete version folders of each connector.
func installedVersions() map[string][]string {
	out := map[string][]string{}
	names, err := os.ReadDir(home.ConnectorsDir())
	if err != nil {
		return out
	}
	for _, n := range names {
		if !n.IsDir() || strings.HasPrefix(n.Name(), ".") {
			continue
		}
		vers, err := os.ReadDir(filepath.Join(home.ConnectorsDir(), n.Name()))
		if err != nil {
			continue
		}
		for _, v := range vers {
			if _, err := os.Stat(filepath.Join(home.ConnectorVersionDir(n.Name(), v.Name()), digestFile)); err == nil {
				out[n.Name()] = append(out[n.Name()], v.Name())
			}
		}
	}
	return out
}

// VersionRow is one catalog version of a connector.
type VersionRow struct {
	Version     string   `json:"version"`
	PublishedAt string   `json:"published_at,omitempty"`
	Yanked      bool     `json:"yanked"`
	Installed   bool     `json:"installed"`
	Current     bool     `json:"current"`
	Workspaces  []string `json:"workspaces"`
}

// Versions lists the catalog versions of a connector, newest first, with
// the installed flag and the layers that use each one.
func (s *Service) Versions(ctx context.Context, name string) ([]VersionRow, error) {
	if s.catalog == nil {
		return nil, ErrNoCatalog
	}
	vers, err := s.catalog.Versions(ctx, name)
	if err != nil {
		return nil, err
	}
	installed := installedVersions()[name]
	e, _ := s.Entry(name)
	s.mu.Lock()
	byVersion := map[string][]string{}
	for ws, ovs := range s.overrides {
		if ov, ok := ovs[name]; ok && ov.Version != "" {
			byVersion[ov.Version] = append(byVersion[ov.Version], ws)
		}
	}
	s.mu.Unlock()
	out := make([]VersionRow, 0, len(vers))
	for i := len(vers) - 1; i >= 0; i-- {
		v := vers[i]
		ws := byVersion[v.Version]
		slices.Sort(ws)
		out = append(out, VersionRow{
			Version: v.Version, PublishedAt: v.PublishedAt, Yanked: v.Yanked,
			Installed:  slices.Contains(installed, v.Version),
			Current:    e.From == FromCatalog && e.Version == v.Version,
			Workspaces: orEmptySlice(ws),
		})
	}
	return out, nil
}

// UpdateResult is one connector that CheckUpdates found behind.
type UpdateResult struct {
	Name    string `json:"name"`
	From    string `json:"from"`
	To      string `json:"to"`
	Applied bool   `json:"applied"`
	Error   string `json:"error,omitempty"`
}

// CheckUpdates finds the newest catalog version on the track of each catalog
// entry. The patch track applies it at once. The minor and latest tracks
// record it and emit connector.update.available. An empty name checks every
// entry.
func (s *Service) CheckUpdates(ctx context.Context, name string) ([]UpdateResult, error) {
	if s.catalog == nil {
		return nil, ErrNoCatalog
	}
	entries := s.snapshotEntries()
	if name != "" {
		e, ok := entries[name]
		if !ok {
			return nil, fmt.Errorf("connector %q not found: %w", name, ErrNotFound)
		}
		entries = map[string]Entry{name: e}
	}
	out := []UpdateResult{}
	for _, n := range sortedNames(entries) {
		e := entries[n]
		if e.From != FromCatalog || e.track() == manifest.TrackExact {
			continue
		}
		next, err := s.pickVersion(ctx, n, "", e.Version, e.track())
		if err != nil {
			if name != "" {
				return nil, err
			}
			s.log.Warn("connector update check", "connector", n, "error", err)
			continue
		}
		if next == "" {
			s.setUpdate(n, "")
			continue
		}
		res := UpdateResult{Name: n, From: e.Version, To: next}
		if e.track() == manifest.TrackPatch {
			if err := s.Update(ctx, n, "", next); err != nil {
				res.Error = err.Error()
			} else {
				res.Applied = true
			}
		} else if s.setUpdate(n, next) {
			s.appendBus(ctx, "connector.update.available", n, map[string]string{"version": next, "current": e.Version})
		}
		out = append(out, res)
	}
	return out, nil
}

// setUpdate records the available version of a connector. It reports
// whether the value changed.
func (s *Service) setUpdate(name, version string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updates[name] == version {
		return false
	}
	if version == "" {
		delete(s.updates, name)
	} else {
		s.updates[name] = version
	}
	return true
}

// availableUpdate returns the recorded update of a connector, or "".
func (s *Service) availableUpdate(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updates[name]
}

// Update installs a version side by side, switches the global entry or the
// workspace layer to it, and restarts the instance. An empty version is the
// newest version that is not yanked.
func (s *Service) Update(ctx context.Context, name, workspaceID, version string) error {
	if s.catalog == nil {
		return ErrNoCatalog
	}
	e, ok := s.Entry(name)
	if !ok {
		return fmt.Errorf("connector %q not found: %w", name, ErrNotFound)
	}
	if e.From != FromCatalog {
		return &ValidationError{Name: name, Fields: []FieldError{{Field: "from", Message: "only a catalog connector has versions"}}}
	}
	version, err := s.pickVersion(ctx, name, version, "", "")
	if err != nil {
		return err
	}
	created, err := s.installVersion(ctx, name, version)
	if err != nil {
		return err
	}
	from := e.Version
	if workspaceID == "" {
		err = s.Patch(ctx, name, EntryPatch{Version: &version})
	} else {
		ov, _, oerr := s.Override(ctx, workspaceID, name)
		if oerr != nil {
			return oerr
		}
		from = ov.Version
		ov.Version = version
		err = s.SetOverride(ctx, workspaceID, name, &ov)
	}
	if err != nil {
		if created {
			s.removeVersion(ctx, name, version)
		}
		return err
	}
	if workspaceID == "" {
		s.setUpdate(name, "")
	}
	s.appendBus(ctx, "connector.updated.version", name, map[string]string{"from": from, "to": version, "workspace": workspaceID})
	if _, err := s.GC(ctx, false); err != nil {
		s.log.Warn("connector gc", "error", err)
	}
	return nil
}

// GC removes each version folder that no layer uses. With dryRun it only
// lists them as name@version.
func (s *Service) GC(ctx context.Context, dryRun bool) ([]string, error) {
	used := map[string]bool{}
	for n, e := range s.snapshotEntries() {
		if e.From == FromCatalog {
			used[n+"@"+e.Version] = true
		}
	}
	s.mu.Lock()
	for _, ovs := range s.overrides {
		for n, ov := range ovs {
			if ov.Version != "" {
				used[n+"@"+ov.Version] = true
			}
		}
	}
	s.mu.Unlock()
	removed := []string{}
	installed := installedVersions()
	for _, n := range sortedNames(installed) {
		for _, v := range installed[n] {
			if used[n+"@"+v] {
				continue
			}
			removed = append(removed, n+"@"+v)
			if !dryRun {
				s.removeVersion(ctx, n, v)
			}
		}
		if !dryRun {
			_ = os.Remove(filepath.Join(home.ConnectorsDir(), n)) // only an empty folder goes
		}
	}
	return removed, nil
}

// appendBus appends one connector domain event.
func (s *Service) appendBus(ctx context.Context, topic, name string, attrs map[string]string) {
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: topic, Entity: "connector", EntityID: name, Action: strings.TrimPrefix(topic, "connector."), Attrs: attrs,
	}); err != nil {
		s.log.Warn("bus append", "topic", topic, "error", err)
	}
}
