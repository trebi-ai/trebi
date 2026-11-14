package update

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// GitHub reads the newest release of a repository.
type GitHub struct {
	http   Doer
	apiURL string
}

// NewGitHub builds a lookup over the releases/latest URL apiURL.
func NewGitHub(h Doer, apiURL string) *GitHub { return &GitHub{http: h, apiURL: apiURL} }

// Latest returns the newest release with its .dmg URL and digest. A release
// without a .dmg returns an empty DMGURL.
func (g *GitHub) Latest(ctx context.Context) (Release, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	body, err := g.get(ctx, g.apiURL, "application/vnd.github+json")
	if err != nil {
		return Release{}, err
	}
	err = json.NewDecoder(body).Decode(&rel)
	_ = body.Close()
	if err != nil {
		return Release{}, fmt.Errorf("github latest: %w", err)
	}
	r := Release{Tag: rel.TagName}
	var dmgName, sumsURL string
	for _, a := range rel.Assets {
		switch {
		case IsDMGAsset(a.Name):
			dmgName, r.DMGURL = a.Name, a.URL
		case a.Name == "SHA256SUMS":
			sumsURL = a.URL
		}
	}
	if r.DMGURL == "" || sumsURL == "" {
		r.DMGURL = ""
		return r, nil
	}
	sums, err := g.get(ctx, sumsURL, "text/plain")
	if err != nil {
		return Release{}, err
	}
	defer sums.Close()
	r.SHA256 = findSum(sums, dmgName)
	if r.SHA256 == "" {
		r.DMGURL = ""
	}
	return r, nil
}

func (g *GitHub) get(ctx context.Context, url, accept string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "trebi-update")
	req.Header.Set("Accept", accept)
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("github %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

// SystemDisk mounts with hdiutil and copies with ditto.
type SystemDisk struct{}

func (SystemDisk) Attach(ctx context.Context, dmg, mountpoint string) error {
	return run(ctx, "hdiutil", "attach", "-nobrowse", "-readonly", "-noautoopen", "-noverify", "-mountpoint", mountpoint, dmg)
}

func (SystemDisk) Detach(ctx context.Context, mountpoint string) error {
	if err := run(ctx, "hdiutil", "detach", mountpoint); err != nil {
		return run(ctx, "hdiutil", "detach", "-force", mountpoint)
	}
	return nil
}

// Copy keeps links, modes, and extended attributes, which a signature needs.
func (SystemDisk) Copy(ctx context.Context, src, dst string) error {
	return run(ctx, "ditto", src, dst)
}

// SystemSigner runs codesign.
type SystemSigner struct{}

func (SystemSigner) Verify(ctx context.Context, app string) (string, error) {
	if err := run(ctx, "codesign", "--verify", "--deep", "--strict", app); err != nil {
		return "", err
	}
	out, err := exec.CommandContext(ctx, "codesign", "-dv", "--verbose=2", app).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("codesign -dv: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return parseTeamID(string(out)), nil
}

// DaemonAPI calls the /v1/system routes of the local daemon. base is read on
// each call, because the daemon port can change after a restart.
type DaemonAPI struct {
	base func() string
	http Doer
}

// NewDaemonAPI builds the client.
func NewDaemonAPI(base func() string, h Doer) *DaemonAPI { return &DaemonAPI{base: base, http: h} }

func (d *DaemonAPI) Health(ctx context.Context) (Health, error) {
	resp, err := d.do(ctx, http.MethodGet, "/v1/system/health")
	if err != nil {
		return Health{}, err
	}
	defer resp.Body.Close()
	var body struct {
		Version    string `json:"version"`
		DrainState struct {
			RunningCount int `json:"running_count"`
		} `json:"drain_state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Health{}, err
	}
	return Health{Version: body.Version, RunningCount: body.DrainState.RunningCount}, nil
}

func (d *DaemonAPI) Drain(ctx context.Context) error   { return d.post(ctx, "/v1/system/drain") }
func (d *DaemonAPI) Undrain(ctx context.Context) error { return d.post(ctx, "/v1/system/undrain") }

func (d *DaemonAPI) post(ctx context.Context, path string) error {
	resp, err := d.do(ctx, http.MethodPost, path)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (d *DaemonAPI) do(ctx context.Context, method, path string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	req, err := http.NewRequestWithContext(ctx, method, d.base()+path, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	resp.Body = cancelBody{resp.Body, cancel}
	return resp, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelBody) Close() error {
	defer b.cancel()
	return b.ReadCloser.Close()
}

// findSum returns the digest of name in the text of a SHA256SUMS file.
func findSum(r io.Reader, name string) string {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return f[0]
		}
	}
	return ""
}

// parseTeamID reads the TeamIdentifier line of codesign -dv output.
func parseTeamID(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if id, ok := strings.CutPrefix(strings.TrimSpace(line), "TeamIdentifier="); ok {
			return id
		}
	}
	return ""
}

func run(ctx context.Context, name string, args ...string) error {
	if out, err := exec.CommandContext(ctx, name, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %s: %w", name, strings.TrimSpace(string(out)), err)
	}
	return nil
}
