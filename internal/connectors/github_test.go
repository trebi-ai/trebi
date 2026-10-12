package connectors

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trebi-ai/trebi/internal/connectors/ghfake"
)

func TestParseGitHubLink(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("ab", 20)
	cases := []struct {
		in      string
		repo    string
		ref     string
		path    string
		refPath []string
	}{
		{"https://github.com/o/r", "o/r", "", "", nil},
		{"github.com/o/r.git", "o/r", "", "", nil},
		{"https://github.com/o/r/tree/main/vitally-cli", "o/r", "", "", []string{"main", "vitally-cli"}},
		{"https://github.com/o/r/tree/main/a/b/", "o/r", "", "", []string{"main", "a", "b"}},
		{"https://github.com/o/r/blob/main/a/trebi-connector.yaml", "o/r", "", "", []string{"main", "a"}},
		{"o/r/a/b@v1.2.0", "o/r", "v1.2.0", "a/b", nil},
		{"https://github.com/o/r/tree/" + sha + "/a", "o/r", "", "", []string{sha, "a"}},
		{"https://github.com/o/r#readme", "o/r", "", "", nil},
	}
	for _, tc := range cases {
		src, refPath, err := ParseGitHubLink(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if src.Repo != tc.repo || src.Ref != tc.ref || src.Path != tc.path || src.Commit != "" || !slices.Equal(refPath, tc.refPath) {
			t.Fatalf("%s: %+v %v", tc.in, src, refPath)
		}
	}
	for _, bad := range []string{
		"", "https://gitlab.com/o/r", "https://gist.github.com/o/abc", "https://github.com/o/r/tree/main/../x",
		"https://github.com/o/r?tab=readme", "https://github.com/o", "https://github.com/-o-/r", "https://github.com/o/r/issues/1",
		"o/r/../x", "ftp://github.com/o/r", "o/r/a@../x",
	} {
		_, _, err := ParseGitHubLink(bad)
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Fields[0].Field != "url" {
			t.Fatalf("%q: want a url validation error, got %v", bad, err)
		}
	}
}

func TestGitHubSourceURL(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("c", 40)
	for _, tc := range []struct {
		src  GitHubSource
		want string
	}{
		{GitHubSource{Repo: "o/r"}, "https://github.com/o/r"},
		{GitHubSource{Repo: "o/r", Path: "a"}, "https://github.com/o/r/tree/HEAD/a"},
		{GitHubSource{Repo: "o/r", Ref: "main", Path: "a"}, "https://github.com/o/r/tree/main/a"},
		{GitHubSource{Repo: "o/r", Ref: "main", Path: "a", Commit: sha}, "https://github.com/o/r/tree/" + sha + "/a"},
	} {
		if got := tc.src.URL(); got != tc.want {
			t.Fatalf("%+v: %s", tc.src, got)
		}
	}
}

// fakeLogin is a GitHubLogin with a fixed token that counts Forget.
type fakeLogin struct {
	token  atomic.Value
	forget atomic.Int32
}

func (l *fakeLogin) Token(context.Context) (string, error) { return l.token.Load().(string), nil }
func (l *fakeLogin) Forget()                               { l.forget.Add(1) }

func TestGitHubResolveSlashBranch(t *testing.T) {
	t.Parallel()
	gh := ghfake.New()
	defer gh.Close()
	sha := gh.Push("o/r", "feature/x", map[string]ghfake.File{"tool/a.txt": {Body: "a"}})
	c := NewGitHubClient(nil, gh.API.URL, nil)
	src, refPath, err := ParseGitHubLink("https://github.com/o/r/tree/feature/x/tool")
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Resolve(context.Background(), src, refPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ref != "feature/x" || got.Path != "tool" || got.Commit != sha {
		t.Fatalf("resolved %+v", got)
	}
	if _, err := c.Resolve(context.Background(), src, []string{"nope", "a"}); !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("missing ref: %v", err)
	}
}

func TestGitHubFetchFolder(t *testing.T) {
	t.Parallel()
	gh := ghfake.New()
	defer gh.Close()
	files := map[string]ghfake.File{
		"README.md":           {Body: "root"},
		"tool/main.go":        {Body: "package main"},
		"tool/run.sh":         {Body: "#!/bin/sh", Exec: true},
		"tool/docs/x.md":      {Body: "x"},
		"tool/link":           {Link: "docs/x.md"},
		"other/secret.txt":    {Body: "no"},
		"toolbox/not-me.txt":  {Body: "no"},
		"tool/sub/deeper.txt": {Body: "d"},
	}
	sha := gh.Push("o/r", "main", files)
	c := NewGitHubClient(nil, gh.API.URL, nil)
	src := GitHubSource{Repo: "o/r", Ref: "main", Path: "tool", Commit: sha}
	dir := t.TempDir()
	digest, err := c.Fetch(context.Background(), src, dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	want := []string{"docs/x.md", "link", "main.go", "run.sh", "sub/deeper.txt"}
	if !slices.Equal(got, want) {
		t.Fatalf("files %v, want %v", got, want)
	}
	if st, _ := os.Stat(filepath.Join(dir, "run.sh")); st.Mode()&0o100 == 0 {
		t.Fatalf("run.sh lost the exec bit: %v", st.Mode())
	}
	// The same files with another compression give the same digest.
	gh.Compression(gzip.BestCompression)
	dir2 := t.TempDir()
	digest2, err := c.Fetch(context.Background(), src, dir2)
	if err != nil || digest2 != digest || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("digest %s vs %s: %v", digest, digest2, err)
	}
	// A folder that is not in the tree is not found.
	if _, err := c.Fetch(context.Background(), GitHubSource{Repo: "o/r", Path: "nope", Commit: sha}, t.TempDir()); !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("missing folder: %v", err)
	}
}

func TestUntarRefusals(t *testing.T) {
	t.Parallel()
	cases := map[string]map[string]ghfake.File{
		"traversal": {"a/../../x": {Body: "x"}},
		"absolute":  {"/etc/x": {Body: "x"}},
		"link out":  {"a/l": {Link: "../../etc/passwd"}},
		"link abs":  {"a/l": {Link: "/etc/passwd"}},
	}
	for name, files := range cases {
		data, err := ghfake.Tarball("top", files, gzip.DefaultCompression)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := untarGz(bytes.NewReader(data), t.TempDir(), "*/", untarLimits{}); err == nil {
			t.Fatalf("%s: want an error", name)
		}
	}
	many := map[string]ghfake.File{}
	for i := range 5 {
		many[fmt.Sprintf("f%d", i)] = ghfake.File{Body: "12345"}
	}
	data, _ := ghfake.Tarball("top", many, gzip.DefaultCompression)
	if _, err := untarGz(bytes.NewReader(data), t.TempDir(), "*/", untarLimits{entries: 4}); !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("entries: %v", err)
	}
	if _, err := untarGz(bytes.NewReader(data), t.TempDir(), "*/", untarLimits{size: 20}); !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("size: %v", err)
	}
	r := &capReader{r: bytes.NewReader(data), max: 10}
	if _, err := untarGz(r, t.TempDir(), "*/", untarLimits{}); !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("download: %v", err)
	}
}

func TestGitHubToken(t *testing.T) {
	t.Parallel()
	gh := ghfake.New()
	defer gh.Close()
	sha := gh.Push("o/priv", "main", map[string]ghfake.File{"a.txt": {Body: "a"}})
	gh.Private("o/priv", "tok")

	// No login: a private repository is not found, and the message names
	// the GitHub CLI login.
	anon := NewGitHubClient(nil, gh.API.URL, nil)
	_, err := anon.Resolve(context.Background(), GitHubSource{Repo: "o/priv", Ref: "main"}, nil)
	var se *SourceError
	if !errors.As(err, &se) || !errors.Is(err, ErrSourceNotFound) || se.Token || !strings.Contains(se.Message, "GitHub CLI") {
		t.Fatalf("anon: %v", err)
	}
	for _, r := range gh.Requests() {
		if r.Auth != "" {
			t.Fatalf("a token went with no login: %+v", r)
		}
	}

	// With a login, the token goes to the API and the codeload host only.
	login := newFakeLogin("tok")
	c := NewGitHubClient(nil, gh.API.URL, login)
	if _, err := c.Fetch(context.Background(), GitHubSource{Repo: "o/priv", Commit: sha}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	reqs := gh.Requests()
	last := reqs[len(reqs)-1]
	if last.Auth != "Bearer tok" || !strings.Contains(last.Path, "legacy.tar.gz") {
		t.Fatalf("codeload request %+v", last)
	}
	other, _ := url.Parse(gh.Codeload.URL)
	other.Host = "localhost:" + other.Port()
	if tok := c.tokenFor(context.Background(), other); tok != "" {
		t.Fatalf("the token goes to %s", other)
	}

	// A 401 forgets the login and tries once more with the fresh token.
	swap := &swapLogin{fakeLogin: newFakeLogin("stale"), next: "tok"}
	c = NewGitHubClient(nil, gh.API.URL, swap)
	got, err := c.Resolve(context.Background(), GitHubSource{Repo: "o/priv", Ref: "main"}, nil)
	if err != nil || got.Commit != sha || swap.forget.Load() != 1 {
		t.Fatalf("retry: %+v %v forget=%d", got, err, swap.forget.Load())
	}
}

// swapLogin gives the next token after Forget.
type swapLogin struct {
	*fakeLogin
	next string
}

func (l *swapLogin) Forget() {
	l.fakeLogin.Forget()
	l.token.Store(l.next)
}

func TestGitHubRateLimit(t *testing.T) {
	t.Parallel()
	gh := ghfake.New()
	defer gh.Close()
	gh.RateLimit(true)
	c := NewGitHubClient(nil, gh.API.URL, nil)
	_, err := c.Resolve(context.Background(), GitHubSource{Repo: "o/r"}, nil)
	var se *SourceError
	if !errors.As(err, &se) || !errors.Is(err, ErrSourceRateLimited) || se.RetryAfter <= 0 {
		t.Fatalf("rate limit: %v", err)
	}
}

func TestGitHubManifestAndHead(t *testing.T) {
	t.Parallel()
	gh := ghfake.New()
	defer gh.Close()
	first := gh.Push("o/r", "main", map[string]ghfake.File{"tool/trebi-connector.yaml": {Body: "version: 0.1.0"}})
	second := gh.Push("o/r", "main", map[string]ghfake.File{"tool/trebi-connector.yaml": {Body: "version: 0.2.0"}})
	c := NewGitHubClient(nil, gh.API.URL, nil)
	src := GitHubSource{Repo: "o/r", Ref: "main", Path: "tool", Commit: first}
	head, err := c.Head(context.Background(), src)
	if err != nil || head != second {
		t.Fatalf("head %s %v", head, err)
	}
	b, err := c.Manifest(context.Background(), src, head)
	if err != nil || string(b) != "version: 0.2.0" {
		t.Fatalf("manifest %q %v", b, err)
	}
}

// TestStagedCheckExpiry proves a check is reused only before it expires and
// only for its commit.
func TestStagedCheckExpiry(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	sha := strings.Repeat("ab", 20)
	s := &Service{checks: map[string]staged{}, now: func() time.Time { return now }}
	s.checks["chk_1"] = staged{dir: t.TempDir(), check: SourceCheck{ID: "chk_1", Source: GitHubSource{Repo: "a/b", Commit: sha}, ExpiresAt: now.Add(checkTTL)}}
	if _, ok := s.stagedCheck("chk_1", sha); !ok {
		t.Fatal("a live check is not reused")
	}
	if _, ok := s.stagedCheck("chk_1", strings.Repeat("cd", 20)); ok {
		t.Fatal("a check of another commit is reused")
	}
	now = now.Add(checkTTL + time.Second)
	if _, ok := s.stagedCheck("chk_1", sha); ok {
		t.Fatal("an expired check is reused")
	}
}

func newFakeLogin(tok string) *fakeLogin {
	l := &fakeLogin{}
	l.token.Store(tok)
	return l
}
