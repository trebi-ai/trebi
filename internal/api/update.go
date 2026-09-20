package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// latestCache holds one GitHub /releases/latest lookup so the SPA can poll
// /v1/system/update without burning the unauthenticated 60/hour quota.
type latestCache struct {
	mu      sync.Mutex
	tag     string
	url     string
	err     error
	fetched time.Time
}

const (
	githubLatestURL    = "https://api.github.com/repos/trebi-ai/trebi/releases/latest"
	githubLatestTTL    = time.Hour
	githubLatestErrTTL = time.Minute
)

func (s *Server) latestRelease(ctx context.Context) (tag, url string, err error) {
	if s.lookupLatest != nil {
		return s.lookupLatest()
	}
	return s.latest.get(ctx, func(ctx context.Context) (string, string, error) {
		return lookupGitHubLatest(ctx, &http.Client{Timeout: 5 * time.Second}, githubLatestURL)
	})
}

func (c *latestCache) get(ctx context.Context, fetch func(context.Context) (string, string, error)) (string, string, error) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fresh(now) {
		return c.tag, c.url, c.err
	}
	tag, url, err := fetch(ctx)
	if err != nil && ctx.Err() != nil {
		return tag, url, err
	}
	c.tag, c.url, c.err, c.fetched = tag, url, err, now
	return tag, url, err
}

func (c *latestCache) fresh(now time.Time) bool {
	if c.fetched.IsZero() {
		return false
	}
	ttl := githubLatestTTL
	if c.err != nil {
		ttl = githubLatestErrTTL
	}
	return now.Sub(c.fetched) < ttl
}

func lookupGitHubLatest(ctx context.Context, client *http.Client, apiURL string) (tag, url string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "trebi/"+Version)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("github latest: %s", resp.Status)
	}
	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", "", err
	}
	want := proAssetName(rel.TagName, runtime.GOOS, runtime.GOARCH)
	for _, a := range rel.Assets {
		if a.Name == want {
			return rel.TagName, a.BrowserDownloadURL, nil
		}
	}
	return rel.TagName, "", nil
}

func proAssetName(tag, goos, goarch string) string {
	ver := strings.TrimPrefix(tag, "v")
	return "trebi_" + ver + "_" + goos + "_" + goarch + ".tar.gz"
}

// versionNewer reports whether latest's major.minor.patch triple is greater
// than current's. Non-semver current (dev, a git describe) is never newer,
// so a development binary does not claim an update.
func versionNewer(latest, current string) bool {
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
