package conformance

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/manifest"
	"github.com/trebi-ai/trebi/internal/procgroup"
)

// versionWait bounds install.version_command.
const versionWait = 10 * time.Second

// installSuite checks the install block. It does not install: it checks
// that a method can work here, that bin is on the version PATH, and that
// version_command prints the version.
type installSuite struct{}

func (installSuite) name() string { return SuiteInstall }

// applies includes a Trebi entry with no install block, which gets a warn.
// An online MCP server with no events has nothing to install.
func (installSuite) applies(m manifest.Manifest) bool {
	online := m.Events == nil && m.Actions != nil && m.Actions.CLI == nil && m.Actions.MCP != nil && m.Actions.MCP.URL != ""
	return m.Install != nil || (m.Publisher == manifest.PublisherTrebi && !online)
}

func (installSuite) run(ctx context.Context, r *run) []Result {
	m := r.t.Manifest
	in := m.Install
	if in == nil {
		return []Result{{Suite: SuiteInstall, Check: "method", Status: StatusWarn,
			Message: "a Trebi entry with no install block: the user must put the program on the PATH"}}
	}
	out := []Result{InstallMethod(m)}
	if in.Bin == "" {
		out = append(out, Result{Suite: SuiteInstall, Check: "bin", Status: StatusSkip, Message: "no install.bin"})
	} else if r.lookPath(in.Bin) == "" {
		out = append(out, row(SuiteInstall, "bin", fmt.Errorf("the command %q is not on the version PATH (<dir>/bin, then PATH)", in.Bin)))
	} else {
		out = append(out, row(SuiteInstall, "bin", nil))
	}
	if in.VersionCommand == "" {
		return append(out, Result{Suite: SuiteInstall, Check: "version", Status: StatusSkip, Message: "no install.version_command"})
	}
	return append(out, row(SuiteInstall, "version", r.version(ctx)))
}

// InstallMethod checks that one declared method can work on this platform.
func InstallMethod(m manifest.Manifest) Result {
	in := m.Install
	platform := runtime.GOOS + "/" + runtime.GOARCH
	onPlatform := len(m.Platforms) == 0 || slices.Contains(m.Platforms, platform)
	have := func(tool string) bool { _, err := exec.LookPath(tool); return err == nil }
	methods := []struct {
		name     string
		declared bool
		works    bool
		why      string
	}{
		{"catalog", in.Catalog, onPlatform, "no program for " + platform},
		{"github_release", in.GitHubRelease != nil, onPlatform, "no program for " + platform},
		{"build", in.Build != nil, have("go"), "go is not installed"},
		{"brew", in.Brew != "", have("brew"), "brew is not installed"},
		{"go", in.Go != "", have("go"), "go is not installed"},
	}
	var why []string
	for _, mt := range methods {
		switch {
		case !mt.declared:
		case mt.works:
			return row(SuiteInstall, "method", nil)
		default:
			why = append(why, mt.name+": "+mt.why)
		}
	}
	if len(why) == 0 {
		return Result{Suite: SuiteInstall, Check: "method", Status: StatusSkip, Message: "no install method"}
	}
	return row(SuiteInstall, "method", fmt.Errorf("no install method works on %s: %s", platform, strings.Join(why, "; ")))
}

// version runs install.version_command with the clean env and checks that
// it prints the manifest version.
func (r *run) version(ctx context.Context) error {
	d, err := r.folders()
	if err != nil {
		return err
	}
	vctx, cancel := context.WithTimeout(ctx, versionWait)
	defer cancel()
	cmdline := r.t.Manifest.Install.VersionCommand
	out, err := procgroup.Command{Script: cmdline, Dir: r.t.Dir, Env: r.env(d, nil)}.Run(vctx, 4096)
	if err != nil {
		if vctx.Err() != nil {
			return fmt.Errorf("%s: no exit in %s", cmdline, versionWait)
		}
		return fmt.Errorf("%s: %w: %s", cmdline, err, strings.TrimSpace(string(out)))
	}
	if !strings.Contains(string(out), r.t.Manifest.Version) {
		return fmt.Errorf("%s prints %q, not %s", cmdline, strings.TrimSpace(string(out)), r.t.Manifest.Version)
	}
	return nil
}
