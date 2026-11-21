package connectors_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/trebi-ai/trebi/internal/connectors"
	"github.com/trebi-ai/trebi/internal/connectors/ghfake"
	"github.com/trebi-ai/trebi/internal/home"
	"github.com/trebi-ai/trebi/internal/registry"
	"github.com/trebi-ai/trebi/internal/store"
)

// fakeCatalog serves snapshots built from a manifest template, and a
// program archive of each version when binary is set.
type fakeCatalog struct {
	versions []registry.IndexVersion
	files    func(version string) map[string]string
	binary   func(version, platform string) ([]byte, error)
}

func (f fakeCatalog) Versions(context.Context, string) ([]registry.IndexVersion, error) {
	return f.versions, nil
}

func (f fakeCatalog) Snapshot(_ context.Context, _, version string) ([]byte, error) {
	return tarGz(f.files(version))
}

func (f fakeCatalog) Binary(_ context.Context, _, version, platform string) ([]byte, error) {
	if f.binary == nil {
		return nil, registry.ErrNotFound
	}
	return f.binary(version, platform)
}

func TestInstallUpdateGC(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	ctx := context.Background()
	st, svc := installStack(t, "demo", nil)

	c, err := svc.Install(ctx, connectors.InstallRequest{Name: "demo", Version: "1.0.0", Track: "patch"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := svc.Resolve(c.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home.PackageDir("demo", "1.0.0"), "bin", "demo-cli")
	if c.Slug != "demo" || r.Version != "1.0.0" || r.BinDir() != filepath.Dir(bin) || r.SkillDir() == "" {
		t.Fatalf("resolved = %+v", r)
	}
	if out, err := exec.Command(bin).Output(); err != nil || strings.TrimSpace(string(out)) != "demo-cli 1.0.0" {
		t.Fatalf("bin = %q %v", out, err)
	}
	var rows int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM connector_installs WHERE type = 'demo' AND version = '1.0.0' AND digest LIKE 'sha256:%'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("install rows = %d %v", rows, err)
	}
	if topicCount(t, st, "connector.installed") != 1 {
		t.Fatal("connector.installed not emitted")
	}
	if _, err := svc.Install(ctx, connectors.InstallRequest{Name: "demo", Slug: "demo"}); !errors.Is(err, connectors.ErrExists) {
		t.Fatalf("install with a used slug = %v", err)
	}

	// A second account of the type shares the package. Its delete keeps the
	// package that the first connection still uses.
	second, err := svc.Install(ctx, connectors.InstallRequest{Name: "demo", Version: "1.0.0", Track: "exact", Title: "Work"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Slug != "demo-2" || second.ID == c.ID || second.Title != "Work" {
		t.Fatalf("second = %+v", second)
	}
	if err := svc.Delete(ctx, "demo-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("delete removed a used package: %v", err)
	}

	vers, err := svc.Versions(ctx, "demo")
	if err != nil || len(vers) != 5 || vers[0].Version != "2.0.0-beta.1" || !vers[4].Current || !vers[4].Installed {
		t.Fatalf("versions = %+v %v", vers, err)
	}

	// The patch track applies 1.0.1 and removes 1.0.0.
	res, err := svc.CheckUpdates(ctx, "")
	if err != nil || len(res) != 1 || !res[0].Applied || res[0].To != "1.0.1" {
		t.Fatalf("check = %+v %v", res, err)
	}
	if e, _ := svc.Lookup("demo"); e.Version != "1.0.1" {
		t.Fatalf("connection version = %s", e.Version)
	}
	if _, err := os.Stat(home.PackageDir("demo", "1.0.0")); !os.IsNotExist(err) {
		t.Fatalf("old version stays: %v", err)
	}

	// The minor track records 1.1.0 and does not apply it.
	track := "minor"
	if _, err := svc.Patch(ctx, "demo", connectors.ConnectionPatch{Track: &track}); err != nil {
		t.Fatal(err)
	}
	res, err = svc.CheckUpdates(ctx, "demo")
	if err != nil || len(res) != 1 || res[0].Applied || res[0].To != "1.1.0" {
		t.Fatalf("minor check = %+v %v", res, err)
	}
	row, _ := svc.Get("demo")
	if u, _ := row["update"].(connectors.Pending); u.Version != "1.1.0" {
		t.Fatalf("update = %v", row["update"])
	}
	if topicCount(t, st, "connector.update.available") != 1 {
		t.Fatal("connector.update.available not emitted")
	}
	found := false
	for _, f := range svc.Doctor(ctx) {
		found = found || f.Code == "connector_update_available"
	}
	if !found {
		t.Fatal("no connector_update_available finding")
	}

	// Update with no version takes the newest version that is not yanked
	// and not a pre-release.
	if err := svc.Update(ctx, "demo", ""); err != nil {
		t.Fatal(err)
	}
	if e, _ := svc.Lookup("demo"); e.Version != "1.1.0" {
		t.Fatalf("updated version = %s", e.Version)
	}
	row, _ = svc.Get("demo")
	if row["update"] != nil {
		t.Fatalf("update after update = %v", row["update"])
	}
	if topicCount(t, st, "connector.updated.version") != 2 {
		t.Fatal("connector.updated.version not emitted")
	}
	if left, err := svc.GC(ctx, true); err != nil || len(left) != 0 {
		t.Fatalf("gc = %v %v", left, err)
	}
}

func TestInstallChecksumRollback(t *testing.T) {
	ctx := context.Background()
	_, svc := installStack(t, "demo", map[string]bool{"1.1.0": true})
	if _, err := svc.Install(ctx, connectors.InstallRequest{Name: "demo"}); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("install = %v", err)
	}
	if _, err := svc.Lookup("demo"); err == nil {
		t.Fatal("a failed install must not create the connection")
	}
	if entries, _ := os.ReadDir(home.ConnectionsDir()); len(entries) != 0 {
		t.Fatalf("connection left behind: %v", entries)
	}
	entries, _ := os.ReadDir(filepath.Join(home.PackagesDir(), "demo"))
	if len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
	if _, err := svc.Install(ctx, connectors.InstallRequest{Name: "demo", Version: "9.9.9"}); !errors.Is(err, connectors.ErrNotFound) {
		t.Fatalf("missing version = %v", err)
	}
}

func TestInstallSnapshotNameMismatch(t *testing.T) {
	_, svc := installStack(t, "other", nil)
	if _, err := svc.Install(context.Background(), connectors.InstallRequest{Name: "demo", Version: "1.0.0"}); err == nil || !strings.Contains(err.Error(), "holds other@1.0.0") {
		t.Fatalf("install = %v", err)
	}
}

func TestInstallFromCatalog(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	ctx := context.Background()
	_, _, svc := openStack(t)
	var asked string
	svc.WithInstall(connectors.InstallConfig{Catalog: fakeCatalog{
		versions: []registry.IndexVersion{{Version: "1.0.0"}},
		files: func(v string) map[string]string {
			files := demoFiles("demo")(v)
			m := files["trebi-connector.yaml"]
			start, end := strings.Index(m, "install:"), strings.Index(m, "  bin:")
			files["trebi-connector.yaml"] = m[:start] + "install:\n  catalog: true\n" + m[end:]
			return files
		},
		binary: func(v, platform string) ([]byte, error) {
			asked = platform
			return tarGz(map[string]string{"demo-cli": "#!/bin/sh\necho demo-cli " + v + "\n"})
		},
	}})
	c, err := svc.Install(ctx, connectors.InstallRequest{Name: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := svc.Resolve(c.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if asked != runtime.GOOS+"/"+runtime.GOARCH {
		t.Fatalf("platform = %q", asked)
	}
	out, err := exec.Command(filepath.Join(r.BinDir(), "demo-cli")).Output()
	if err != nil || strings.TrimSpace(string(out)) != "demo-cli 1.0.0" {
		t.Fatalf("bin = %q %v", out, err)
	}
}

// TestInstallReleaseThroughAPI reads a release that the plain link does not
// give through the API, and keeps the files next to the program.
func TestInstallReleaseThroughAPI(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	ctx := context.Background()
	gh := ghfake.New()
	defer gh.Close()
	asset := fmt.Sprintf("demo-cli_1.0.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	data, err := ghfake.Tarball("demo-cli_1.0.0", map[string]ghfake.File{
		"demo-cli":    {Body: "#!/bin/sh\necho demo-cli 1.0.0\n", Exec: true},
		"demo-helper": {Body: "#!/bin/sh\n", Exec: true},
		"NOTICE":      {Body: "notice"},
		"docs/README": {Body: "other folder"},
	}, gzip.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	gh.Release("acme/demo", "v1.0.0", map[string][]byte{asset: data, "SHA256SUMS": []byte(hex.EncodeToString(sum[:]) + "  " + asset + "\n")})
	missing := httptest.NewServer(http.NotFoundHandler())
	defer missing.Close()
	_, _, svc := openStack(t)
	svc.WithInstall(connectors.InstallConfig{
		Catalog:   fakeCatalog{versions: []registry.IndexVersion{{Version: "1.0.0"}}, files: demoFiles("demo")},
		GitHubURL: missing.URL,
		GitHubAPI: gh.API.URL,
	})
	c, err := svc.Install(ctx, connectors.InstallRequest{Name: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := svc.Resolve(c.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	for name, exec := range map[string]bool{"demo-cli": true, "demo-helper": true, "NOTICE": false} {
		fi, err := os.Stat(filepath.Join(r.BinDir(), name))
		if err != nil || (fi.Mode()&0o111 != 0) != exec {
			t.Fatalf("%s: %v %v", name, fi, err)
		}
	}
	if _, err := os.Stat(filepath.Join(r.BinDir(), "README")); !os.IsNotExist(err) {
		t.Fatalf("a file of another folder is in bin: %v", err)
	}
}

func TestInstallFromLink(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go")
	}
	ctx := context.Background()
	st, svc, gh := linkStack(t)
	sha := gh.Push("acme/tools", "main", sourceFiles(t, "buildable", "vitally/"))
	link := "https://github.com/acme/tools/tree/main/vitally"

	c := installLink(t, svc, link, "")
	if n := tarballCount(gh); n != 1 {
		t.Fatalf("a reused check fetched again: %d downloads", n)
	}
	pkg := home.PackageDir("vitally", "0.1.0+g"+sha[:12])
	digest, err := os.ReadFile(filepath.Join(pkg, ".digest"))
	if err != nil {
		t.Fatal(err)
	}
	if c.From != connectors.FromGitHub || c.Source == nil || c.Source.Commit != sha || c.Source.Path != "vitally" ||
		c.Version != "0.1.0" || c.Digest != strings.TrimSpace(string(digest)) || c.Track != "" {
		t.Fatalf("connection = %+v", c)
	}
	if out, err := exec.Command(filepath.Join(pkg, "bin", "vitally"), "--version").Output(); err != nil || !strings.Contains(string(out), "0.1.0") {
		t.Fatalf("program = %q %v", out, err)
	}
	var attrs string
	if err := st.DB().QueryRow(`SELECT attrs_json FROM event_log WHERE topic = 'connector.installed'`).Scan(&attrs); err != nil ||
		!strings.Contains(attrs, `"source":"github"`) || !strings.Contains(attrs, `"repo":"acme/tools"`) || !strings.Contains(attrs, sha) {
		t.Fatalf("installed attrs = %s %v", attrs, err)
	}
	if topicCount(t, st, "connector.install.step") < 7 {
		t.Fatal("too few connector.install.step events")
	}
	row, err := svc.Get("vitally")
	if err != nil || row["publisher"] != connectors.PublisherUnverified {
		t.Fatalf("row = %v %v", row["publisher"], err)
	}
	if src, _ := row["source"].(map[string]any); src["commit"] != sha || src["url"] != "https://github.com/acme/tools/tree/"+sha+"/vitally" {
		t.Fatalf("row source = %v", row["source"])
	}

	// A check id that does not exist checks again.
	if _, err := svc.Install(ctx, connectors.InstallRequest{Source: &connectors.SourceInstall{URL: link, CheckID: "chk_gone"}, Slug: "vitally-2"}); err != nil {
		t.Fatal(err)
	}
	if n := tarballCount(gh); n != 2 {
		t.Fatalf("an unknown check did not fetch again: %d downloads", n)
	}
	if err := svc.Delete(ctx, "vitally-2"); err != nil {
		t.Fatal(err)
	}

	// A link to the commit is pinned on purpose: no update check.
	pinned := installLink(t, svc, "acme/tools/vitally@"+sha, "vitally-pinned")
	if pinned.Source.Ref != sha {
		t.Fatalf("pinned ref = %q", pinned.Source.Ref)
	}

	// Restore: the missing package comes back from the pinned commit.
	if err := os.RemoveAll(pkg); err != nil {
		t.Fatal(err)
	}
	if err := svc.EnsurePackage(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(pkg, "bin", "vitally")); err != nil {
		t.Fatalf("restore: %v", err)
	}
	// A commit whose tree changed fails the digest.
	changed := sourceFiles(t, "buildable", "vitally/")
	changed["vitally/README.md"] = ghfake.File{Body: "changed\n"}
	gh.Rewrite(sha, changed)
	if err := os.RemoveAll(pkg); err != nil {
		t.Fatal(err)
	}
	if err := svc.EnsurePackage(ctx, c); !errors.Is(err, connectors.ErrDigestMismatch) {
		t.Fatalf("changed tree = %v", err)
	}
	gh.Rewrite(sha, sourceFiles(t, "buildable", "vitally/"))
	if err := svc.EnsurePackage(ctx, c); err != nil {
		t.Fatal(err)
	}

	// A new commit on the branch is recorded, never applied. No catalog is
	// configured, and that blocks nothing.
	next := sourceFiles(t, "buildable", "vitally/")
	next["vitally/trebi-connector.yaml"] = ghfake.File{Body: strings.Replace(next["vitally/trebi-connector.yaml"].Body, "version: 0.1.0", "version: 0.2.0", 1)}
	next["vitally/main.go"] = ghfake.File{Body: strings.Replace(next["vitally/main.go"].Body, "vitally 0.1.0", "vitally 0.2.0", 1)}
	head := gh.Push("acme/tools", "main", next)
	res, err := svc.CheckUpdates(ctx, "")
	if err != nil || len(res) != 1 || res[0].Slug != "vitally" || res[0].To != "0.2.0" || res[0].Commit != head || res[0].Applied {
		t.Fatalf("check = %+v %v", res, err)
	}
	if e, _ := svc.Lookup("vitally"); e.Source.Commit != sha {
		t.Fatal("an update applied on its own")
	}
	row, _ = svc.Get("vitally")
	if u, _ := row["update"].(connectors.Pending); u.Version != "0.2.0" || u.Commit != head {
		t.Fatalf("update = %v", row["update"])
	}
	if topicCount(t, st, "connector.update.available") != 1 {
		t.Fatal("connector.update.available not emitted")
	}

	// Update moves the connection; GC removes nothing that the pinned
	// connection still uses.
	if err := svc.Update(ctx, "vitally", ""); err != nil {
		t.Fatal(err)
	}
	e, _ := svc.Lookup("vitally")
	if e.Source.Commit != head || e.Version != "0.2.0" {
		t.Fatalf("updated = %+v", e)
	}
	if _, err := os.Stat(home.PackageDir("vitally", "0.2.0+g"+head[:12])); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pkg); err != nil {
		t.Fatalf("GC removed the package of the pinned connection: %v", err)
	}
	if err := svc.Delete(ctx, "vitally-pinned"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pkg); !os.IsNotExist(err) {
		t.Fatalf("old package stays: %v", err)
	}
}

// TestPatchVersion moves a connection to the version that the patch gives.
// The version replaces the old one; it never takes the build suffix of the
// old commit.
func TestPatchVersion(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	ctx := context.Background()
	st, svc := installStack(t, "demo", nil)
	if _, err := svc.Install(ctx, connectors.InstallRequest{Name: "demo", Version: "1.0.0", Track: "exact"}); err != nil {
		t.Fatal(err)
	}
	v := "1.0.1"
	if _, err := svc.Patch(ctx, "demo", connectors.ConnectionPatch{Version: &v}); err != nil {
		t.Fatal(err)
	}
	r, err := svc.Resolve("demo", "")
	if err != nil || r.Version != "1.0.1" {
		t.Fatalf("resolved = %+v %v", r, err)
	}
	digest, err := os.ReadFile(filepath.Join(home.PackageDir("demo", "1.0.1"), ".digest"))
	if e, _ := svc.Lookup("demo"); err != nil || e.Digest != strings.TrimSpace(string(digest)) {
		t.Fatalf("digest = %s %v", e.Digest, err)
	}
	if topicCount(t, st, "connector.updated.version") != 1 {
		t.Fatal("connector.updated.version not emitted")
	}
}

func TestPatchVersionLink(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go")
	}
	ctx := context.Background()
	_, svc, gh := linkStack(t)
	sha := gh.Push("acme/tools", "main", sourceFiles(t, "buildable", "vitally/"))
	installLink(t, svc, "https://github.com/acme/tools/tree/main/vitally", "")
	next := sourceFiles(t, "buildable", "vitally/")
	next["vitally/trebi-connector.yaml"] = ghfake.File{Body: strings.Replace(next["vitally/trebi-connector.yaml"].Body, "version: 0.1.0", "version: 0.2.0", 1)}
	next["vitally/main.go"] = ghfake.File{Body: strings.Replace(next["vitally/main.go"].Body, "vitally 0.1.0", "vitally 0.2.0", 1)}
	head := gh.Push("acme/tools", "main", next)

	// The current version is no change.
	same := "0.1.0+g" + sha[:12]
	if _, err := svc.Patch(ctx, "vitally", connectors.ConnectionPatch{Version: &same}); err != nil {
		t.Fatal(err)
	}
	if e, _ := svc.Lookup("vitally"); e.Source.Commit != sha || e.Version != "0.1.0" {
		t.Fatalf("same version moved = %+v", e)
	}

	// A version that the branch head does not hold is a validation error.
	bad := "0.3.0"
	if _, err := svc.Patch(ctx, "vitally", connectors.ConnectionPatch{Version: &bad}); !errors.Is(err, connectors.ErrInvalid) {
		t.Fatalf("version not at head = %v", err)
	}
	if e, _ := svc.Lookup("vitally"); e.Source.Commit != sha || e.Version != "0.1.0" {
		t.Fatalf("bad version moved = %+v", e)
	}

	// The package name of the head moves the link to the head commit.
	want := "0.2.0+g" + head[:12]
	if _, err := svc.Patch(ctx, "vitally", connectors.ConnectionPatch{Version: &want}); err != nil {
		t.Fatal(err)
	}
	e, _ := svc.Lookup("vitally")
	if e.Source.Commit != head || e.Version != "0.2.0" {
		t.Fatalf("patched = %+v", e)
	}
	r, err := svc.Resolve("vitally", "")
	if err != nil || r.Dir != home.PackageDir("vitally", want) {
		t.Fatalf("resolved = %+v %v", r, err)
	}
}

func TestInstallFromLinkConformance(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go")
	}
	ctx := context.Background()
	_, svc, gh := linkStack(t)
	files := sourceFiles(t, "buildable", "")
	files["trebi-connector.yaml"] = ghfake.File{Body: strings.Replace(files["trebi-connector.yaml"].Body, "vitally --version", "vitally --nope", 1)}
	sha := gh.Push("acme/vitally", "main", files)
	req := connectors.InstallRequest{Source: &connectors.SourceInstall{URL: "acme/vitally"}}

	_, err := svc.Install(ctx, req)
	var ce *connectors.InstallCheckError
	if !errors.As(err, &ce) || ce.Reason != connectors.ReasonConformanceFailed {
		t.Fatalf("install = %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Join(home.PackagesDir(), "vitally")); len(ents) != 0 {
		t.Fatalf("left behind: %v", ents)
	}
	if _, err := svc.Lookup("vitally"); err == nil {
		t.Fatal("a failed install created the connection")
	}
	req.Force = true
	c, err := svc.Install(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if c.Source.Commit != sha || c.Source.Ref != "" {
		t.Fatalf("forced = %+v", c.Source)
	}

	bad := []connectors.InstallRequest{
		{Name: "vitally", Source: req.Source},
		{Source: req.Source, Track: "minor"},
		{Name: "vitally", Force: true},
	}
	for _, r := range bad {
		var ve *connectors.ValidationError
		if _, err := svc.Install(ctx, r); !errors.As(err, &ve) {
			t.Fatalf("install %+v = %v", r, err)
		}
	}
	files["trebi-connector.yaml"] = ghfake.File{Body: "name: x\n"}
	gh.Push("acme/broken", "main", files)
	if _, err := svc.Install(ctx, connectors.InstallRequest{Source: &connectors.SourceInstall{URL: "acme/broken"}}); !errors.As(err, &ce) || ce.Reason != connectors.ReasonCheckFailed {
		t.Fatalf("broken = %v", err)
	}
}

func tarGz(files map[string]string) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// releaseHost serves demo-cli release assets. A version in bad gets a
// checksum that does not match.
func releaseHost(t *testing.T, bad map[string]bool) *httptest.Server {
	t.Helper()
	asset := func(v string) (string, []byte) {
		name := fmt.Sprintf("demo-cli_%s_%s_%s.tar.gz", v, runtime.GOOS, runtime.GOARCH)
		b, err := tarGz(map[string]string{"demo-cli_" + v + "/demo-cli": "#!/bin/sh\necho demo-cli " + v + "\n"})
		if err != nil {
			t.Fatal(err)
		}
		return name, b
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, "/acme/demo/releases/download/v")
		if !ok {
			http.NotFound(w, r)
			return
		}
		v, file, _ := strings.Cut(rest, "/")
		name, data := asset(v)
		switch file {
		case name:
			_, _ = w.Write(data)
		case "SHA256SUMS":
			sum := sha256.Sum256(data)
			if bad[v] {
				sum = sha256.Sum256([]byte("other"))
			}
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), name)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func demoFiles(name string) func(string) map[string]string {
	return func(v string) map[string]string {
		return map[string]string{
			"trebi-connector.yaml": `schema: trebi-connector/1
name: ` + name + `
title: Demo
description: A demo CLI.
version: ` + v + `
install:
  github_release:
    repo: acme/demo
    tag: v{version}
    asset: demo-cli_{version}_{os}_{arch}.tar.gz
    checksums: SHA256SUMS
  bin: demo-cli
  version_command: demo-cli --version
actions:
  cli:
    commands: [demo-cli]
`,
			"skill/SKILL.md": "---\nname: " + name + "\ndescription: Use demo-cli.\n---\n\nRun demo-cli.\n",
		}
	}
}

func installStack(t *testing.T, name string, bad map[string]bool) (*store.Store, *connectors.Service) {
	t.Helper()
	st, _, svc := openStack(t)
	srv := releaseHost(t, bad)
	svc.WithInstall(connectors.InstallConfig{
		Catalog: fakeCatalog{
			versions: []registry.IndexVersion{{Version: "1.0.0"}, {Version: "1.0.1"}, {Version: "1.1.0"}, {Version: "1.2.0", Yanked: true}, {Version: "2.0.0-beta.1"}},
			files:    demoFiles(name),
		},
		GitHubURL: srv.URL,
	})
	return st, svc
}

func topicCount(t *testing.T, st *store.Store, topic string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM event_log WHERE topic = ?`, topic).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// installLink checks the buildable fixture at vitally/ on main and installs
// it with the check id.
func installLink(t *testing.T, svc *connectors.Service, link, slug string) connectors.Connection {
	t.Helper()
	ctx := context.Background()
	chk, err := svc.CheckSource(ctx, link)
	if err != nil || !chk.CanInstall {
		t.Fatalf("check = %+v %v", chk, err)
	}
	c, err := svc.Install(ctx, connectors.InstallRequest{Source: &connectors.SourceInstall{URL: link, CheckID: chk.ID}, Slug: slug})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home.ConnectorStagingDir(chk.ID)); !os.IsNotExist(err) {
		t.Fatalf("staging folder stays: %v", err)
	}
	return c
}

func tarballCount(gh *ghfake.Server) int {
	n := 0
	for _, r := range gh.Requests() {
		if strings.Contains(r.Path, "/tarball/") {
			n++
		}
	}
	return n
}

func TestDeleteClearsConnectorDoctorState(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go")
	}
	ctx := context.Background()
	_, svc, gh := linkStack(t)
	sha := gh.Push("acme/tools", "main", sourceFiles(t, "buildable", "vitally/"))
	c := installLink(t, svc, "https://github.com/acme/tools/tree/main/vitally", "")

	changed := sourceFiles(t, "buildable", "vitally/")
	changed["vitally/README.md"] = ghfake.File{Body: "changed\n"}
	gh.Rewrite(sha, changed)
	if err := os.RemoveAll(home.PackageDir("vitally", "0.1.0+g"+sha[:12])); err != nil {
		t.Fatal(err)
	}
	svc.EnsurePackages(ctx)
	hasMismatch := func() bool {
		for _, f := range svc.Doctor(ctx) {
			if f.Connector == c.ID && f.Code == "connector_digest_mismatch" {
				return true
			}
		}
		return false
	}
	if !hasMismatch() {
		t.Fatal("doctor has no digest mismatch finding")
	}
	if err := svc.Delete(ctx, "vitally"); err != nil {
		t.Fatal(err)
	}
	if hasMismatch() {
		t.Fatal("doctor keeps a finding of a deleted connection")
	}
}
