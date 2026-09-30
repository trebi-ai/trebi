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
	"maps"
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

// InstallRequest adds one connection of a catalog type. Name is the type.
// An empty slug is the default slug of the type.
type InstallRequest struct {
	Name    string            `json:"name"`
	Version string            `json:"version,omitempty"`
	Track   string            `json:"track,omitempty"`
	Inputs  map[string]string `json:"inputs,omitempty"`
	Slug    string            `json:"slug,omitempty"`
	Title   string            `json:"title,omitempty"`
}

// Install reads one catalog version, installs its program into the package
// folder, creates the connection, and starts it (Part E.7). A failure
// removes the package folder that it made.
func (s *Service) Install(ctx context.Context, req InstallRequest) (Connection, error) {
	if s.catalog == nil {
		return Connection{}, ErrNoCatalog
	}
	if err := s.GuardReserved(req.Name); err != nil {
		return Connection{}, err
	}
	if req.Slug != "" {
		if err := s.GuardReserved(req.Slug); err != nil {
			return Connection{}, err
		}
		if !s.slugFree(req.Slug, "") {
			return Connection{}, fmt.Errorf("%w: slug %q", ErrExists, req.Slug)
		}
	}
	version, err := s.pickVersion(ctx, req.Name, req.Version, "", "")
	if err != nil {
		return Connection{}, err
	}
	created, digest, err := s.installVersion(ctx, req.Name, version)
	if err != nil {
		return Connection{}, err
	}
	c, err := s.Create(ctx, Connection{
		Type: req.Name, Slug: req.Slug, Title: req.Title, From: FromCatalog,
		Version: version, Digest: digest, Track: manifest.Track(req.Track), Inputs: req.Inputs,
	})
	if err != nil {
		if created {
			s.removeVersion(ctx, req.Name, version)
		}
		return Connection{}, err
	}
	s.appendBus(ctx, "connector.installed", c.ID, map[string]string{"version": version, "type": c.Type, "slug": c.Slug})
	return c, nil
}

// EnsurePackage installs the missing package of a catalog connection by its
// version and checks it against the digest in the record. A restore needs
// no user step.
func (s *Service) EnsurePackage(ctx context.Context, c Connection) error {
	if c.From != FromCatalog {
		return nil
	}
	have := packageDigest(c.Type, c.Version)
	if have == "" {
		if s.catalog == nil {
			return fmt.Errorf("%w: %s %s", ErrNotInstalled, c.Type, c.Version)
		}
		created, digest, err := s.installVersion(ctx, c.Type, c.Version)
		if err != nil {
			return err
		}
		if c.Digest != "" && digest != c.Digest {
			if created {
				s.removeVersion(ctx, c.Type, c.Version)
			}
			return fmt.Errorf("connector %q: the catalog package %s %s has digest %s, not %s", c.Slug, c.Type, c.Version, digest, c.Digest)
		}
		return nil
	}
	if c.Digest != "" && have != c.Digest {
		return fmt.Errorf("connector %q: the package %s %s has digest %s, not %s", c.Slug, c.Type, c.Version, have, c.Digest)
	}
	return nil
}

// EnsurePackages runs EnsurePackage for each catalog connection whose
// package folder is missing. A failure is logged; the row shows the resolve
// error.
func (s *Service) EnsurePackages(ctx context.Context) {
	for _, c := range s.snapshot() {
		if c.From != FromCatalog || packageDigest(c.Type, c.Version) != "" {
			continue
		}
		if err := s.EnsurePackage(ctx, c); err != nil {
			s.log.Warn("connector package", "connector", c.ID, "error", err)
		}
	}
}

// packageDigest reads the digest of a complete package folder, or "".
func packageDigest(typ, version string) string {
	b, err := os.ReadFile(filepath.Join(home.PackageDir(typ, version), digestFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
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
// whether it made the folder, and the package digest; a complete folder
// stays as it is.
func (s *Service) installVersion(ctx context.Context, name, version string) (bool, string, error) {
	dir := home.PackageDir(name, version)
	if d := packageDigest(name, version); d != "" {
		return false, d, nil
	}
	snap, err := s.catalog.Snapshot(ctx, name, version)
	if err != nil {
		return false, "", err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return false, "", err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".install-*")
	if err != nil {
		return false, "", err
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // best effort; empty after the rename
	m, err := unpackSnapshot(snap, tmp, name, version)
	if err != nil {
		return false, "", err
	}
	binPath, err := s.installBinary(ctx, m, filepath.Join(tmp, "bin"))
	if err != nil {
		return false, "", fmt.Errorf("install %s@%s: %w", name, version, err)
	}
	sum := sha256.Sum256(snap)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(tmp, digestFile), []byte(digest+"\n"), 0o600); err != nil {
		return false, "", err
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, "", err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return false, "", err
	}
	if binPath != "" && strings.HasPrefix(binPath, tmp) {
		binPath = filepath.Join(dir, strings.TrimPrefix(binPath, tmp))
	}
	s.recordInstall(ctx, name, version, digest, binPath)
	return true, digest, nil
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
func (s *Service) recordInstall(ctx context.Context, typ, version, digest, binPath string) {
	if _, err := s.st.DB().ExecContext(ctx, `INSERT INTO connector_installs (type, version, digest, bin_path, installed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (type, version) DO UPDATE SET digest = excluded.digest, bin_path = excluded.bin_path, installed_at = excluded.installed_at`,
		typ, version, digest, binPath, s.now().UTC().Format(time.RFC3339)); err != nil {
		s.log.Warn("connector install row", "connector", typ, "version", version, "error", err)
	}
}

// removeVersion deletes one package folder and its install row.
func (s *Service) removeVersion(ctx context.Context, typ, version string) {
	if err := os.RemoveAll(home.PackageDir(typ, version)); err != nil {
		s.log.Warn("remove connector version", "connector", typ, "version", version, "error", err)
	}
	if _, err := s.st.DB().ExecContext(ctx, `DELETE FROM connector_installs WHERE type = ? AND version = ?`, typ, version); err != nil {
		s.log.Warn("connector install row", "connector", typ, "version", version, "error", err)
	}
}

// installedVersions lists the complete package folders of each type.
func installedVersions() map[string][]string {
	out := map[string][]string{}
	types, err := os.ReadDir(home.PackagesDir())
	if err != nil {
		return out
	}
	for _, t := range types {
		if !t.IsDir() || strings.HasPrefix(t.Name(), ".") {
			continue
		}
		vers, err := os.ReadDir(filepath.Join(home.PackagesDir(), t.Name()))
		if err != nil {
			continue
		}
		for _, v := range vers {
			if packageDigest(t.Name(), v.Name()) != "" {
				out[t.Name()] = append(out[t.Name()], v.Name())
			}
		}
	}
	return out
}

// VersionRow is one catalog version of a connector type.
type VersionRow struct {
	Version     string   `json:"version"`
	PublishedAt string   `json:"published_at,omitempty"`
	Yanked      bool     `json:"yanked"`
	Installed   bool     `json:"installed"`
	Current     bool     `json:"current"`
	Connections []string `json:"connections"` // slugs of the connections of the type on this version
}

// Versions lists the catalog versions of the type of a connection, newest
// first, with the installed flag and the connections that use each one.
func (s *Service) Versions(ctx context.Context, ref string) ([]VersionRow, error) {
	if s.catalog == nil {
		return nil, ErrNoCatalog
	}
	c, err := s.Lookup(ref)
	if err != nil {
		return nil, err
	}
	vers, err := s.catalog.Versions(ctx, c.Type)
	if err != nil {
		return nil, err
	}
	installed := installedVersions()[c.Type]
	byVersion := map[string][]string{}
	for _, o := range s.snapshot() {
		if o.Type == c.Type && o.From == FromCatalog {
			byVersion[o.Version] = append(byVersion[o.Version], o.Slug)
		}
	}
	out := make([]VersionRow, 0, len(vers))
	for i := len(vers) - 1; i >= 0; i-- {
		v := vers[i]
		out = append(out, VersionRow{
			Version: v.Version, PublishedAt: v.PublishedAt, Yanked: v.Yanked,
			Installed:   slices.Contains(installed, v.Version),
			Current:     c.From == FromCatalog && c.Version == v.Version,
			Connections: orEmptySlice(byVersion[v.Version]),
		})
	}
	return out, nil
}

// UpdateResult is one connection that CheckUpdates found behind.
type UpdateResult struct {
	ID      string `json:"id"`
	Slug    string `json:"slug"`
	From    string `json:"from"`
	To      string `json:"to"`
	Applied bool   `json:"applied"`
	Error   string `json:"error,omitempty"`
}

// CheckUpdates finds the newest catalog version on the track of each catalog
// connection. The patch track applies it at once. The minor and latest
// tracks record it and emit connector.update.available. An empty ref checks
// every connection.
func (s *Service) CheckUpdates(ctx context.Context, ref string) ([]UpdateResult, error) {
	if s.catalog == nil {
		return nil, ErrNoCatalog
	}
	conns := s.snapshot()
	if ref != "" {
		c, err := s.Lookup(ref)
		if err != nil {
			return nil, err
		}
		conns = []Connection{c}
	}
	out := []UpdateResult{}
	for _, c := range conns {
		if c.From != FromCatalog || c.track() == manifest.TrackExact {
			continue
		}
		next, err := s.pickVersion(ctx, c.Type, "", c.Version, c.track())
		if err != nil {
			if ref != "" {
				return nil, err
			}
			s.log.Warn("connector update check", "connector", c.ID, "error", err)
			continue
		}
		if next == "" {
			s.setUpdate(c.ID, "")
			continue
		}
		res := UpdateResult{ID: c.ID, Slug: c.Slug, From: c.Version, To: next}
		if c.track() == manifest.TrackPatch {
			if err := s.Update(ctx, c.ID, next); err != nil {
				res.Error = err.Error()
			} else {
				res.Applied = true
			}
		} else if s.setUpdate(c.ID, next) {
			s.appendBus(ctx, "connector.update.available", c.ID, map[string]string{"version": next, "current": c.Version, "slug": c.Slug})
		}
		out = append(out, res)
	}
	return out, nil
}

// setUpdate records the available version of a connection. It reports
// whether the value changed.
func (s *Service) setUpdate(id, version string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updates[id] == version {
		return false
	}
	if version == "" {
		delete(s.updates, id)
	} else {
		s.updates[id] = version
	}
	return true
}

// availableUpdate returns the recorded update of a connection, or "".
func (s *Service) availableUpdate(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updates[id]
}

// Update installs a version side by side, moves the version and the digest
// of the connection to it, and restarts the process. An empty version is
// the newest version that is not yanked.
func (s *Service) Update(ctx context.Context, ref, version string) error {
	if s.catalog == nil {
		return ErrNoCatalog
	}
	if err := s.GuardReserved(ref); err != nil {
		return err
	}
	c, err := s.Lookup(ref)
	if err != nil {
		return err
	}
	if c.From != FromCatalog {
		return &ValidationError{Name: c.Slug, Fields: []FieldError{{Field: "from", Message: "only a catalog connector has versions"}}}
	}
	version, err = s.pickVersion(ctx, c.Type, version, "", "")
	if err != nil {
		return err
	}
	created, digest, err := s.installVersion(ctx, c.Type, version)
	if err != nil {
		return err
	}
	from := c.Version
	if err := s.setVersion(ctx, c.ID, version, digest); err != nil {
		if created {
			s.removeVersion(ctx, c.Type, version)
		}
		return err
	}
	s.setUpdate(c.ID, "")
	s.appendBus(ctx, "connector.updated.version", c.ID, map[string]string{"from": from, "to": version, "slug": c.Slug})
	if _, err := s.GC(ctx, false); err != nil {
		s.log.Warn("connector gc", "error", err)
	}
	return nil
}

// setVersion writes a new version and digest to a connection record.
func (s *Service) setVersion(ctx context.Context, id, version, digest string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.checkLoaded(id); err != nil {
		return err
	}
	old, err := s.Lookup(id)
	if err != nil {
		return err
	}
	c := old.clone()
	c.Version, c.Digest = version, digest
	return s.commit(ctx, c, &old, nil)
}

// GC removes each package folder that no connection uses. With dryRun it
// only lists them as type@version.
func (s *Service) GC(ctx context.Context, dryRun bool) ([]string, error) {
	used := map[string]bool{}
	for _, c := range s.snapshot() {
		if c.From == FromCatalog {
			used[c.Type+"@"+c.Version] = true
		}
	}
	removed := []string{}
	installed := installedVersions()
	for _, typ := range slices.Sorted(maps.Keys(installed)) {
		for _, v := range installed[typ] {
			if used[typ+"@"+v] {
				continue
			}
			removed = append(removed, typ+"@"+v)
			if !dryRun {
				s.removeVersion(ctx, typ, v)
			}
		}
		if !dryRun {
			_ = os.Remove(filepath.Join(home.PackagesDir(), typ)) // only an empty folder goes
		}
	}
	return removed, nil
}

// appendBus appends one connector domain event keyed by the connection id.
func (s *Service) appendBus(ctx context.Context, topic, id string, attrs map[string]string) {
	if _, err := s.st.Append(ctx, store.BusEvent{
		Topic: topic, Entity: "connector", EntityID: id, Action: strings.TrimPrefix(topic, "connector."), Attrs: attrs,
	}); err != nil {
		s.log.Warn("bus append", "topic", topic, "error", err)
	}
}

// unpackSnapshot unpacks a catalog snapshot into dir and checks that its
// manifest is the one of name at version.
func unpackSnapshot(snap []byte, dir, name, version string) (manifest.Manifest, error) {
	if err := untarGz(snap, dir); err != nil {
		return manifest.Manifest{}, fmt.Errorf("unpack %s@%s: %w", name, version, err)
	}
	m, err := readManifest(dir)
	if err != nil {
		return manifest.Manifest{}, withLayer(err, name, LayerCatalog)
	}
	if m.Name != name || m.Version != version {
		return manifest.Manifest{}, fmt.Errorf("unpack %s@%s: the snapshot holds %s@%s", name, version, m.Name, m.Version)
	}
	if err := m.Validate(); err != nil {
		return manifest.Manifest{}, withLayer(err, name, LayerCatalog)
	}
	return m, nil
}
