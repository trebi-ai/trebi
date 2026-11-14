package update

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type mapDoer map[string]string

func (m mapDoer) Do(req *http.Request) (*http.Response, error) {
	body, ok := m[req.URL.String()]
	if !ok {
		return &http.Response{StatusCode: 404, Status: "404 Not Found", Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
}

const latestJSON = `{"tag_name":"v2.0.0","assets":[
 {"name":"trebi_2.0.0_darwin_arm64.tar.gz","browser_download_url":"https://dl.test/tar"},
 {"name":"Trebi-2.0.0-universal.dmg","browser_download_url":"https://dl.test/dmg"},
 {"name":"SHA256SUMS","browser_download_url":"https://dl.test/sums"}]}`

func TestGitHubLatest(t *testing.T) {
	t.Parallel()
	sums := "aaa  trebi_2.0.0_darwin_arm64.tar.gz\n" + strings.Repeat("b", 64) + "  Trebi-2.0.0-universal.dmg\n"
	g := NewGitHub(mapDoer{"https://api.test/latest": latestJSON, "https://dl.test/sums": sums}, "https://api.test/latest")
	r, err := g.Latest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if r.Tag != "v2.0.0" || r.DMGURL != "https://dl.test/dmg" || r.SHA256 != strings.Repeat("b", 64) {
		t.Fatalf("release %+v", r)
	}

	t.Run("dmg missing from SHA256SUMS", func(t *testing.T) {
		g := NewGitHub(mapDoer{"https://api.test/latest": latestJSON, "https://dl.test/sums": "aaa  other\n"}, "https://api.test/latest")
		r, err := g.Latest(t.Context())
		if err != nil || r.Tag != "v2.0.0" || r.DMGURL != "" {
			t.Fatalf("release %+v err %v", r, err)
		}
	})
	t.Run("no dmg asset", func(t *testing.T) {
		g := NewGitHub(mapDoer{"https://api.test/latest": `{"tag_name":"v2.0.0","assets":[]}`}, "https://api.test/latest")
		r, err := g.Latest(t.Context())
		if err != nil || r.DMGURL != "" {
			t.Fatalf("release %+v err %v", r, err)
		}
	})
	t.Run("http error", func(t *testing.T) {
		if _, err := NewGitHub(mapDoer{}, "https://api.test/latest").Latest(t.Context()); err == nil {
			t.Fatal("no error")
		}
	})
}

func TestParseTeamID(t *testing.T) {
	t.Parallel()
	out := "Executable=/x/Trebi\nIdentifier=ai.trebi.app\nAuthority=Developer ID Application: Ocral (Q56WK6TB88)\nTeamIdentifier=Q56WK6TB88\nSealed Resources version=2\n"
	if got := parseTeamID(out); got != "Q56WK6TB88" {
		t.Errorf("team %q", got)
	}
	if got := parseTeamID("TeamIdentifier=not set\n"); got != "not set" {
		t.Errorf("unsigned team %q", got)
	}
	if got := parseTeamID("nothing"); got != "" {
		t.Errorf("empty team %q", got)
	}
}

func TestIsDMGAsset(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"Trebi-0.3.0-universal.dmg":       true,
		"trebi_0.3.0_darwin_arm64.tar.gz": false,
		"Trebi.dmg":                       false,
		"SHA256SUMS":                      false,
	} {
		if got := IsDMGAsset(name); got != want {
			t.Errorf("IsDMGAsset(%q) = %v", name, got)
		}
	}
}

func TestDaemonAPI(t *testing.T) {
	t.Parallel()
	var drained []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/system/health":
			_, _ = io.WriteString(w, `{"version":"2.0.0","drain_state":{"draining":false,"running_count":3}}`)
		case "POST /v1/system/drain", "POST /v1/system/undrain":
			drained = append(drained, r.URL.Path)
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	d := NewDaemonAPI(func() string { return ts.URL }, ts.Client())
	ctx := context.Background()
	h, err := d.Health(ctx)
	if err != nil || h.Version != "2.0.0" || h.RunningCount != 3 {
		t.Fatalf("health %+v err %v", h, err)
	}
	if err := d.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.Undrain(ctx); err != nil {
		t.Fatal(err)
	}
	if len(drained) != 2 {
		t.Errorf("posts %v", drained)
	}
	bad := NewDaemonAPI(func() string { return ts.URL + "/nope" }, ts.Client())
	if _, err := bad.Health(ctx); err == nil {
		t.Error("404 health: no error")
	}
}
