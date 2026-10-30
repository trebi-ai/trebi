package agentcli

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/trebi-ai/trebi/internal/harness"
)

// versionHTTPTimeout bounds one release lookup.
const versionHTTPTimeout = 10 * time.Second

// commandRunner runs bin with args and returns the combined output.
type commandRunner func(ctx context.Context, bin string, args ...string) ([]byte, error)

func execCommand(ctx context.Context, bin string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// VersionSources gives the harness.VersionSource of each harness that Trebi
// can update. The zero value uses the network and the real programs.
type VersionSources struct {
	HTTP *http.Client
	Run  commandRunner
}

var _ harness.VersionSources = VersionSources{}

// VendorBin is the program on the PATH, not the kept copy in the bin store.
func (v VersionSources) VendorBin(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}

// Source returns the version source of name.
func (v VersionSources) Source(name string) (harness.VersionSource, bool) {
	run := v.Run
	if run == nil {
		run = execCommand
	}
	client := v.HTTP
	if client == nil {
		client = &http.Client{Timeout: versionHTTPTimeout}
	}
	src := vendorVersions{name: name, run: run}
	switch name {
	case Claude:
		src.latest = func(ctx context.Context) (string, error) {
			var tags struct {
				Latest string `json:"latest"`
			}
			err := getJSON(ctx, client, "https://registry.npmjs.org/-/package/@anthropic-ai/claude-code/dist-tags", &tags)
			return tags.Latest, err
		}
		src.update = []string{"update"}
		src.off = map[string]string{"DISABLE_AUTOUPDATER": "1", "DISABLE_UPDATES": "1"}
	case Codex:
		src.latest = githubLatest(client, "openai/codex")
		src.update = []string{"update"}
	case Grok:
		src.latest = func(ctx context.Context) (string, error) {
			bin := v.VendorBin(Grok)
			if bin == "" {
				return "", harness.ErrNotInstalled
			}
			out, err := run(ctx, bin, "update", "--check", "--json")
			if err != nil {
				return "", err
			}
			var check struct {
				Latest string `json:"latestVersion"`
			}
			if err := json.Unmarshal(out, &check); err != nil {
				return "", fmt.Errorf("grok update --check: %w", err)
			}
			return check.Latest, nil
		}
		src.update = []string{"update"}
		src.install = func(version string) []string { return []string{"update", "--version", version} }
		src.off = map[string]string{"GROK_DISABLE_AUTOUPDATER": "1"}
	case OpenCode:
		src.latest = githubLatest(client, "sst/opencode")
		src.update = []string{"upgrade"}
		src.install = func(version string) []string { return []string{"upgrade", version} }
		src.off = map[string]string{"OPENCODE_DISABLE_AUTOUPDATE": "1"}
	default:
		return nil, false
	}
	if src.install != nil {
		return installingVersions{src}, true
	}
	return src, true
}

// vendorVersions drives the version commands of one vendor CLI.
type vendorVersions struct {
	name    string
	run     commandRunner
	latest  func(ctx context.Context) (string, error)
	update  []string
	install func(version string) []string
	off     map[string]string
}

func (s vendorVersions) Installed(ctx context.Context, bin string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := s.run(ctx, bin, "--version")
	if err != nil {
		return "", err
	}
	v := harness.ParseVersion(string(out))
	if v == "" {
		return "", fmt.Errorf("%s --version: no version in %q", s.name, strings.TrimSpace(string(out)))
	}
	return v, nil
}

func (s vendorVersions) Latest(ctx context.Context) (string, error) {
	v, err := s.latest(ctx)
	if err != nil {
		return "", err
	}
	if p := harness.ParseVersion(v); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("%s: no version in %q", s.name, v)
}

func (s vendorVersions) Update(ctx context.Context, bin string) error {
	_, err := s.run(ctx, bin, s.update...)
	return err
}

func (s vendorVersions) AutoUpdateOff() map[string]string { return maps.Clone(s.off) }

// installingVersions is a source that also installs a named version.
type installingVersions struct{ vendorVersions }

func (s installingVersions) Install(ctx context.Context, bin, version string) error {
	_, err := s.run(ctx, bin, s.install(version)...)
	return err
}

// githubLatest reads the tag of the latest GitHub release of repo.
func githubLatest(client *http.Client, repo string) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		var rel struct {
			Tag string `json:"tag_name"`
		}
		err := getJSON(ctx, client, "https://api.github.com/repos/"+repo+"/releases/latest", &rel)
		return rel.Tag, err
	}
}

func getJSON(ctx context.Context, client *http.Client, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	return nil
}

// autoUpdateOff is the env that stops name from updating itself in a run.
func autoUpdateOff(name string) map[string]string {
	src, ok := VersionSources{}.Source(name)
	if !ok {
		return nil
	}
	return src.AutoUpdateOff()
}
