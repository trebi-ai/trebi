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

func TestInstallUpdateGC(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	ctx := context.Background()
	st, svc := installStack(t, "demo", nil)

	r, err := svc.Install(ctx, connectors.InstallRequest{Name: "demo", Version: "1.0.0", Track: "patch"})
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home.ConnectorVersionDir("demo", "1.0.0"), "bin", "demo-cli")
	if r.Version != "1.0.0" || r.BinDir() != filepath.Dir(bin) || r.SkillDir() == "" {
		t.Fatalf("resolved = %+v", r)
	}
	if out, err := exec.Command(bin).Output(); err != nil || strings.TrimSpace(string(out)) != "demo-cli 1.0.0" {
		t.Fatalf("bin = %q %v", out, err)
	}
	var rows int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM connector_installs WHERE name = 'demo' AND version = '1.0.0' AND digest LIKE 'sha256:%'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("install rows = %d %v", rows, err)
	}
	if topicCount(t, st, "connector.installed") != 1 {
		t.Fatal("connector.installed not emitted")
	}
	if _, err := svc.Install(ctx, connectors.InstallRequest{Name: "demo"}); !errors.Is(err, connectors.ErrExists) {
		t.Fatalf("second install = %v", err)
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
	if e, _ := svc.Entry("demo"); e.Version != "1.0.1" {
		t.Fatalf("entry version = %s", e.Version)
	}
	if _, err := os.Stat(home.ConnectorVersionDir("demo", "1.0.0")); !os.IsNotExist(err) {
		t.Fatalf("old version stays: %v", err)
	}

	// The minor track records 1.1.0 and does not apply it.
	track := "minor"
	if err := svc.Patch(ctx, "demo", connectors.EntryPatch{Track: &track}); err != nil {
		t.Fatal(err)
	}
	res, err = svc.CheckUpdates(ctx, "demo")
	if err != nil || len(res) != 1 || res[0].Applied || res[0].To != "1.1.0" {
		t.Fatalf("minor check = %+v %v", res, err)
	}
	row, _ := svc.Get("demo")
	if u, _ := row["update"].(map[string]any); u["version"] != "1.1.0" {
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
	if err := svc.Update(ctx, "demo", "", ""); err != nil {
		t.Fatal(err)
	}
	if e, _ := svc.Entry("demo"); e.Version != "1.1.0" {
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
	if _, ok := svc.Entry("demo"); ok {
		t.Fatal("a failed install must not write the entry")
	}
	entries, _ := os.ReadDir(filepath.Join(home.ConnectorsDir(), "demo"))
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
	r, err := svc.Install(ctx, connectors.InstallRequest{Name: "demo"})
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
