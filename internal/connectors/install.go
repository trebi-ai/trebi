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

var (
	// ErrNoCatalog is an install or update with no Trebi catalog configured.
	ErrNoCatalog = errors.New("no connector catalog is configured")
	// ErrDigestMismatch is a package whose content is not the one that the
	// connection record pins.
	ErrDigestMismatch = errors.New("connector package digest mismatch")

	errNoRelease = errors.New("not found")
)

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

	GitHubAPI   string      // links; "" is https://api.github.com
	GitHubLogin GitHubLogin // nil: links use no login
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
	login := c.GitHubLogin
	if c.GitHubAPI != "" && c.GitHubAPI != defaultGitHubAPI {
		login = nil // a fixture server never gets the real login
	}
	s.gh = NewGitHubClient(c.HTTP, c.GitHubAPI, login)
	s.releaseAPI = c.GitHubURL == "" || c.GitHubAPI != ""
	return s
}

// InstallRequest adds one connection of a catalog type or of a GitHub link.
// Name is the catalog type; Source is the link. An empty slug is the default
// slug of the type.
type InstallRequest struct {
	Name    string            `json:"name,omitempty"`
	Source  *SourceInstall    `json:"source,omitempty"`
	Version string            `json:"version,omitempty"`
	Track   string            `json:"track,omitempty"`
	Inputs  map[string]string `json:"inputs,omitempty"`
	Slug    string            `json:"slug,omitempty"`
	Title   string            `json:"title,omitempty"`
	Force   bool              `json:"force,omitempty"` // install when a conformance suite fails; advanced only
	// Scope is the workspace ids that see the new connection. Empty uses
	// the default: Workspace for a connector that needs an account, else
	// global.
	Scope     Scope  `json:"scope,omitempty"`
	Workspace string `json:"workspace,omitempty"`
}

// SourceInstall is the link of an install. CheckID reuses the staging
// folder of a check.
type SourceInstall struct {
	URL     string `json:"url"`
	CheckID string `json:"check_id,omitempty"`
}

// Install reads one catalog version, installs its program into the package
// folder, creates the connection, and starts it (Part E.7). A failure
// removes the package folder that it made. A request with Source installs
// from a link.
func (s *Service) Install(ctx context.Context, req InstallRequest) (Connection, error) {
	if req.Source != nil || req.Force {
		return s.installLink(ctx, req)
	}
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
		Version: version, Digest: digest, Track: manifest.Track(req.Track), Inputs: req.Inputs, Scope: req.Scope,
	}.WithDefaultScope(req.Workspace))
	if err != nil {
		if created {
			s.removeVersion(ctx, req.Name, version)
		}
		return Connection{}, err
	}
	s.appendBus(ctx, "connector.installed", c.ID, map[string]string{"version": version, "type": c.Type, "slug": c.Slug})
	return c, nil
}

// EnsurePackage installs the missing package of a catalog or link
// connection and checks it against the digest in the record. A restore needs
// no user step.
func (s *Service) EnsurePackage(ctx context.Context, c Connection) error {
	if c.From == FromGitHub {
		return s.ensureLinkPackage(ctx, c)
	}
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
			return fmt.Errorf("%w: connector %q: the catalog package %s %s has digest %s, not %s", ErrDigestMismatch, c.Slug, c.Type, c.Version, digest, c.Digest)
		}
		return nil
	}
	if c.Digest != "" && have != c.Digest {
		return fmt.Errorf("%w: connector %q: the package %s %s has digest %s, not %s", ErrDigestMismatch, c.Slug, c.Type, c.Version, have, c.Digest)
	}
	return nil
}

// EnsurePackages runs EnsurePackage for each catalog or link connection
// whose package folder is missing. A failure is logged and kept for doctor;
// the row shows the resolve error.
func (s *Service) EnsurePackages(ctx context.Context) {
	for _, c := range s.snapshot() {
		if (c.From != FromCatalog && c.From != FromGitHub) || packageDigest(c.Type, c.packageVersion()) != "" {
			continue
		}
		err := s.EnsurePackage(ctx, c)
		s.mu.Lock()
		if err != nil {
			s.pkgErr[c.ID] = err
		} else {
			delete(s.pkgErr, c.ID)
		}
		s.mu.Unlock()
		if err != nil {
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
	binPath, err := s.installBinary(ctx, m, tmp, false)
	if err != nil {
		return false, "", fmt.Errorf("install %s@%s: %w", name, version, err)
	}
	sum := sha256.Sum256(snap)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if err := s.placePackage(ctx, name, version, tmp, digest, binPath); err != nil {
		return false, "", err
	}
	return true, digest, nil
}

// placePackage writes the digest into the complete temp folder tmp, renames
// it to the package folder of name at version, and records the install.
func (s *Service) placePackage(ctx context.Context, name, version, tmp, digest, binPath string) error {
	dir := home.PackageDir(name, version)
	if err := os.WriteFile(filepath.Join(tmp, digestFile), []byte(digest+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return err
	}
	if binPath != "" && strings.HasPrefix(binPath, tmp) {
		binPath = filepath.Join(dir, strings.TrimPrefix(binPath, tmp))
	}
	s.recordInstall(ctx, name, version, digest, binPath)
	return nil
}

// installBinary runs the first install method that works in the package
// folder dir and checks version_command. source reports whether dir holds
// the source of the program (a link), so build can run. It returns the path
// of the program, or "" when the manifest installs none.
func (s *Service) installBinary(ctx context.Context, m manifest.Manifest, dir string, source bool) (string, error) {
	in := m.Install
	if in == nil || in.Bin == "" {
		return "", nil
	}
	bin := filepath.Join(dir, "bin")
	var errs []error
	methods := []struct {
		name string
		ok   bool
		run  func() error
	}{
		{"catalog", in.Catalog, func() error { return s.installCatalog(ctx, m, bin) }},
		{"github_release", in.GitHubRelease != nil, func() error { return s.installRelease(ctx, m, bin) }},
		{"build", in.Build != nil, func() error {
			if !source {
				return errors.New("the catalog snapshot has no source")
			}
			return buildGo(ctx, *in.Build, dir, filepath.Join(bin, in.Bin))
		}},
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
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, lastLines(out, 1))
	}
	return nil
}

// buildTimeout bounds one install.build run. GOTOOLCHAIN=auto can download
// a Go toolchain on the first build.
const buildTimeout = 10 * time.Minute

// buildGo runs go build for install.build in the module folder of entry and
// writes the program to out.
func buildGo(ctx context.Context, b manifest.Build, entry, out string) error {
	if _, err := exec.LookPath("go"); err != nil {
		return errors.New("go is not installed")
	}
	mod, err := b.ModuleDir(entry)
	if err != nil {
		return err
	}
	pkg, err := filepath.Rel(mod, filepath.Join(entry, filepath.FromSlash(b.Go)))
	if err != nil {
		return err
	}
	bctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	cmd := exec.CommandContext(bctx, "go", "build", "-trimpath", "-o", out, "./"+filepath.ToSlash(pkg))
	cmd.Dir = mod
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=-mod=readonly", "GOTOOLCHAIN=auto", "GOWORK=off") // a go.work above the home must not take the build
	if output, err := cmd.CombinedOutput(); err != nil {
		if bctx.Err() != nil {
			return fmt.Errorf("go build: no exit in %s", buildTimeout)
		}
		return fmt.Errorf("go build: %w:\n%s", err, lastLines(output, 20))
	}
	return nil
}

// lastLines returns the last n lines of b.
func lastLines(b []byte, n int) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
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
	files, err := extractProgram(m.Install.Bin+".tar.gz", data, m.Install.Bin)
	if err != nil {
		return err
	}
	return files.write(bin)
}

// installRelease downloads the release asset of this platform, checks it
// against the checksums file, and writes the program into bin. A release
// that the plain link does not give, as in a private repository, comes
// through the API with the GitHub login.
func (s *Service) installRelease(ctx context.Context, m manifest.Manifest, bin string) error {
	gh := m.Install.GitHubRelease
	platform := runtime.GOOS + "/" + runtime.GOARCH
	if len(m.Platforms) > 0 && !slices.Contains(m.Platforms, platform) {
		return fmt.Errorf("no release for %s", platform)
	}
	tag := expandInstall(gh.Tag, m.Version)
	fetch := func(name string) ([]byte, error) {
		data, err := s.download(ctx, s.githubURL+"/"+gh.Repo+"/releases/download/"+tag+"/"+name)
		if errors.Is(err, errNoRelease) && s.releaseAPI {
			return s.gh.ReleaseAsset(ctx, gh.Repo, tag, name)
		}
		return data, err
	}
	asset := expandInstall(gh.Asset, m.Version)
	data, err := fetch(asset)
	if err != nil {
		return err
	}
	if gh.Checksums != "" {
		sums, err := fetch(expandInstall(gh.Checksums, m.Version))
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
	files, err := extractProgram(asset, data, m.Install.Bin)
	if err != nil {
		return err
	}
	return files.write(bin)
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
	switch resp.StatusCode {
	case http.StatusOK:
		return io.ReadAll(io.LimitReader(resp.Body, maxRelease))
	case http.StatusNotFound:
		return nil, fmt.Errorf("GET %s: %w", url, errNoRelease)
	}
	return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
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

// programFile is one file of the program folder of an asset.
type programFile struct {
	name string
	data []byte
	exec bool
}

// programFiles are the files that go into the bin folder.
type programFiles []programFile

func (p programFiles) write(bin string) error {
	if err := os.MkdirAll(bin, 0o700); err != nil {
		return err
	}
	for _, f := range p {
		mode := os.FileMode(0o644)
		if f.exec {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(bin, f.name), f.data, mode); err != nil {
			return err
		}
	}
	return nil
}

// extractProgram returns the files of the folder that holds bin in a tar.gz
// or zip asset, so a program keeps the helper files next to it. An asset
// that is not an archive is the program itself.
func extractProgram(asset string, data []byte, bin string) (programFiles, error) {
	a := archiveFiles{asset: asset, bin: bin}
	var err error
	switch {
	case strings.HasSuffix(asset, ".tar.gz"), strings.HasSuffix(asset, ".tgz"):
		err = a.readTar(data)
	case strings.HasSuffix(asset, ".zip"):
		err = a.readZip(data)
	default:
		return programFiles{{name: bin, data: data, exec: true}}, nil
	}
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(a.all, func(e archiveFile) bool { return e.name == bin })
	if i < 0 {
		return nil, fmt.Errorf("%s has no file %q", asset, bin)
	}
	var out programFiles
	for _, e := range a.all {
		if e.dir == a.all[i].dir && !strings.HasPrefix(e.name, "._") {
			out = append(out, e.programFile)
		}
	}
	return out, nil
}

// archiveFile is one regular file of an archive and its folder.
type archiveFile struct {
	dir string
	programFile
}

// archiveFiles reads the regular files of one asset within maxRelease and
// maxEntries.
type archiveFiles struct {
	asset string
	bin   string
	all   []archiveFile
	size  int64
}

func (a *archiveFiles) add(name string, mode os.FileMode, r io.Reader) error {
	b, err := io.ReadAll(io.LimitReader(r, maxRelease-a.size+1))
	if err != nil {
		return err
	}
	if a.size += int64(len(b)); a.size > maxRelease || len(a.all) >= maxEntries {
		return fmt.Errorf("%s is too large", a.asset)
	}
	dir, base := path.Split(name)
	a.all = append(a.all, archiveFile{dir, programFile{name: base, data: b, exec: mode&0o111 != 0 || base == a.bin}})
	return nil
}

func (a *archiveFiles) readTar(data []byte) error {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeReg {
			if err := a.add(hdr.Name, hdr.FileInfo().Mode(), tr); err != nil {
				return err
			}
		}
	}
}

func (a *archiveFiles) readZip(data []byte) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		if !f.FileInfo().Mode().IsRegular() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = a.add(f.Name, f.FileInfo().Mode(), rc)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
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
	c, err := s.Lookup(ref)
	if err != nil {
		return nil, err
	}
	if c.From == FromGitHub {
		return nil, &ValidationError{Name: c.Slug, Fields: []FieldError{{Field: "from", Message: "only a catalog connector has versions"}}}
	}
	if s.catalog == nil {
		return nil, ErrNoCatalog
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
	Commit  string `json:"commit,omitempty"` // the new commit of a link connection
	Applied bool   `json:"applied"`
	Error   string `json:"error,omitempty"`
}

// CheckUpdates finds the newest catalog version on the track of each catalog
// connection. The patch track applies it at once. The minor and latest
// tracks record it and emit connector.update.available. A link connection
// records a new commit of its branch and never applies it. An empty ref
// checks every connection.
func (s *Service) CheckUpdates(ctx context.Context, ref string) ([]UpdateResult, error) {
	conns := s.snapshot()
	if ref != "" {
		c, err := s.Lookup(ref)
		if err != nil {
			return nil, err
		}
		if c.From == FromCatalog && s.catalog == nil {
			return nil, ErrNoCatalog
		}
		conns = []Connection{c}
	}
	out := []UpdateResult{}
	for _, c := range conns {
		var res *UpdateResult
		var err error
		switch {
		case c.From == FromGitHub:
			res, err = s.checkLink(ctx, c)
		case c.From == FromCatalog && s.catalog != nil && c.track() != manifest.TrackExact:
			res, err = s.checkCatalog(ctx, c)
		}
		if err != nil {
			if ref != "" {
				return nil, err
			}
			s.log.Warn("connector update check", "connector", c.ID, "error", err)
			continue
		}
		if res != nil {
			out = append(out, *res)
		}
	}
	return out, nil
}

// checkCatalog finds the newest catalog version on the track of c. The
// patch track applies it.
func (s *Service) checkCatalog(ctx context.Context, c Connection) (*UpdateResult, error) {
	next, err := s.pickVersion(ctx, c.Type, "", c.Version, c.track())
	if err != nil {
		return nil, err
	}
	if next == "" {
		s.setUpdate(c.ID, Pending{})
		return nil, nil //nolint:nilnil // no update is not an error
	}
	res := UpdateResult{ID: c.ID, Slug: c.Slug, From: c.Version, To: next}
	if c.track() == manifest.TrackPatch {
		if err := s.Update(ctx, c.ID, next); err != nil {
			res.Error = err.Error()
		} else {
			res.Applied = true
		}
	} else if s.setUpdate(c.ID, Pending{Version: next}) {
		s.appendBus(ctx, "connector.update.available", c.ID, map[string]string{"version": next, "current": c.Version, "slug": c.Slug})
	}
	return &res, nil
}

// Pending is the update that a check found for a connection. Commit is set
// for a link connection.
type Pending struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
}

// PublisherUnverified is the publisher of a link connector, whatever its
// manifest says.
const PublisherUnverified = "unverified"

// setUpdate records the available update of a connection; the zero value
// clears it. It reports whether the value changed.
func (s *Service) setUpdate(id string, u Pending) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updates[id] == u {
		return false
	}
	if u == (Pending{}) {
		delete(s.updates, id)
	} else {
		s.updates[id] = u
	}
	return true
}

// availableUpdate returns the recorded update of a connection.
func (s *Service) availableUpdate(id string) Pending {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updates[id]
}

// Update installs a version side by side, moves the version and the digest
// of the connection to it, and restarts the process. An empty version is
// the newest version that is not yanked.
func (s *Service) Update(ctx context.Context, ref, version string) error {
	if err := s.GuardReserved(ref); err != nil {
		return err
	}
	c, err := s.Lookup(ref)
	if err != nil {
		return err
	}
	if c.From == FromGitHub {
		return s.updateLink(ctx, c)
	}
	if s.catalog == nil {
		return ErrNoCatalog
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
	if err := s.setVersion(ctx, c.ID, version, digest, nil); err != nil {
		if created {
			s.removeVersion(ctx, c.Type, version)
		}
		return err
	}
	s.setUpdate(c.ID, Pending{})
	s.appendBus(ctx, "connector.updated.version", c.ID, map[string]string{"from": from, "to": version, "slug": c.Slug})
	if _, err := s.GC(ctx, false); err != nil {
		s.log.Warn("connector gc", "error", err)
	}
	return nil
}

// setVersion writes a new version and digest to a connection record. A
// non-nil src also moves the link to a new commit.
func (s *Service) setVersion(ctx context.Context, id, version, digest string, src *GitHubSource) error {
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
	if src != nil {
		c.Source = src
	}
	return s.commit(ctx, c, &old, nil)
}

// GC removes each package folder that no connection uses and no holder
// keeps. With dryRun it only lists them as type@version.
func (s *Service) GC(ctx context.Context, dryRun bool) ([]string, error) {
	if !dryRun {
		s.SweepStaging()
	}
	holds, err := s.holds(ctx)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	for ref := range holds {
		used[ref] = true
	}
	for _, c := range s.snapshot() {
		if c.From == FromCatalog || c.From == FromGitHub {
			used[c.Type+"@"+c.packageVersion()] = true
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
	if _, err := untarGz(bytes.NewReader(snap), dir, "", untarLimits{size: maxRelease}); err != nil {
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
