package connectors

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Errors of a GitHub link. The API maps them to 404, 429, 502, and 413.
var (
	ErrSourceNotFound    = errors.New("source not found")
	ErrSourceRateLimited = errors.New("source rate limited")
	ErrSourceUnavailable = errors.New("source unavailable")
	ErrSourceTooLarge    = errors.New("source too large")
)

// Limits of one folder download.
const (
	maxArchive  = 64 << 20
	maxUnpacked = 256 << 20
	maxEntries  = 20000
	maxRefTries = 4
	maxManifest = 1 << 20
)

const defaultGitHubAPI = "https://api.github.com"

// EnvGitHubAPI points links at a fixture server. Only trebi-eval sets it.
const EnvGitHubAPI = "TREBI_EVAL_GITHUB_API"

var (
	ghOwnerRE  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	ghRepoRE   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	ghCommitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// ghLoginTTL is how long a gh token stays cached.
const ghLoginTTL = 10 * time.Minute

// GitHubSource is one folder of one GitHub repository at one commit.
type GitHubSource struct {
	Repo   string `yaml:"repo" json:"repo"`                     // owner/name
	Path   string `yaml:"path,omitempty" json:"path,omitempty"` // folder in the repo; "" is the root
	Ref    string `yaml:"ref,omitempty" json:"ref,omitempty"`   // branch or tag that the user gave; "" is the default branch
	Commit string `yaml:"commit" json:"commit"`                 // 40 hex; the pin
}

// ParseGitHubLink reads a GitHub link. Commit stays "". For a tree or blob
// link, the ref and the path are not clear until Resolve, so the second
// result holds their segments; it is nil for the other forms.
func ParseGitHubLink(s string) (GitHubSource, []string, error) {
	s = strings.TrimSpace(s)
	var src GitHubSource
	var refPath []string
	var msg string
	switch {
	case s == "":
		msg = "required"
	case strings.Contains(s, "://") || strings.HasPrefix(s, "github.com/") || strings.HasPrefix(s, "www.github.com/"):
		src, refPath, msg = parseURLLink(s)
	default:
		src, msg = parseShortLink(s)
	}
	if msg == "" {
		if err := src.Validate(); err != nil {
			msg = err.Error()
		}
	}
	if msg != "" {
		return GitHubSource{}, nil, &ValidationError{Name: "link", Fields: []FieldError{{Field: "url", Message: msg}}}
	}
	return src, refPath, nil
}

// Validate checks the form of the set fields.
func (g GitHubSource) Validate() error {
	owner, name, ok := strings.Cut(g.Repo, "/")
	switch {
	case !ok || !ghOwnerRE.MatchString(owner):
		return fmt.Errorf("owner %q is not a GitHub name", owner)
	case !ghRepoRE.MatchString(name) || name == "." || name == "..":
		return fmt.Errorf("repository %q is not a GitHub name", name)
	case g.Path != "" && !cleanRepoPath(g.Path):
		return fmt.Errorf("folder %q must be a clean path in the repository", g.Path)
	case strings.ContainsAny(g.Ref, " ~^:?*[\\") || strings.Contains(g.Ref, ".."):
		return fmt.Errorf("ref %q is not a git ref", g.Ref)
	case g.Commit != "" && !ghCommitRE.MatchString(g.Commit):
		return fmt.Errorf("commit %q must be 40 hex characters", g.Commit)
	}
	return nil
}

// URL is the web link of the folder: at the commit, else at the ref.
func (g GitHubSource) URL() string {
	u := "https://github.com/" + g.Repo
	at := g.Commit
	if at == "" {
		at = g.Ref
	}
	if at == "" && g.Path == "" {
		return u
	}
	if at == "" {
		at = "HEAD"
	}
	u += "/tree/" + at
	if g.Path != "" {
		u += "/" + g.Path
	}
	return u
}

// pinned reports whether the user linked a commit, so no update applies. An
// empty ref follows the default branch.
func (g GitHubSource) pinned() bool { return ghCommitRE.MatchString(g.Ref) }

// SourceError is a GitHub failure with the text for the user. Token reports
// whether the request used a login.
type SourceError struct {
	Kind       error
	Message    string
	Token      bool
	RetryAfter time.Duration
}

func (e *SourceError) Error() string { return e.Message }

func (e *SourceError) Unwrap() error { return e.Kind }

// GitHubLogin gives the token of the existing GitHub login, or "".
type GitHubLogin interface {
	Token(ctx context.Context) (string, error)
	Forget()
}

// GitHubClient reads commits and archives from the GitHub API.
type GitHubClient struct {
	http  *http.Client
	api   string
	login GitHubLogin // nil: no login
	log   *slog.Logger
}

// NewGitHubClient builds a client. An empty api is https://api.github.com;
// a nil http is a 10 min client.
func NewGitHubClient(c *http.Client, api string, login GitHubLogin) *GitHubClient {
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Minute}
	}
	// Redirects are followed by hand, so the token goes only to the hosts
	// that tokenFor allows.
	cl := *c
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	api = strings.TrimRight(api, "/")
	if api == "" {
		api = defaultGitHubAPI
	}
	return &GitHubClient{http: &cl, api: api, login: login, log: slog.Default().With("source", "github")}
}

// Resolve fills Ref, Path, and Commit. refPath holds the segments of a tree
// link; the shortest ref candidate that GitHub knows wins.
func (g *GitHubClient) Resolve(ctx context.Context, src GitHubSource, refPath []string) (GitHubSource, error) {
	if refPath == nil {
		sha, err := g.commit(ctx, src.Repo, src.Ref)
		if err != nil {
			return src, err
		}
		src.Commit = sha
		return src, nil
	}
	for i := 1; i <= len(refPath) && i <= maxRefTries; i++ {
		ref := strings.Join(refPath[:i], "/")
		sha, err := g.commit(ctx, src.Repo, ref)
		if errors.Is(err, ErrSourceNotFound) {
			continue
		}
		if err != nil {
			return src, err
		}
		src.Ref, src.Path, src.Commit = ref, strings.Join(refPath[i:], "/"), sha
		return src, src.Validate()
	}
	return src, g.notFound(ctx)
}

// Head returns the commit of src.Ref now.
func (g *GitHubClient) Head(ctx context.Context, src GitHubSource) (string, error) {
	return g.commit(ctx, src.Repo, src.Ref)
}

// Manifest reads trebi-connector.yaml of the folder at commit.
func (g *GitHubClient) Manifest(ctx context.Context, src GitHubSource, commit string) ([]byte, error) {
	p := "trebi-connector.yaml"
	if src.Path != "" {
		p = src.Path + "/" + p
	}
	resp, err := g.get(ctx, "/repos/"+src.Repo+"/contents/"+escapeRef(p)+"?ref="+url.QueryEscape(commit), "application/vnd.github.raw")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, maxManifest))
}

// Fetch downloads the archive of src.Commit and writes only src.Path into
// dir. It returns the digest of the folder.
func (g *GitHubClient) Fetch(ctx context.Context, src GitHubSource, dir string) (string, error) {
	if !ghCommitRE.MatchString(src.Commit) {
		return "", fmt.Errorf("fetch %s: no commit", src.Repo)
	}
	resp, err := g.get(ctx, "/repos/"+src.Repo+"/tarball/"+src.Commit, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body := &capReader{r: resp.Body, max: maxArchive}
	prefix := "*/"
	if src.Path != "" {
		prefix += src.Path + "/"
	}
	n, err := untarGz(body, dir, prefix, untarLimits{size: maxUnpacked, entries: maxEntries})
	if err != nil {
		if errors.Is(err, ErrSourceTooLarge) {
			return "", &SourceError{Kind: ErrSourceTooLarge, Message: "The folder is too large: " + err.Error()}
		}
		return "", err
	}
	if n == 0 {
		return "", g.notFound(ctx)
	}
	return treeDigest(dir)
}

// commit reads the sha of a ref; "" is the default branch.
func (g *GitHubClient) commit(ctx context.Context, repo, ref string) (string, error) {
	if ref == "" {
		ref = "HEAD"
	}
	resp, err := g.get(ctx, "/repos/"+repo+"/commits/"+escapeRef(ref), "application/vnd.github+json")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var body struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxManifest)).Decode(&body); err != nil {
		return "", fmt.Errorf("%w: commit %s: %v", ErrSourceUnavailable, ref, err)
	}
	if !ghCommitRE.MatchString(body.SHA) {
		return "", fmt.Errorf("%w: commit %s has no sha", ErrSourceUnavailable, ref)
	}
	return body.SHA, nil
}

// get runs one GET against the API. It follows redirects by hand, sends the
// token only where tokenFor allows, and retries once after a 401 with a
// fresh login.
func (g *GitHubClient) get(ctx context.Context, p, accept string) (*http.Response, error) {
	resp, used, err := g.try(ctx, g.api+p, accept)
	if err == nil && resp.StatusCode == http.StatusUnauthorized && used {
		resp.Body.Close()
		g.login.Forget()
		resp, used, err = g.try(ctx, g.api+p, accept)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) //nolint:errcheck // drain for reuse only
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusUnprocessableEntity:
		return nil, sourceNotFound(used)
	case (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) && rateLimited(resp.Header):
		wait := retryAfter(resp.Header, time.Now())
		return nil, &SourceError{Kind: ErrSourceRateLimited, Token: used, RetryAfter: wait,
			Message: fmt.Sprintf("GitHub limits the requests from this computer. Try again in %s.", wait.Round(time.Second))}
	case resp.StatusCode == http.StatusForbidden:
		return nil, sourceNotFound(used)
	}
	return nil, &SourceError{Kind: ErrSourceUnavailable, Token: used, Message: "GitHub is not available: " + resp.Status}
}

// try runs the request and its redirects. It reports whether a token went.
func (g *GitHubClient) try(ctx context.Context, target, accept string) (*http.Response, bool, error) {
	used := false
	for range 5 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, used, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if tok := g.tokenFor(ctx, req.URL); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
			used = true
		}
		resp, err := g.http.Do(req)
		if err != nil {
			return nil, used, err
		}
		loc := resp.Header.Get("Location")
		if resp.StatusCode < 300 || resp.StatusCode > 399 || loc == "" {
			return resp, used, nil
		}
		resp.Body.Close()
		next, err := req.URL.Parse(loc)
		if err != nil {
			return nil, used, err
		}
		target = next.String()
	}
	return nil, used, errors.New("too many redirects")
}

// tokenFor returns the login token for the API host and the codeload host,
// else "".
func (g *GitHubClient) tokenFor(ctx context.Context, u *url.URL) string {
	if g.login == nil {
		return ""
	}
	api, err := url.Parse(g.api)
	if err != nil {
		return ""
	}
	if u.Hostname() != api.Hostname() && u.Hostname() != "codeload.github.com" {
		return ""
	}
	tok, err := g.login.Token(ctx)
	if err != nil {
		g.log.Debug("github login", "err", err)
		return ""
	}
	return tok
}

// notFound is the not-found error with the login state of this client.
func (g *GitHubClient) notFound(ctx context.Context) error {
	return sourceNotFound(g.tokenFor(ctx, &url.URL{Host: hostOf(g.api)}) != "")
}

// capReader fails when more than max bytes come.
type capReader struct {
	r   io.Reader
	n   int64
	max int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if rest := c.max - c.n + 1; int64(len(p)) > rest {
		p = p[:rest] // one byte past max shows that the stream is too long
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.max {
		return 0, fmt.Errorf("%w: the download is over %d MiB", ErrSourceTooLarge, c.max>>20)
	}
	return n, err
}

// untarLimits bound one extract. A zero field is no limit.
type untarLimits struct {
	size    int64
	entries int
}

// GHLogin reads the token of the GitHub CLI login on this computer.
type GHLogin struct {
	mu    sync.Mutex
	token string
	at    time.Time
	now   func() time.Time
}

// NewGHLogin builds the login of the GitHub CLI.
func NewGHLogin() *GHLogin { return &GHLogin{now: time.Now} }

// Token runs gh auth token. A missing gh, a failure, or no output give ""
// and no error.
func (l *GHLogin) Token(ctx context.Context) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.at.IsZero() && l.now().Sub(l.at) < ghLoginTTL {
		return l.token, nil
	}
	l.token, l.at = ghToken(ctx), l.now()
	return l.token, nil
}

// Forget clears the cached token.
func (l *GHLogin) Forget() {
	l.mu.Lock()
	l.at = time.Time{}
	l.mu.Unlock()
}

// parseURLLink reads a github.com link. The last result is the problem, or
// "".
func parseURLLink(s string) (GitHubSource, []string, string) {
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	switch {
	case err != nil:
		return GitHubSource{}, nil, "is not a link"
	case u.Scheme != "https" && u.Scheme != "http":
		return GitHubSource{}, nil, "must be an https link"
	case !slices.Contains([]string{"github.com", "www.github.com"}, strings.ToLower(u.Hostname())):
		return GitHubSource{}, nil, "Trebi reads only github.com links"
	case u.RawQuery != "" || u.User != nil:
		return GitHubSource{}, nil, "must not have a query string"
	}
	segs := splitPath(u.Path)
	if len(segs) < 2 {
		return GitHubSource{}, nil, "needs an owner and a repository"
	}
	refPath, problem := linkRefPath(segs)
	if problem != "" {
		return GitHubSource{}, nil, problem
	}
	return GitHubSource{Repo: segs[0] + "/" + strings.TrimSuffix(segs[1], ".git")}, refPath, ""
}

// linkRefPath returns the segments after tree/ or blob/ of a link path. They
// hold the ref and the folder, which only the API can split.
func linkRefPath(segs []string) ([]string, string) {
	var refPath []string
	switch {
	case len(segs) == 2:
	case len(segs) >= 4 && segs[2] == "tree":
		refPath = segs[3:]
	case len(segs) >= 5 && segs[2] == "blob":
		refPath = segs[3 : len(segs)-1] // the folder of the file
	default:
		return nil, "must link to a repository or a folder in it"
	}
	if slices.Contains(refPath, "..") || slices.Contains(refPath, ".") {
		return nil, "must not have . or .. in the path"
	}
	return refPath, ""
}

// parseShortLink reads owner/repo[/path][@ref].
func parseShortLink(s string) (GitHubSource, string) {
	body, ref, _ := strings.Cut(s, "@")
	segs := splitPath(body)
	if len(segs) < 2 || strings.ContainsAny(s, "?#:") {
		return GitHubSource{}, "must be a github.com link or owner/repo[/folder][@ref]"
	}
	return GitHubSource{Repo: segs[0] + "/" + strings.TrimSuffix(segs[1], ".git"), Path: strings.Join(segs[2:], "/"), Ref: ref}, ""
}

// splitPath splits a slash path and drops the empty segments.
func splitPath(p string) []string {
	var out []string
	for _, seg := range strings.Split(p, "/") {
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

// cleanRepoPath reports whether p is a clean folder path in a repository.
func cleanRepoPath(p string) bool {
	return !path.IsAbs(p) && path.Clean(p) == p && p != "." && p != ".." && !strings.HasPrefix(p, "../")
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// sourceNotFound is ErrSourceNotFound with the text for the user.
func sourceNotFound(token bool) error {
	msg := "Trebi cannot open this link. The repository is private, or the branch or folder does not exist."
	if !token {
		msg += " Log in with the GitHub CLI on this computer to use a private repository."
	}
	return &SourceError{Kind: ErrSourceNotFound, Message: msg, Token: token}
}

func rateLimited(h http.Header) bool {
	return h.Get("X-Ratelimit-Remaining") == "0" || h.Get("Retry-After") != ""
}

// retryAfter reads Retry-After, else X-Ratelimit-Reset; the default is one
// minute.
func retryAfter(h http.Header, now time.Time) time.Duration {
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if reset, err := strconv.ParseInt(h.Get("X-Ratelimit-Reset"), 10, 64); err == nil {
		if d := time.Unix(reset, 0).Sub(now); d > 0 {
			return d
		}
	}
	return time.Minute
}

// escapeRef escapes each segment of a slash path.
func escapeRef(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// untarGz unpacks a tar.gz stream into dir. Only the entries under prefix
// go, without the prefix; a "*/" at the start of prefix matches any top
// folder. It refuses an absolute name, "..", and a symlink that leaves the
// folder, and skips devices and FIFOs. It returns the count of entries
// written.
func untarGz(r io.Reader, dir, prefix string, lim untarLimits) (int, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return 0, err
	}
	tr := tar.NewReader(zr)
	var total int64
	n := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		name, ok, err := entryName(hdr, prefix)
		if err != nil {
			return n, err
		}
		if !ok {
			continue
		}
		if hdr.Typeflag == tar.TypeReg {
			total += hdr.Size
			if lim.size > 0 && total > lim.size {
				return n, fmt.Errorf("%w: the folder is over %d MiB", ErrSourceTooLarge, lim.size>>20)
			}
		}
		wrote, err := extractEntry(tr, hdr, name, filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return n, err
		}
		if !wrote {
			continue
		}
		n++
		if lim.entries > 0 && n > lim.entries {
			return n, fmt.Errorf("%w: the folder has over %d files", ErrSourceTooLarge, lim.entries)
		}
	}
}

// extractEntry writes one folder, file, or link that stays inside. It
// reports false for a skipped type.
func extractEntry(tr *tar.Reader, hdr *tar.Header, name, target string) (bool, error) {
	switch hdr.Typeflag {
	case tar.TypeDir:
		return true, os.MkdirAll(target, 0o700)
	case tar.TypeReg:
		return true, writeEntry(tr, target, hdr)
	case tar.TypeSymlink:
		dest := path.Join(path.Dir(name), hdr.Linkname)
		if path.IsAbs(hdr.Linkname) || dest == ".." || strings.HasPrefix(dest, "../") {
			return false, fmt.Errorf("unsafe link %q -> %q", hdr.Name, hdr.Linkname)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return false, err
		}
		return true, os.Symlink(hdr.Linkname, target)
	}
	return false, nil // devices, FIFOs, hard links
}

// entryName returns the clean name of a tar entry under prefix. It reports
// false for an entry to skip, and refuses a name that leaves the folder.
func entryName(hdr *tar.Header, prefix string) (string, bool, error) {
	if hdr.Typeflag == tar.TypeXGlobalHeader {
		return "", false, nil
	}
	name, ok := stripPrefix(hdr.Name, prefix)
	if !ok {
		return "", false, nil
	}
	name = path.Clean(name)
	if name == "." {
		return "", false, nil
	}
	if path.IsAbs(hdr.Name) || name == ".." || strings.HasPrefix(name, "../") || slices.Contains(strings.Split(hdr.Name, "/"), "..") {
		return "", false, fmt.Errorf("unsafe path %q", hdr.Name)
	}
	return name, true, nil
}

// stripPrefix removes prefix from a tar name, or reports false.
func stripPrefix(name, prefix string) (string, bool) {
	if rest, ok := strings.CutPrefix(prefix, "*/"); ok {
		_, after, found := strings.Cut(name, "/")
		if !found {
			return "", false
		}
		name, prefix = after, rest
	}
	if prefix == "" {
		return name, true
	}
	if name+"/" == prefix {
		return ".", true
	}
	return strings.CutPrefix(name, prefix)
}

// writeEntry writes one regular file and keeps the exec bit.
func writeEntry(r io.Reader, target string, hdr *tar.Header) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if hdr.Mode&0o111 != 0 {
		mode = 0o700
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, io.LimitReader(r, hdr.Size)); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// treeDigest is sha256 over a sorted listing of the folder: the relative
// path, the mode (x for an executable, l for a link), and the sha256 of
// each file or link target. It does not depend on the archive bytes.
func treeDigest(dir string) (string, error) {
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		var mode string
		var data []byte
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			mode, data = "l", []byte(target)
		case info.Mode().IsRegular():
			mode = "-"
			if info.Mode()&0o111 != 0 {
				mode = "x"
			}
			if data, err = os.ReadFile(p); err != nil {
				return err
			}
		default:
			return nil
		}
		sum := sha256.Sum256(data)
		lines = append(lines, rel+"\x00"+mode+"\x00"+hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		return "", err
	}
	slices.Sort(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ghToken runs gh with a short timeout and a clean env. The LaunchAgent
// PATH is short, so the Homebrew folders are also searched.
func ghToken(ctx context.Context) string {
	pathEnv := strings.Join([]string{os.Getenv("PATH"), "/opt/homebrew/bin", "/usr/local/bin"}, string(os.PathListSeparator))
	gh := lookPath("gh", pathEnv)
	if gh == "" {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, gh, "auth", "token", "--hostname", "github.com")
	cmd.Env = []string{"HOME=" + os.Getenv("HOME"), "PATH=" + pathEnv}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
